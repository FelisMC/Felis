package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	apiTokenPrefix      = "felis_pat_"
	APITokensEnabledKey = "api_tokens_enabled"
	maxAPITokensPerUser = 10
	maxAPITokenName     = 64
	apiTokenMaxLife     = 365 * 24 * time.Hour
	adminTokenMaxLife   = 90 * 24 * time.Hour
	apiTokenTouchEvery  = time.Minute
	// apiTokenShown is how many characters after the prefix identify a key in lists and audits.
	apiTokenShown = 8
)

var ErrAPITokenLimit = errors.New("api token limit reached")

// APIToken never carries the key itself; Prefix is the start of the key shown to its owner.
type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Admin      bool       `json:"admin"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	UserID     string     `json:"-"`
}

// newAPIToken returns the key, its displayed prefix and the hash that is stored.
func newAPIToken() (key, prefix, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", "", err
	}
	key = apiTokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return key, key[:len(apiTokenPrefix)+apiTokenShown], hashAPIToken(key), nil
}

func hashAPIToken(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func newTokenID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// bearerAPIToken reports an API key carried in the Authorization header.
func bearerAPIToken(r *http.Request) (string, bool) {
	scheme, key, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || !strings.HasPrefix(key, apiTokenPrefix) {
		return "", false
	}
	return key, true
}

// keyOnlyRequest is a request authenticated by an API key alone, which a browser cannot forge across sites.
func keyOnlyRequest(r *http.Request) bool {
	_, ok := bearerAPIToken(r)
	return ok && r.Header.Get("Cookie") == ""
}

func apiTokensEnabled(ctx context.Context, repo Repo) (bool, error) {
	raw, err := repo.GetSetting(ctx, APITokensEnabledKey)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		return false, nil
	}
	return enabled, nil
}

func (s SessionAuth) authenticateAPIToken(r *http.Request, key string) (*Principal, error) {
	if _, err := r.Cookie(sessionCookieName); err == nil {
		return nil, fmt.Errorf("a request carries either a session or an API key")
	}
	ctx := r.Context()
	for _, check := range []func(context.Context, Repo) (bool, error){localAuthEnabledStatus, apiTokensEnabled} {
		on, err := check(ctx, s.Repo)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errAuthBackend, err)
		}
		if !on {
			return nil, fmt.Errorf("api keys are disabled")
		}
	}
	now := s.now()
	u, tok, err := s.Repo.APITokenUser(ctx, hashAPIToken(key), now)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, fmt.Errorf("invalid api key: %w", err)
	case err != nil:
		return nil, fmt.Errorf("%w: %v", errAuthBackend, err)
	}
	if tok.LastUsedAt == nil || now.Sub(*tok.LastUsedAt) >= apiTokenTouchEvery {
		if err := s.Repo.TouchAPIToken(ctx, tok.ID, now); err != nil {
			log.Printf("api: record api key use (request_id=%s): %v", requestIDFromContext(ctx), err)
		}
	}
	return &Principal{
		UserID:         u.ID,
		Username:       u.Username,
		Email:          u.Email,
		Role:           u.Role,
		ViaAdminAccess: tok.Admin && staffRole(u.Role) && hostIsAdminConsole(r, s.RootDomain, s.AdminHostname),
		EmailVerified:  u.EmailVerified,
		APIKey:         tok.Prefix,
	}, nil
}

// requireSessionCaller keeps key management in a browser session so a leaked key cannot mint or hide keys.
func requireSessionCaller(w http.ResponseWriter, r *http.Request, p *Principal) bool {
	if p == nil || !p.ViaSession {
		writeError(w, r, newError(http.StatusForbidden, "session_required",
			"API keys are managed from a signed-in session"))
		return false
	}
	return true
}

func (a *API) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !requireSessionCaller(w, r, p) {
		return
	}
	enabled, err := apiTokensEnabled(r.Context(), a.Repo)
	if err != nil {
		writeError(w, r, err)
		return
	}
	tokens, err := a.Repo.ListAPITokens(r.Context(), p.UserID, a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if tokens == nil {
		tokens = []APIToken{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": enabled, "limit": maxAPITokensPerUser, "can_create_admin": p.IsAdmin(), "tokens": tokens,
	})
}

func (a *API) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !requireSessionCaller(w, r, p) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body struct {
		Name      string     `json:"name"`
		ExpiresAt *time.Time `json:"expires_at"`
		Admin     bool       `json:"admin"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	enabled, err := apiTokensEnabled(r.Context(), a.Repo)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !enabled {
		writeError(w, r, newError(http.StatusForbidden, "api_tokens_disabled", "API keys are turned off on this platform"))
		return
	}
	now := a.now()
	name := strings.TrimSpace(body.Name)
	maxLife := apiTokenMaxLife
	if body.Admin {
		maxLife = adminTokenMaxLife
	}
	switch {
	case name == "" || utf8.RuneCountInString(name) > maxAPITokenName:
		writeError(w, r, newError(http.StatusBadRequest, "bad_api_token", "name must be 1 to %d characters", maxAPITokenName))
		return
	case strings.IndexFunc(name, func(c rune) bool { return c < 0x20 || c == 0x7f }) >= 0:
		writeError(w, r, newError(http.StatusBadRequest, "bad_api_token", "name must be a single line"))
		return
	case body.Admin && body.ExpiresAt == nil:
		writeError(w, r, newError(http.StatusBadRequest, "bad_api_token", "an admin key needs an expiry date"))
		return
	case body.ExpiresAt != nil && !body.ExpiresAt.After(now):
		writeError(w, r, newError(http.StatusBadRequest, "bad_api_token", "expires_at must be in the future"))
		return
	case body.ExpiresAt != nil && body.ExpiresAt.Sub(now) > maxLife:
		writeError(w, r, newError(http.StatusBadRequest, "bad_api_token", "a key lasts at most %d days", int(maxLife/(24*time.Hour))))
		return
	}
	if body.Admin {
		if !p.IsAdmin() {
			writeError(w, r, newError(http.StatusForbidden, "forbidden", "only staff on the operator console create admin keys"))
			return
		}
		if !a.requireReauth(w, r, p) {
			return
		}
	}
	key, prefix, hash, err := newAPIToken()
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := newTokenID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	tok := APIToken{ID: id, UserID: p.UserID, Name: name, Prefix: prefix, Admin: body.Admin}
	if body.ExpiresAt != nil {
		tok.ExpiresAt = ptrTime(body.ExpiresAt.UTC())
	}
	switch err := a.Repo.CreateAPIToken(r.Context(), &tok, hash, maxAPITokensPerUser, now); {
	case errors.Is(err, ErrAPITokenLimit):
		writeError(w, r, newError(http.StatusConflict, "api_token_limit", "an account has at most %d API keys", maxAPITokensPerUser))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	a.auditEntry(r, AuditEntry{Actor: auditActor(p), ActorUserID: p.UserID, Action: "account.api_token.create",
		Payload: auditPayload(map[string]any{"id": tok.ID, "prefix": tok.Prefix, "admin": tok.Admin})})
	writeJSON(w, http.StatusCreated, struct {
		APIToken
		Token string `json:"token"`
	}{tok, key})
}

func (a *API) handleRevokeAPIToken(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !requireSessionCaller(w, r, p) {
		return
	}
	id := r.PathValue("id")
	if err := a.Repo.RevokeAPIToken(r.Context(), p.UserID, id, a.now()); err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	a.auditEntry(r, AuditEntry{Actor: auditActor(p), ActorUserID: p.UserID, Action: "account.api_token.revoke",
		Payload: auditPayload(map[string]any{"id": id})})
	w.WriteHeader(http.StatusNoContent)
}

type apiTokenSettingsView struct {
	Enabled  bool   `json:"enabled"`
	Revision string `json:"revision"`
}

func (a *API) readAPITokenSettings(ctx context.Context) (apiTokenSettingsView, []byte, error) {
	raw, err := a.Repo.GetSetting(ctx, APITokensEnabledKey)
	switch {
	case errors.Is(err, ErrNotFound):
		raw = nil
	case err != nil:
		return apiTokenSettingsView{}, nil, err
	}
	enabled, err := apiTokensEnabled(ctx, a.Repo)
	if err != nil {
		return apiTokenSettingsView{}, nil, err
	}
	canonical, _ := json.Marshal(enabled)
	sum := sha256.Sum256(canonical)
	return apiTokenSettingsView{Enabled: enabled, Revision: hex.EncodeToString(sum[:])}, raw, nil
}

func (a *API) handleGetAPITokenSettings(w http.ResponseWriter, r *http.Request) {
	view, _, err := a.readAPITokenSettings(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleSetAPITokenSettings(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body apiTokenSettingsView
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	current, expected, err := a.readAPITokenSettings(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if body.Revision != current.Revision {
		writeError(w, r, newError(http.StatusConflict, "conflict", "the API key setting changed; reload before saving"))
		return
	}
	raw, _ := json.Marshal(body.Enabled)
	if err := a.Repo.CompareAndSetSetting(r.Context(), APITokensEnabledKey, expected, raw); err != nil {
		writeError(w, r, err)
		return
	}
	p := principalFromContext(r.Context())
	a.auditEntry(r, AuditEntry{Actor: auditActor(p), ActorUserID: p.UserID, Action: "platform.api_tokens",
		ServerName: "platform", Payload: auditPayload(map[string]any{"enabled": body.Enabled})})
	view, _, err := a.readAPITokenSettings(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
