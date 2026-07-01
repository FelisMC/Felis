package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// Passkey enrollment (spec §14 WebAuthn / Phase 6 bind). An already-authenticated
// principal binds a passkey to their account — the WebAuthn credential-creation
// ceremony — and manages the credentials they have bound. Email-OTP (handlers_email_otp.go)
// stays the fallback factor, so a player with no passkey is never locked out.
//
// Scope of the HANDLERS in this file: ENROLLMENT only. Every ceremony here rides on a
// known principal — the challenge is bound to the caller's user_id and the finish
// verifies against the server-stashed SessionData, never a client-echoed challenge. The
// login/assertion path (proving a passkey to mint or elevate a session from an
// UNauthenticated state) has its cryptographic half built and Oracle-verified in the
// adapter (internal/passkey BeginLogin/FinishLogin, against a virtual authenticator),
// and its persist-ready output shape is VerifiedAssertion below — but the login HTTP
// handlers, the session minting, and the panel.* passkey relying-party boundary/tier
// decision (see migration 0007) are a deferred slice: this file adds no unauthenticated
// login route.
//
// The cryptographic half is a seam (PasskeyVerifier) so this package never imports
// go-webauthn: ceremony state crosses the boundary as opaque bytes, the attestation
// as an io.Reader, and the verified result as a plain VerifiedCredential. Production
// wires the real go-webauthn verifier (cmd/felis); a nil verifier makes the begin and
// finish routes report 503 (the authenticated enrollment boundary is still exercised),
// and tests inject a fake so the state machine runs without real attestation crypto.

const (
	// passkeyChallengeTTL bounds how long a freshly minted credential-creation
	// challenge is accepted. The ceremony is interactive (a user taps an
	// authenticator), so a few minutes is ample; a shorter window shrinks the gap in
	// which a stashed challenge is live.
	passkeyChallengeTTL = 5 * time.Minute
	// passkeyPurposeRegister scopes a challenge to the enrollment (credential-creation)
	// flow. The purpose column exists so a later assertion/login flow can mint
	// challenges that never collide with an enrollment challenge for the same user.
	passkeyPurposeRegister = "passkey_register"
)

// PasskeyVerifier performs the cryptographic half of a WebAuthn credential-creation
// ceremony. It is a seam so the api package stays free of go-webauthn types: the real
// implementation (cmd/felis) wraps github.com/go-webauthn/webauthn, while tests inject
// a fake. All ceremony state crosses the seam as opaque bytes — the marshaled
// SessionData the server stashes between begin and finish — so the handler persists it
// without understanding it.
type PasskeyVerifier interface {
	// BeginRegistration starts a credential-creation ceremony for user. It returns the
	// publicKey creation options to hand to the browser's navigator.credentials.create()
	// AND the opaque sessionData the server must stash and replay at finish.
	// user.Credentials carries the passkeys already bound so the authenticator can be
	// told to exclude them (no double-binding one device).
	BeginRegistration(user PasskeyUser) (options json.RawMessage, sessionData []byte, err error)
	// FinishRegistration verifies the authenticator's attestation response against the
	// stashed sessionData and returns the credential to persist. attestation is the raw
	// navigator.credentials.create() result the browser posts back; sessionData is the
	// blob BeginRegistration returned. A failed verification returns a non-nil error;
	// the handler maps it to 400 (the ceremony state exists; the attestation is bad).
	FinishRegistration(user PasskeyUser, sessionData []byte, attestation io.Reader) (VerifiedCredential, error)
}

// PasskeyUser is the relying-party view of the enrolling principal the verifier needs:
// a stable user handle (ID), the names an authenticator shows the human, and the
// passkeys already bound (so the ceremony can exclude them). It is a plain value so the
// api package stays free of go-webauthn types; the real verifier adapts it to a
// webauthn.User.
type PasskeyUser struct {
	ID          string
	Name        string
	DisplayName string
	Credentials []PasskeyCredential
}

// VerifiedCredential is the public, persist-ready output of a finished registration
// ceremony — the material CreatePasskeyCredential stores. It carries no secret: a
// WebAuthn public key is public by design, so it is safe at rest.
type VerifiedCredential struct {
	CredentialID string // base64url(raw credential id)
	PublicKey    string // base64(COSE public key bytes)
	SignCount    uint32
	AAGUID       string
}

// VerifiedAssertion is the output of a finished LOGIN (assertion) ceremony: which of the
// user's bound credentials proved itself and the signature counter the authenticator
// reported. Like VerifiedCredential it carries no secret. SignCount is the raw ceremony
// fact, NOT a policy verdict: the handler that eventually consumes this holds the
// previously-stored counter and decides whether a non-increase is a cloned-authenticator
// signal — the verifier deliberately does not, so clone policy lives in one place with
// the stored state. SignCount is legitimately 0 for authenticators that keep no counter.
//
// The login handlers do not exist yet (see the file header): this is the stable seam
// output the production adapter (internal/passkey) already produces and its Oracle test
// already asserts on, so wiring the handlers later needs no reshaping here.
type VerifiedAssertion struct {
	CredentialID string // base64url(raw credential id) — which bound credential signed
	SignCount    uint32
}

// errPasskeyUnavailable is returned when the WebAuthn verifier is not configured on
// this api instance, so the begin/finish ceremony routes answer 503 rather than panic.
var errPasskeyUnavailable = newError(http.StatusServiceUnavailable, "passkey_unavailable",
	"passkey subsystem is not configured")

// newPasskeyID returns an opaque random row id (128 bits, hex) for a passkey row.
func newPasskeyID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// passkeyUserFor builds the relying-party view of a principal for the verifier. The
// human-facing names fall back to the user id when no email is bound yet (a player
// mid-onboarding), so the authenticator always shows a stable, non-empty label.
func passkeyUserFor(p *Principal, creds []PasskeyCredential) PasskeyUser {
	label := auditActor(p)
	return PasskeyUser{ID: p.UserID, Name: label, DisplayName: label, Credentials: creds}
}

// handlePasskeyRegisterBegin mints a credential-creation challenge for the caller
// (spec §14, external app face). It loads the passkeys the caller has already bound so
// the ceremony excludes them (one authenticator binds once), asks the verifier for the
// creation options + opaque SessionData, stashes the SessionData under a short TTL, and
// returns the options verbatim for navigator.credentials.create(). The challenge never
// leaves the server in a forgeable form — only the publicKey options the browser needs.
func (a *API) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if a.Passkey == nil {
		writeError(w, r, errPasskeyUnavailable)
		return
	}
	p := principalFromContext(r.Context())
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
	// The creation options are the WebAuthn {"publicKey": {...}} document the browser
	// passes straight to navigator.credentials.create(); return them verbatim.
	writeJSON(w, http.StatusOK, options)
}

// passkeyFinishRequest is the finish body: the human nickname for the new passkey and
// the raw navigator.credentials.create() attestation response. Attestation is captured
// as RawMessage so the handler hands the exact bytes the browser produced to the
// verifier without re-encoding (a re-marshal could perturb the signed payload).
type passkeyFinishRequest struct {
	Name        string          `json:"name"`
	Attestation json.RawMessage `json:"attestation"`
}

// handlePasskeyRegisterFinish verifies an attestation and binds the passkey (spec §14,
// external app face). It atomically consumes the caller's live challenge (a missing or
// expired one → 400, single-use), verifies the attestation against the stashed
// SessionData, and persists the public credential. A credential_id already bound to any
// account → 409 (the UNIQUE guard); the handler never silently rebinds an authenticator.
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
		ID:           id,
		UserID:       p.UserID,
		CredentialID: vc.CredentialID,
		PublicKey:    vc.PublicKey,
		SignCount:    vc.SignCount,
		AAGUID:       vc.AAGUID,
		Name:         req.Name,
		CreatedAt:    a.now(),
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
	a.audit(r, auditActor(p), "account.passkey.registered", "")
	writeJSON(w, http.StatusCreated, passkeyView(cred))
}

// passkeyCredentialView is the display projection of a bound passkey: never the public
// key (the client has no use for it), only what the credential-management UI renders.
type passkeyCredentialView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	AAGUID     string     `json:"aaguid,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// passkeyView maps a stored credential to its display projection.
func passkeyView(c PasskeyCredential) passkeyCredentialView {
	return passkeyCredentialView{
		ID:         c.ID,
		Name:       c.Name,
		AAGUID:     c.AAGUID,
		CreatedAt:  c.CreatedAt.UTC(),
		LastUsedAt: c.LastUsedAt,
	}
}

// handlePasskeyList returns the passkeys the caller has bound (spec §14, external app
// face), newest first, for the credential-management view. It reads only the
// principal's own credentials and returns display fields only (never a secret).
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

// handlePasskeyDelete unbinds one of the caller's passkeys (spec §14, external app
// face). The delete is scoped to the principal, so a caller can only remove their OWN
// credential; an unknown or cross-user id → 404 (it never silently no-ops as success).
func (a *API) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "credential id is required"))
		return
	}
	if err := a.Repo.DeletePasskeyCredential(r.Context(), p.UserID, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "no such passkey"))
			return
		}
		writeError(w, r, err)
		return
	}
	a.audit(r, auditActor(p), "account.passkey.removed", id)
	w.WriteHeader(http.StatusNoContent)
}
