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

// Passkey (spec §14 WebAuthn). Two slices live in this file: ENROLLMENT — an already-
// authenticated principal binds a passkey to their account (the WebAuthn credential-
// creation ceremony) and manages the credentials they have bound — and the public LOGIN
// (assertion) door, which resolves an account by email, proves one of its bound passkeys,
// and mints a session from an UNauthenticated state (handlePasskeyLoginBegin/Finish, near
// the end of this file). Email-OTP (handlers_email_otp.go) stays the fallback factor, so a
// player with no passkey is never locked out.
//
// Every ceremony rides on a challenge bound to a user_id whose finish verifies against the
// server-stashed SessionData, never a client-echoed challenge. The login door's
// cryptographic half is built and Oracle-verified in the adapter (internal/passkey
// BeginLogin/FinishLogin, against a virtual authenticator); its persist-ready output shape
// is VerifiedAssertion below. The login door's design checkpoint (task #36) resolved two
// questions that still frame it:
//
//   - RP boundary (RESOLVED): felis-api is the app-login relying party (panel.*); the
//     WebAuthn-as-security-gate lives at the Cloudflare Access EDGE, not here. Spec §14
//     ties WebAuthn/posture to admin.* (Access), while panel.* is plain app login with
//     no WebAuthn requirement — so this door is a login convenience, not a spec-required
//     backend step-up consumer (the role-switcher step-up UX is frontend).
//   - Identifier (RESOLVED by #69/#70): a from-zero login needs a unique, human-typable
//     handle to resolve the account before its passkeys can be offered. users.email was
//     nullable and NOT unique (0001_init.sql), and a player's users.username IS their
//     Minecraft uuid (pgrepo.go RedeemPlayerBindCode mints a uuid-derived unique username)
//     — opaque, never typed into a form. The verified-email uniqueness invariant
//     (0010_verified_email_unique.sql) plus UserByEmail gave the door the typable handle
//     it keys on: begin resolves email → account → its bound passkeys.
//
// That EMAIL-first assertion is one of TWO login doors this subsystem now offers. The other,
// the TRULY from-zero door, is discoverable ("usernameless") login (handlers_passkey_discoverable.go,
// task #40): the browser calls navigator.credentials.get() with an EMPTY allowCredentials, the
// authenticator offers a resident credential it holds, and the account is resolved from the
// userHandle inside the signed assertion — no identifier typed at all. It reshaped enrollment
// (ResidentKey=Preferred in the verifier) and added a non-user-keyed challenge store (migration
// 0013). Two honest limits frame it: (1) the from-zero door partly bypasses the returning-player
// root of trust — control of the in-game identity, which handlers_onboard.go re-mints a session
// through even after passkey/OTP are bound — but it stands on the same footing as the email door
// (#72): a passkey is a possession+UV two-factor authenticator strong enough to stand alone; and
// (2) whether an authenticator actually STORES a resident key is a device property no server
// request compels, so a credential enrolled before this slice, or on hardware that declines
// residency, stays username-first (BeginLogin) — the from-zero door is inert for it until its
// owner enrolls a new passkey. The assertion crypto for both doors is Oracle-verified.
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
	// BeginLogin starts an assertion (login) ceremony for a known user. It returns the
	// {"publicKey": {...}} request options for navigator.credentials.get() and the
	// opaque SessionData the handler stashes and replays at finish. user.Credentials
	// carries the passkeys already bound so the authenticator can be told which to
	// offer. A user with no bound credential yields an error (nothing to assert); the
	// handler treats that as "offer the email-OTP fallback instead", never a server
	// fault.
	BeginLogin(user PasskeyUser) (options json.RawMessage, sessionData []byte, err error)
	// FinishLogin verifies the browser's assertion against the stashed SessionData and
	// reports which of the user's credentials signed and the signature counter the
	// authenticator reported. assertion is the raw navigator.credentials.get() result
	// the browser posts back; sessionData is the blob BeginLogin returned. A failed
	// verification returns a non-nil error; the handler maps it to 400.
	FinishLogin(user PasskeyUser, sessionData []byte, assertion io.Reader) (VerifiedAssertion, error)
	// BeginDiscoverableLogin starts a USERNAMELESS assertion ceremony (task #40): there is no
	// user yet, so no allowCredentials — the authenticator offers a resident (discoverable)
	// credential it holds for this RP and reveals the account only in the signed response. It
	// returns the {"publicKey": {...}} request options for navigator.credentials.get() and the
	// opaque SessionData the handler stashes under an opaque handle (not a user id) and replays
	// at finish.
	BeginDiscoverableLogin() (options json.RawMessage, sessionData []byte, err error)
	// FinishDiscoverableLogin verifies a usernameless assertion. resolveUser is called with the
	// authenticator-revealed user handle so the caller loads the account and its bound
	// credentials WITHOUT any client-supplied identifier; the verifier then checks the asserted
	// credential id is one that user holds and verifies the signature. A resolveUser error
	// (unknown handle) fails the ceremony closed; the handle is the account's stable user id, so
	// resolveUser is a direct id lookup. A failed verification returns a non-nil error the
	// handler maps to 400.
	FinishDiscoverableLogin(resolveUser func(userHandle []byte) (PasskeyUser, error), sessionData []byte, assertion io.Reader) (VerifiedAssertion, error)
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
	// Ceremony flags captured at enrollment. UserVerified records that a PIN/biometric
	// (not mere presence) was performed; BackupEligible/BackupState record whether the
	// credential is syncable/backed up. All are non-secret ceremony facts a future login
	// path can enforce or surface per credential.
	UserVerified   bool
	BackupEligible bool
	BackupState    bool
}

// VerifiedAssertion is the output of a finished LOGIN (assertion) ceremony: which of the
// user's bound credentials proved itself, the signature counter the authenticator reported,
// and whether that counter regressed (a possible clone). Like VerifiedCredential it carries
// no secret. SignCount is a raw ceremony fact, NOT a policy verdict; CloneWarning IS the
// verifier's regression verdict, but the refuse-vs-allow decision is the handler's. Clone
// policy therefore lives in one place with the stored counter (applyAssertion, which
// every assertion door calls). SignCount is legitimately 0 for authenticators that keep no counter.
//
// applyAssertion is that single consumer: it refuses an unverified user or a CloneWarning
// fail-closed and, on success, advances the stored counter and stamps last_used_at. This is the stable seam
// output the production adapter (internal/passkey) produces and its Oracle test asserts on,
// so handler and adapter agree on shape without either reshaping the other.
type VerifiedAssertion struct {
	CredentialID string // base64url(raw credential id) — which bound credential signed
	SignCount    uint32
	// CloneWarning is go-webauthn's verdict that the signature counter did not advance past
	// the stored value (WebAuthn §6.1.1 clone detection). It is meaningful only for
	// counter-keeping authenticators: synced/counter-less keys report SignCount 0 on every
	// assertion and structurally never raise it. The login handlers refuse it fail-closed.
	CloneWarning bool
	// UserVerified records that a PIN/biometric (not mere presence) was performed
	// during this assertion. The verifier asks for UV=required today, so a successful
	// assertion carries it; applyAssertion checks it anyway, together with the
	// credential's stored bind-time flag, so a later UV=preferred policy or a door that
	// asks for less cannot let a presence-only assertion through.
	UserVerified bool
}

// errPasskeyUnavailable is returned when the WebAuthn verifier is not configured on
// this api instance, so the begin/finish ceremony routes answer 503 rather than panic.
var errPasskeyUnavailable = newError(http.StatusServiceUnavailable, "passkey_unavailable",
	"passkey subsystem is not configured")

// errPasskeyClonedAuthenticator is the internal signal from applyAssertion that a
// verified assertion carried a clone warning (its signature counter did not advance past the
// stored value). It never reaches the client verbatim: the login doors map it to the generic
// passkey_login_invalid envelope — no clone oracle to a prober — and audit it distinctly.
var errPasskeyClonedAuthenticator = errors.New("passkey assertion rejected: clone warning")

// errPasskeyUserNotVerified is the internal signal from applyAssertion that the assertion,
// or the credential it came from, did not verify the user. Like the clone signal it is
// answered with the opaque envelope and audited distinctly.
var errPasskeyUserNotVerified = errors.New("passkey assertion rejected: user not verified")

// applyAssertion is the single consumer of a verified assertion, shared by the
// username-first and discoverable login doors and the step-up confirmation, so assertion
// policy lives in one place with the stored credential. creds are the account's bound
// passkeys the verifier checked the assertion against.
//
// Every one of those doors grants a session or confirms one, so each needs user
// verification, per credential (migration 0009): the credential must have verified the
// user when it was bound, and this assertion must have verified the user now. Either
// missing fails closed with errPasskeyUserNotVerified. A credential bound presence-only
// (a pre-0009 row, or a future UV=preferred enrollment) therefore cannot sign in; its
// owner still has the email-code door.
//
// A CloneWarning fails closed with errPasskeyClonedAuthenticator; otherwise it advances
// the stored counter to the asserted value and stamps last_used_at. Counter-less/synced
// authenticators report 0 and never warn, so they pass through and simply re-stamp 0 — the
// check gates only counter-keeping authenticators, where a rollback is the meaningful clone
// signal. It runs BEFORE the session is minted, so a refusal or a persist failure denies
// the login rather than leaving an advanced counter with no session.
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

// unwrapPublicKey strips go-webauthn's {"publicKey": {...}} envelope so the register and
// username-login begin handlers return the FLAT options the panel reads (options.challenge,
// options.user.id, options.allowCredentials) rather than options.publicKey.challenge — the
// envelope is what made the panel crash on base64urlToBytes(undefined). Discoverable login
// keeps the envelope (it reads options.publicKey.*), so it does not call this. A body with
// no publicKey member is returned unchanged.
func unwrapPublicKey(options json.RawMessage) json.RawMessage {
	var env struct {
		PublicKey json.RawMessage `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &env); err != nil || len(env.PublicKey) == 0 {
		return options
	}
	return env.PublicKey
}

// optionsChallenge returns the challenge in go-webauthn's request options
// ({"publicKey": {"challenge": ...}}) in canonical form: the value the browser will
// sign into the assertion's clientDataJSON.
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

// assertionChallenge returns, in canonical form, the challenge a
// navigator.credentials.get() result signed: response.clientDataJSON is base64url
// JSON whose challenge member is that value. It only picks the ceremony to verify
// against; the verifier checks the signature over the same bytes.
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

// canonicalChallenge decodes a base64url challenge, padded or not (go-webauthn
// accepts both), and re-encodes it unpadded, so the stored and the signed forms
// compare equal. An empty or undecodable challenge is an error.
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
	// Gate the begin: the finish only consumes the challenge minted here.
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
	// go-webauthn wraps the creation options as {"publicKey": {...}}; the panel's register
	// flow reads them flat (options.challenge, options.user.id), so strip the envelope.
	writeJSON(w, http.StatusOK, unwrapPublicKey(options))
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
	// The session just showed an authenticator now bound to the account, the
	// same strength as a passkey reauth, so the next guarded step of a first-time
	// setup (verifying an email) runs without asking again.
	a.markReauthQuietly(r)
	a.notifyPasskeyAdded(r, p)
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
// The last passkey of an account without a verified email → 409 last_passkey: it is
// that account's only durable way in (ErrLastPasskey).
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

// ---- passkey login (assertion) ----

// passkeyPurposeLogin scopes a challenge to the login (assertion) flow, keeping it
// from ever colliding with an enrollment challenge (passkeyPurposeRegister) for the
// same user. The challenge store is queried per (user, purpose), so the two flows
// are fully independent even for one account with both a live enrollment and a live
// login challenge.
const passkeyPurposeLogin = "passkey_login"

// passkeyLoginBeginRequest is the begin body: the email that resolves the account
// before its passkeys can be offered. There is no principal yet (this is a
// pre-session route), so the email is the identifier — the same role the typed
// email plays in the email-OTP and op-login doors.
type passkeyLoginBeginRequest struct {
	Email string `json:"email"`
}

// handlePasskeyLoginBegin starts a passkey assertion ceremony for a returning user
// (Public, pre-session). It resolves the typed email to an account, loads the
// passkeys that account has bound, and asks the verifier for the assertion options
// + opaque SessionData the browser needs for navigator.credentials.get(). The
// SessionData is stashed under a short TTL beside the account's other live login
// ceremonies, tagged with the challenge the browser will sign, so finish consumes
// exactly the ceremony it answers: anyone who knows the address can begin a login for
// it, and a begin never cancels the owner's. The store holds each network to
// maxLiveChallengesPerSource live login challenges (429 too_many_challenges past it).
// Requires local sessions to be enabled (like the other pre-session doors). A user
// with no bound passkey, an unknown email, and a real account with passkeys are
// distinguished by status code (400 vs 200) — this is an accepted enumeration
// trade-off (the /auth/options oracle is the sanctioned place to learn existence);
// volume per caller is bounded by the auth-door bucket.
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
	// go-webauthn wraps the assertion options as {"publicKey": {...}}; the panel's
	// username-login flow reads them flat (options.challenge, options.allowCredentials),
	// so strip the envelope. (Discoverable login keeps the envelope — see its handler.)
	writeJSON(w, http.StatusOK, unwrapPublicKey(options))
}

// passkeyLoginFinishRequest is the finish body: the email (to resolve the account,
// as in the begin step) and the raw navigator.credentials.get() assertion response.
// Attestation is captured as RawMessage so the handler hands the exact bytes the
// browser produced to the verifier without re-encoding.
type passkeyLoginFinishRequest struct {
	Email     string          `json:"email"`
	Assertion json.RawMessage `json:"assertion"`
}

// handlePasskeyLoginFinish verifies a passkey assertion and mints a session (Public,
// pre-session). It resolves the email to the account, atomically consumes the
// stashed login challenge the assertion signed (clientDataJSON names it; a missing,
// expired, or unnamed one → 400), verifies the assertion
// against the SessionData, and mints a felis_session. Both players and staff may
// log in this way — the passkey is a two-factor authenticator (possession +
// biometric/PIN), strong enough to stand alone without the in-game approval the
// op-login flow requires. The session cookie is host-only, so a session minted on
// console.<root_domain> cannot reach op.console, and ViaAdminAccess is host-checked
// so admin operations are gated regardless.
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
	// UV + clone policy + counter advance, in one place shared with the discoverable door. A
	// refusal is answered with the same opaque envelope (no oracle) but audited distinctly; a
	// successful assertion advances the stored counter and stamps last_used_at.
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
