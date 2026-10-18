package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Passkey (WebAuthn) enrollment and login handlers.

const (
	passkeyChallengeTTL    = 5 * time.Minute    // credential-creation challenge lifetime
	passkeyPurposeRegister = "passkey_register" // challenges are keyed per (user, purpose) so enrollment and login never collide
)

// PasskeyVerifier performs the WebAuthn ceremony crypto; session data crosses it as opaque bytes.
type PasskeyVerifier interface {
	// BeginRegistration returns the browser creation options and the session data to replay at finish.
	BeginRegistration(user PasskeyUser) (options json.RawMessage, sessionData []byte, err error)
	FinishRegistration(user PasskeyUser, sessionData []byte, attestation io.Reader) (VerifiedCredential, error)
	// BeginLogin errors when user has no bound credential.
	BeginLogin(user PasskeyUser) (options json.RawMessage, sessionData []byte, err error)
	FinishLogin(user PasskeyUser, sessionData []byte, assertion io.Reader) (VerifiedAssertion, error)
	// BeginDiscoverableLogin sends no allowCredentials; the account is revealed only in the signed response.
	BeginDiscoverableLogin() (options json.RawMessage, sessionData []byte, err error)
	// FinishDiscoverableLogin calls resolveUser with the asserted user handle; a resolveUser error fails the ceremony.
	FinishDiscoverableLogin(resolveUser func(userHandle []byte) (PasskeyUser, error), sessionData []byte, assertion io.Reader) (VerifiedAssertion, error)
}

type PasskeyUser struct {
	ID          string
	Name        string
	DisplayName string
	Credentials []PasskeyCredential
}

type VerifiedCredential struct {
	CredentialID   string // base64url(raw credential id)
	PublicKey      string // base64(COSE public key bytes)
	SignCount      uint32
	AAGUID         string
	UserVerified   bool
	BackupEligible bool
	BackupState    bool
}

type VerifiedAssertion struct {
	CredentialID string // base64url(raw credential id)
	SignCount    uint32 // 0 for authenticators without a counter
	CloneWarning bool   // counter did not advance past the stored value; never set for counter-less authenticators
	UserVerified bool   // PIN/biometric performed, not mere presence
}

var errPasskeyUnavailable = newError(http.StatusServiceUnavailable, "passkey_unavailable",
	"passkey subsystem is not configured")

// errPasskeyClonedAuthenticator and errPasskeyUserNotVerified are internal: login doors answer with the
// generic passkey_login_invalid so probers learn nothing, and audit them distinctly.
var errPasskeyClonedAuthenticator = errors.New("passkey assertion rejected: clone warning")

var errPasskeyUserNotVerified = errors.New("passkey assertion rejected: user not verified")

// Before minting a session, require verified credential/assertion state, reject clones, and advance the counter.
func (a *API) applyAssertion(ctx context.Context, va VerifiedAssertion, creds []PasskeyCredential) error {
	bound := slices.IndexFunc(creds, func(c PasskeyCredential) bool { return c.CredentialID == va.CredentialID })
	if !va.UserVerified || bound < 0 || !creds[bound].UserVerified {
		return errPasskeyUserNotVerified
	}
	if va.CloneWarning {
		return errPasskeyClonedAuthenticator
	}
	return a.Repo.AdvanceCredentialSignCount(ctx, va.CredentialID, va.SignCount, a.now())
}

func newPasskeyID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func passkeyUserFor(p *Principal, creds []PasskeyCredential) PasskeyUser {
	label := auditActor(p)
	return PasskeyUser{ID: p.UserID, Name: label, DisplayName: label, Credentials: creds}
}

// unwrapPublicKey strips the {"publicKey": {...}} envelope; the panel reads register and username-login
// options flat. Discoverable login keeps the envelope.
func unwrapPublicKey(options json.RawMessage) json.RawMessage {
	var env struct {
		PublicKey json.RawMessage `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &env); err != nil || len(env.PublicKey) == 0 {
		return options
	}
	return env.PublicKey
}

func optionsChallenge(options json.RawMessage) (string, error) {
	var env struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &env); err != nil {
		return "", err
	}
	return canonicalChallenge(env.PublicKey.Challenge)
}

// assertionChallenge reads the challenge from response.clientDataJSON; it only selects the ceremony,
// the verifier checks the signature.
func assertionChallenge(assertion json.RawMessage) (string, error) {
	var body struct {
		Response struct {
			ClientDataJSON string `json:"clientDataJSON"`
		} `json:"response"`
	}
	if err := json.Unmarshal(assertion, &body); err != nil {
		return "", err
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(body.Response.ClientDataJSON, "="))
	if err != nil {
		return "", err
	}
	var clientData struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(raw, &clientData); err != nil {
		return "", err
	}
	return canonicalChallenge(clientData.Challenge)
}

// canonicalChallenge re-encodes a padded or unpadded base64url challenge as unpadded so stored and signed forms compare equal.
func canonicalChallenge(s string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", errors.New("empty challenge")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (a *API) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if a.Passkey == nil {
		writeError(w, r, errPasskeyUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	// Gate begin: finish only consumes the challenge minted here.
	if !a.requireReauth(w, r, p) {
		return
	}
	creds, err := a.Repo.PasskeyCredentialsForUser(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	options, sessionData, err := a.Passkey.BeginRegistration(passkeyUserFor(p, creds))
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := newPasskeyID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	expiresAt := a.now().Add(passkeyChallengeTTL)
	if err := a.Repo.CreatePasskeyChallenge(r.Context(), id, p.UserID, passkeyPurposeRegister, sessionData, expiresAt); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, unwrapPublicKey(options))
}

type passkeyFinishRequest struct {
	Name        string          `json:"name"`
	Attestation json.RawMessage `json:"attestation"` // raw bytes; re-marshaling could alter the signed payload
}

func (a *API) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if a.Passkey == nil {
		writeError(w, r, errPasskeyUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	var req passkeyFinishRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if len(req.Attestation) == 0 {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "attestation is required"))
		return
	}
	name := strings.TrimSpace(req.Name)
	if len(name) > 100 {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "passkey name must be at most 100 characters"))
		return
	}
	sessionData, err := a.Repo.ConsumePasskeyChallengeByUser(r.Context(), p.UserID, passkeyPurposeRegister, a.now())
	if err != nil {
		if errors.Is(err, ErrPasskeyChallengeInvalid) {
			writeError(w, r, newError(http.StatusBadRequest, "passkey_challenge_invalid",
				"no live passkey registration in progress; begin again"))
			return
		}
		writeError(w, r, err)
		return
	}
	vc, err := a.Passkey.FinishRegistration(passkeyUserFor(p, nil), sessionData, bytes.NewReader(req.Attestation))
	if err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "invalid_attestation",
			"passkey attestation could not be verified"))
		return
	}
	id, err := newPasskeyID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	cred := PasskeyCredential{
		ID:             id,
		UserID:         p.UserID,
		CredentialID:   vc.CredentialID,
		PublicKey:      vc.PublicKey,
		SignCount:      vc.SignCount,
		AAGUID:         vc.AAGUID,
		Name:           name,
		CreatedAt:      a.now(),
		UserVerified:   vc.UserVerified,
		BackupEligible: vc.BackupEligible,
		BackupState:    vc.BackupState,
	}
	if err := a.Repo.CreatePasskeyCredential(r.Context(), cred); err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "passkey_already_bound",
				"this passkey is already bound to an account"))
			return
		}
		writeError(w, r, err)
		return
	}
	a.audit(r, "account.passkey.registered", cred.ID)
	// Registering proves the same strength as a passkey reauth.
	a.markReauthQuietly(r)
	a.notifyPasskeyAdded(r, p)
	writeJSON(w, http.StatusCreated, passkeyView(cred))
}

// passkeyCredentialView is the display projection of a bound passkey; the public key is never returned.
type passkeyCredentialView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	AAGUID     string     `json:"aaguid,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func passkeyView(c PasskeyCredential) passkeyCredentialView {
	return passkeyCredentialView{
		ID:         c.ID,
		Name:       c.Name,
		AAGUID:     c.AAGUID,
		CreatedAt:  c.CreatedAt.UTC(),
		LastUsedAt: c.LastUsedAt,
	}
}

func (a *API) handlePasskeyList(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	creds, err := a.Repo.PasskeyCredentialsForUser(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	views := make([]passkeyCredentialView, 0, len(creds))
	for _, c := range creds {
		views = append(views, passkeyView(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": views})
}

func (a *API) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "credential id is required"))
		return
	}
	if !a.requireReauth(w, r, p) {
		return
	}
	if err := a.Repo.DeletePasskeyCredential(r.Context(), p.UserID, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "no such passkey"))
			return
		}
		if errors.Is(err, ErrLastPasskey) {
			writeError(w, r, newError(http.StatusConflict, "last_passkey",
				"this is your only passkey and your email is not verified; add another passkey or verify an email first"))
			return
		}
		writeError(w, r, err)
		return
	}
	a.audit(r, "account.passkey.removed", id)
	a.revokeOtherSessionsAfter(r, "passkey removal")
	a.notifyPasskeyRemoved(r, p)
	w.WriteHeader(http.StatusNoContent)
}

const passkeyPurposeLogin = "passkey_login"

type passkeyLoginBeginRequest struct {
	Email string `json:"email"`
}

// handlePasskeyLoginBegin keeps each account's live login challenges side by side, so a stranger's begin
// never cancels the owner's; finish consumes the one the assertion signed.
func (a *API) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if a.Passkey == nil {
		writeError(w, r, errPasskeyUnavailable)
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req passkeyLoginBeginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	email := strings.TrimSpace(req.Email)
	if !looksLikeEmail(email) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "a valid email is required"))
		return
	}

	u, err := a.Repo.UserByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusBadRequest, "no_passkey",
				"no passkey enrolled for this account; use email or operator login"))
			return
		}
		writeError(w, r, err)
		return
	}

	creds, err := a.Repo.PasskeyCredentialsForUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if len(creds) == 0 {
		writeError(w, r, newError(http.StatusBadRequest, "no_passkey",
			"no passkey enrolled for this account; use email or operator login"))
		return
	}

	user := PasskeyUser{
		ID:          u.ID,
		Name:        email,
		DisplayName: u.Username,
		Credentials: creds,
	}
	options, sessionData, err := a.Passkey.BeginLogin(user)
	if err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "passkey_login_failed",
			"could not start passkey login"))
		return
	}
	challenge, err := optionsChallenge(options)
	if err != nil {
		writeError(w, r, fmt.Errorf("passkey login options carry no challenge: %w", err))
		return
	}
	id, err := newPasskeyID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	now := a.now()
	if err := a.Repo.AddPasskeyLoginChallenge(r.Context(), id, u.ID, passkeyPurposeLogin,
		challengeSource(a.clientIP(r)), challenge, sessionData, now, now.Add(passkeyChallengeTTL)); err != nil {
		if errors.Is(err, ErrTooManyPasskeyChallenges) {
			writeError(w, r, errTooManyChallenges)
			return
		}
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, unwrapPublicKey(options))
}

type passkeyLoginFinishRequest struct {
	Email     string          `json:"email"`
	Assertion json.RawMessage `json:"assertion"` // raw bytes; re-marshaling could alter the signed payload
}

func (a *API) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if a.Passkey == nil {
		writeError(w, r, errPasskeyUnavailable)
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req passkeyLoginFinishRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	email := strings.TrimSpace(req.Email)
	if !looksLikeEmail(email) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "a valid email is required"))
		return
	}
	if len(req.Assertion) == 0 {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "assertion is required"))
		return
	}

	u, err := a.Repo.UserByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			a.authFailure(r, "passkey", "no_account", nil)
			writeError(w, r, newError(http.StatusBadRequest, "passkey_login_invalid",
				"passkey login could not be completed; begin again"))
			return
		}
		writeError(w, r, err)
		return
	}

	challenge, err := assertionChallenge(req.Assertion)
	if err != nil {
		a.authFailure(r, "passkey", "bad_assertion", u)
		writeError(w, r, newError(http.StatusBadRequest, "passkey_login_invalid",
			"passkey login could not be completed; begin again"))
		return
	}
	sessionData, err := a.Repo.ConsumePasskeyLoginChallenge(r.Context(), u.ID, passkeyPurposeLogin, challenge, a.now())
	if err != nil {
		if errors.Is(err, ErrPasskeyChallengeInvalid) {
			a.authFailure(r, "passkey", "challenge_invalid", u)
			writeError(w, r, newError(http.StatusBadRequest, "passkey_login_invalid",
				"passkey login could not be completed; begin again"))
			return
		}
		writeError(w, r, err)
		return
	}

	creds, err := a.Repo.PasskeyCredentialsForUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	user := PasskeyUser{
		ID:          u.ID,
		Name:        email,
		DisplayName: u.Username,
		Credentials: creds,
	}
	va, err := a.Passkey.FinishLogin(user, sessionData, bytes.NewReader(req.Assertion))
	if err != nil {
		a.authFailure(r, "passkey", "bad_assertion", u)
		writeError(w, r, newError(http.StatusBadRequest, "passkey_login_invalid",
			"passkey login could not be completed; begin again"))
		return
	}
	// Refusals get the same opaque envelope as other failures but are audited distinctly.
	if err := a.applyAssertion(r.Context(), va, creds); err != nil {
		if a.passkeyAssertionRejected(r, "passkey", u, va.CredentialID, err) {
			writeError(w, r, newError(http.StatusBadRequest, "passkey_login_invalid",
				"passkey login could not be completed; begin again"))
			return
		}
		writeError(w, r, err)
		return
	}

	if err := a.startSession(w, r, u.ID, provenSignIn); err != nil {
		writeError(w, r, err)
		return
	}
	a.auditAccount(r, u, "auth.passkey_login", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": u.ID,
		"role":    u.Role,
	})
}
