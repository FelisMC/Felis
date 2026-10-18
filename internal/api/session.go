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
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

const (
	// sessionCookieName is host-only to isolate operator and player sessions.
	sessionCookieName = "felis_session"
	sessionTTL        = 12 * time.Hour
	// staffSessionIdle counts all authenticated requests, including background refreshes.
	staffSessionIdle = 30 * time.Minute
	// sessionTouchEvery bounds last_seen_at write frequency and idle-expiry precision.
	sessionTouchEvery   = time.Minute
	maxSessionUserAgent = 256
)

// LocalAuthEnabledKey is read per request; changing the setting requires no pod restart.
const LocalAuthEnabledKey = "local_auth_enabled"

// newSessionToken returns a 256-bit cookie value; only its hash is persisted.
func newSessionToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func hashCookie(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// signInProof determines whether a new session also receives a fresh reauth window.
type signInProof bool

const (
	provenSignIn signInProof = true
	// bindCodeSignIn proves the game identity without unlocking changes to account factors.
	bindCodeSignIn signInProof = false
)

func (a *API) startSession(w http.ResponseWriter, r *http.Request, userID string, proof signInProof) error {
	token, s, err := a.mintSession(r, userID, proof)
	if err != nil {
		return err
	}
	if err := a.Repo.CreateSession(r.Context(), s); err != nil {
		return err
	}
	setSessionCookie(w, token, s.ExpiresAt)
	return nil
}

// mintSession returns the cookie value and an unpersisted session row.
func (a *API) mintSession(r *http.Request, userID string, proof signInProof) (string, NewSession, error) {
	token, err := newSessionToken()
	if err != nil {
		return "", NewSession{}, err
	}
	now := a.now()
	expires := now.Add(sessionTTL)
	ip := ""
	if addr := a.clientIP(r); addr.IsValid() {
		ip = addr.String()
	}
	var reauth time.Time
	if proof == provenSignIn {
		reauth = now
	}
	return token, NewSession{
		TokenHash: hashCookie(token),
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: expires,
		UserAgent: truncateUTF8(r.UserAgent(), maxSessionUserAgent),
		ClientIP:  ip,
		ReauthAt:  reauth,
	}, nil
}

func currentSessionHash(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return ""
	}
	return hashCookie(c.Value)
}

func setSessionCookie(w http.ResponseWriter, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie must match setSessionCookie's attributes to overwrite the cookie.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// hostIsAdminConsole accepts IP hosts only when explicitly configured or encoded in the wildcard-DNS root domain.
func hostIsAdminConsole(r *http.Request, rootDomain, adminHostname string) bool {
	want := strings.TrimSpace(adminHostname)
	if want == "" {
		if rootDomain == "" {
			return false
		}
		want = "op.console." + rootDomain
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		ip = ip.Unmap()
		if named, err := netip.ParseAddr(strings.Trim(want, "[]")); err == nil && named.Unmap() == ip {
			return true
		}
		embedded, ok := rootDomainIP(rootDomain)
		return ok && embedded == ip
	}
	return strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(want, "."))
}

// rootDomainIP extracts the address from a nip.io or sslip.io root domain.
func rootDomainIP(rootDomain string) (netip.Addr, bool) {
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(rootDomain), "."))
	for _, suffix := range []string{".nip.io", ".sslip.io"} {
		if base, ok := strings.CutSuffix(domain, suffix); ok {
			if ip, err := netip.ParseAddr(base); err == nil {
				return ip.Unmap(), true
			}
		}
	}
	return netip.Addr{}, false
}

// SessionAuth requires an enabled local_auth_enabled setting and a cookie hash resolving to a live user.
type SessionAuth struct {
	Repo          Repo
	RootDomain    string
	AdminHostname string
	Now           func() time.Time
}

func (s SessionAuth) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s SessionAuth) Authenticate(r *http.Request) (*Principal, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, fmt.Errorf("no session")
	}

	ctx := r.Context()
	enabled, err := localAuthEnabledStatus(ctx, s.Repo)
	if err != nil {
		// Store outages return 503 so they do not appear as invalid credentials.
		return nil, fmt.Errorf("%w: %v", errAuthBackend, err)
	}
	if !enabled {
		return nil, fmt.Errorf("local auth disabled")
	}

	hash, now := hashCookie(cookie.Value), s.now()
	u, err := s.Repo.SessionUser(ctx, hash, now)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, fmt.Errorf("invalid session: %w", err)
	case err != nil:
		return nil, fmt.Errorf("%w: %v", errAuthBackend, err)
	}
	if now.Sub(u.LastSeenAt) >= sessionTouchEvery {
		// A failed touch can cause an early idle sign-out but does not reject this request.
		if err := s.Repo.TouchSession(ctx, hash, now); err != nil {
			log.Printf("api: record session activity (request_id=%s): %v", requestIDFromContext(ctx), err)
		}
	}
	return &Principal{
		UserID:         u.ID,
		Username:       u.Username,
		Email:          u.Email,
		Role:           u.Role,
		ViaAdminAccess: staffRole(u.Role) && hostIsAdminConsole(r, s.RootDomain, s.AdminHostname),
		EmailVerified:  u.EmailVerified,
		ViaSession:     true,
		ReauthAt:       u.ReauthAt,
	}, nil
}

// errAuthBackend maps store failures to 503 instead of 401.
var errAuthBackend = errors.New("auth backend unavailable")

// localAuthEnabled fails closed on missing, invalid or unreadable settings.
func localAuthEnabled(ctx context.Context, repo Repo) bool {
	enabled, _ := localAuthEnabledStatus(ctx, repo)
	return enabled
}

// localAuthEnabledStatus distinguishes a missing or invalid setting from a store outage.
func localAuthEnabledStatus(ctx context.Context, repo Repo) (bool, error) {
	raw, err := repo.GetSetting(ctx, LocalAuthEnabledKey)
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

var _ ExternalAuth = SessionAuth{}
