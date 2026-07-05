// Package passkey wraps github.com/go-webauthn/webauthn behind the api.PasskeyVerifier
// seam. The api package deliberately never imports go-webauthn — ceremony state crosses
// the boundary as opaque bytes, the attestation as an io.Reader, and the verified result
// as a plain api.VerifiedCredential — so the real relying-party crypto lives here and is
// wired in only at the composition root (cmd/felis). The dependency arrow points one way:
// this package imports api for the seam types; api never imports this package, which is
// what keeps the seam (and the api test suite's fake verifier) honest.
//
// Scope: the full WebAuthn ceremony crypto across three halves, each Oracle-verified in
// verifier_test.go against a virtual authenticator:
//
//   - enrollment (BeginRegistration/FinishRegistration over go-webauthn's
//     BeginRegistration/CreateCredential),
//   - username-first login (BeginLogin/FinishLogin over BeginLogin/ValidateLogin), where the
//     account is known and its bound credentials scope allowCredentials, and
//   - discoverable, "usernameless" login (BeginDiscoverableLogin/FinishDiscoverableLogin over
//     BeginDiscoverableLogin/ValidateDiscoverableLogin), where the account is unknown at begin
//     and revealed only by the userHandle inside the signed assertion (task #40).
//
// All three are in the api.PasskeyVerifier interface and consumed by handlers today (enrollment
// + login in handlers_passkey.go, from-zero login in handlers_passkey_discoverable.go).
package passkey

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"felis.lolicon.best/internal/api"
)

// Verifier is the production api.PasskeyVerifier: a thin adapter over a configured
// *webauthn.WebAuthn relying party. It holds no per-ceremony state — the SessionData the
// server stashes between begin and finish is the only ceremony state, and it travels
// through the seam as opaque bytes (marshaled webauthn.SessionData).
type Verifier struct {
	wa *webauthn.WebAuthn
}

// compile-time proof the adapter satisfies the seam the handlers depend on.
var _ api.PasskeyVerifier = (*Verifier)(nil)

// New builds a Verifier for a relying party identified by rpID (the WebAuthn RP ID — the
// registrable-domain-suffix host, e.g. the panel hostname, without scheme or port) whose
// permitted browser origins are origins (each a full "https://host" string). displayName
// is the human-facing RP name an authenticator may show. It returns an error if the
// go-webauthn config is invalid (e.g. an empty rpID), so a misconfigured deployment fails
// loudly at construction rather than silently minting unverifiable challenges.
func New(rpID, displayName string, origins []string) (*Verifier, error) {
	// go-webauthn defers RP-id validation to the first ceremony; guard here so a
	// misconfigured deployment fails at construction (loudly, once) rather than minting
	// challenges that only fail later when a user tries to enroll.
	if rpID == "" {
		return nil, fmt.Errorf("passkey: relying-party id must not be empty")
	}
	if len(origins) == 0 {
		return nil, fmt.Errorf("passkey: at least one relying-party origin is required")
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: displayName,
		RPOrigins:     origins,
		// Require user verification (a PIN/biometric, not mere presence) at enrollment,
		// so a bound passkey always proves two factors — possession of the authenticator
		// AND the user. go-webauthn stamps this requirement into the SessionData at begin
		// and enforces the UV flag at CreateCredential, so an authenticator that only
		// tested presence is rejected. A device that cannot do UV simply falls back to the
		// email-OTP factor (migration 0004); no one is locked out.
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
			// Prefer a discoverable (resident) credential so a passkey can later be asserted
			// usernamelessly (task #40 from-zero login): the authenticator stores the credential
			// and can present it with no identifier typed. PREFERRED, not Required, keeps the
			// no-lockout ethos — an authenticator that cannot make a resident key still binds a
			// working username-first passkey (BeginLogin) and falls back to email-OTP; only the
			// from-zero convenience is unavailable. This shapes only the creation options a browser
			// receives (a server-side request, asserted in TestEnrollmentRequestsResidentKey);
			// whether a real authenticator honors it — actually storing a resident key — is a device
			// property no unit test can prove, so already-bound non-resident credentials stay
			// username-first until their owner enrolls a new passkey.
			ResidentKey: protocol.ResidentKeyRequirementPreferred,
		},
	})
	if err != nil {
		return nil, err
	}
	return &Verifier{wa: wa}, nil
}

// BeginRegistration starts a credential-creation ceremony. It returns the WebAuthn
// {"publicKey": {...}} creation options (marshaled verbatim for navigator.credentials.create())
// and the opaque, marshaled SessionData the handler stashes for finish. The passkeys the
// principal has already bound are passed as excludeCredentials so an authenticator that
// already holds a credential for this account refuses to create a second one.
func (v *Verifier) BeginRegistration(user api.PasskeyUser) (json.RawMessage, []byte, error) {
	var opts []webauthn.RegistrationOption
	if excl := excludeDescriptors(user.Credentials); len(excl) > 0 {
		opts = append(opts, webauthn.WithExclusions(excl))
	}
	creation, session, err := v.wa.BeginRegistration(webauthnUser{u: user}, opts...)
	if err != nil {
		return nil, nil, err
	}
	// CredentialCreation marshals to {"publicKey": {...}} (its Response field carries the
	// `publicKey` json tag), which is exactly the document the browser hands to
	// navigator.credentials.create().
	options, err := json.Marshal(creation)
	if err != nil {
		return nil, nil, err
	}
	// SessionData is JSON-marshalable; the handler treats the result as opaque and replays
	// the exact bytes at finish. We do NOT set an expiry inside SessionData — the challenge
	// row's TTL (passkeyChallengeTTL) is the single authority on liveness.
	sessionData, err := json.Marshal(session)
	if err != nil {
		return nil, nil, err
	}
	return options, sessionData, nil
}

// FinishRegistration verifies the browser's attestation against the stashed SessionData
// and returns the persist-ready credential. The user handle must match the one bound at
// begin (go-webauthn enforces bytes.Equal(user.WebAuthnID(), session.UserID)); the
// challenge, RP ID, and origin are all checked against server-held values, never against
// anything the client echoes.
func (v *Verifier) FinishRegistration(user api.PasskeyUser, sessionData []byte, attestation io.Reader) (api.VerifiedCredential, error) {
	var session webauthn.SessionData
	if err := json.Unmarshal(sessionData, &session); err != nil {
		return api.VerifiedCredential{}, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(attestation)
	if err != nil {
		return api.VerifiedCredential{}, err
	}
	cred, err := v.wa.CreateCredential(webauthnUser{u: user}, session, parsed)
	if err != nil {
		return api.VerifiedCredential{}, err
	}
	return api.VerifiedCredential{
		CredentialID: base64.RawURLEncoding.EncodeToString(cred.ID),
		PublicKey:    base64.StdEncoding.EncodeToString(cred.PublicKey),
		SignCount:    cred.Authenticator.SignCount,
		AAGUID:       aaguidString(cred.Authenticator.AAGUID),
		// Record the ceremony flags go-webauthn derived from the authenticator data.
		// UserVerified is redundant with the required-UV policy today (a non-UV finish is
		// rejected before we get here) but persisting it makes the guarantee auditable and
		// survives a future policy that permits UV=preferred credentials. BackupEligible/
		// BackupState tell a later login path whether the passkey is a single-device key or
		// a syncable/multi-device one — a posture signal worth capturing at bind time.
		UserVerified:   cred.Flags.UserVerified,
		BackupEligible: cred.Flags.BackupEligible,
		BackupState:    cred.Flags.BackupState,
	}, nil
}

// BeginLogin starts an assertion (login) ceremony for a KNOWN user. It is username-first
// by construction, not by preference: go-webauthn scopes allowCredentials to the user's
// bound passkeys (from WebAuthnCredentials), which is the only fit here because the
// enrolled credentials are not resident/discoverable and the challenge store is user-keyed
// (migration 0007) — discoverable ("usernameless") login would need resident-key
// enrollment plus a non-user-keyed challenge store, a future migration, so it is out of
// scope. It returns the {"publicKey": {...}} request options for navigator.credentials.get()
// and the opaque, marshaled SessionData the handler stashes and replays at finish. A user
// with no bound credential yields an error from go-webauthn (nothing to assert); the caller
// treats that as "offer the email-OTP fallback instead", never as a server fault.
func (v *Verifier) BeginLogin(user api.PasskeyUser) (json.RawMessage, []byte, error) {
	assertion, session, err := v.wa.BeginLogin(webauthnUser{u: user})
	if err != nil {
		return nil, nil, err
	}
	// CredentialAssertion marshals to {"publicKey": {...}} (its Response field carries the
	// `publicKey` json tag), exactly the document the browser hands to navigator.credentials.get().
	options, err := json.Marshal(assertion)
	if err != nil {
		return nil, nil, err
	}
	// As with registration, we stash the marshaled SessionData verbatim and let the
	// challenge row's TTL be the sole authority on liveness (no expiry inside SessionData).
	sessionData, err := json.Marshal(session)
	if err != nil {
		return nil, nil, err
	}
	return options, sessionData, nil
}

// FinishLogin verifies the browser's assertion against the stashed SessionData and reports
// which of the user's credentials signed and the signature counter the authenticator
// reported. go-webauthn checks the challenge, RP id, and origin against server-held values,
// that the asserted credential id is one the user actually holds (it returns
// protocol.ErrorUnknownCredential otherwise), and the signature against the stored COSE
// public key, and runs go-webauthn's UpdateCounter so a signature counter that fails to
// advance past the stored value raises CloneWarning. It does NOT decide clone policy here:
// the returned SignCount and CloneWarning are raw ceremony facts, and the handler — the one
// consumer, holding the stored counter — decides (it refuses, fail-closed). The verified
// credential id is returned base64url so the handler can look up the exact row to update.
func (v *Verifier) FinishLogin(user api.PasskeyUser, sessionData []byte, assertion io.Reader) (api.VerifiedAssertion, error) {
	var session webauthn.SessionData
	if err := json.Unmarshal(sessionData, &session); err != nil {
		return api.VerifiedAssertion{}, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(assertion)
	if err != nil {
		return api.VerifiedAssertion{}, err
	}
	cred, err := v.wa.ValidateLogin(webauthnUser{u: user}, session, parsed)
	if err != nil {
		return api.VerifiedAssertion{}, err
	}
	return api.VerifiedAssertion{
		CredentialID: base64.RawURLEncoding.EncodeToString(cred.ID),
		SignCount:    cred.Authenticator.SignCount,
		CloneWarning: cred.Authenticator.CloneWarning,
	}, nil
}

// BeginDiscoverableLogin starts a USERNAMELESS assertion ceremony (task #40): the caller is
// not yet identified, so — unlike BeginLogin — there is no user and no allowCredentials. The
// authenticator picks a resident (discoverable) credential it holds for this RP and reveals
// the account only inside the signed response at finish. It returns the {"publicKey": {...}}
// request options for navigator.credentials.get() and the opaque, marshaled SessionData the
// handler stashes under an opaque handle (migration 0013's non-user-keyed store) and replays
// at finish. User verification is required, matching enrollment, so a from-zero login still
// proves possession AND user.
func (v *Verifier) BeginDiscoverableLogin() (json.RawMessage, []byte, error) {
	assertion, session, err := v.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, nil, err
	}
	// CredentialAssertion marshals to {"publicKey": {...}} with an EMPTY allowCredentials —
	// exactly the usernameless document the browser hands to navigator.credentials.get().
	options, err := json.Marshal(assertion)
	if err != nil {
		return nil, nil, err
	}
	// As with the other ceremonies, we stash the marshaled SessionData verbatim and let the
	// challenge row's TTL be the sole authority on liveness (no expiry inside SessionData).
	sessionData, err := json.Marshal(session)
	if err != nil {
		return nil, nil, err
	}
	return options, sessionData, nil
}

// FinishDiscoverableLogin verifies a usernameless assertion (task #40). go-webauthn hands the
// authenticator-revealed user handle to resolveUser, which the caller uses to load the account
// and its bound credentials WITHOUT any client-supplied identifier; go-webauthn then checks
// the asserted credential id is one that user holds and verifies the signature against its
// stored COSE public key. The user handle is the account's stable id (webauthnUser.WebAuthnID),
// so resolveUser is a direct id lookup. A resolveUser error (unknown handle) fails the ceremony
// closed. resolveUser is a plain api-typed callback so the api package still never imports
// go-webauthn: the adapter wraps it into go-webauthn's DiscoverableUserHandler here.
func (v *Verifier) FinishDiscoverableLogin(resolveUser func(userHandle []byte) (api.PasskeyUser, error), sessionData []byte, assertion io.Reader) (api.VerifiedAssertion, error) {
	var session webauthn.SessionData
	if err := json.Unmarshal(sessionData, &session); err != nil {
		return api.VerifiedAssertion{}, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(assertion)
	if err != nil {
		return api.VerifiedAssertion{}, err
	}
	handler := func(_, userHandle []byte) (webauthn.User, error) {
		u, err := resolveUser(userHandle)
		if err != nil {
			return nil, err
		}
		return webauthnUser{u: u}, nil
	}
	cred, err := v.wa.ValidateDiscoverableLogin(handler, session, parsed)
	if err != nil {
		return api.VerifiedAssertion{}, err
	}
	return api.VerifiedAssertion{
		CredentialID: base64.RawURLEncoding.EncodeToString(cred.ID),
		SignCount:    cred.Authenticator.SignCount,
		CloneWarning: cred.Authenticator.CloneWarning,
	}, nil
}

// excludeDescriptors turns the principal's already-bound passkeys into the
// excludeCredentials list for a creation ceremony. A stored credential id that does not
// decode as base64url is skipped rather than aborting the whole ceremony — a single
// malformed row must not lock a user out of enrolling a new key.
func excludeDescriptors(creds []api.PasskeyCredential) []protocol.CredentialDescriptor {
	out := make([]protocol.CredentialDescriptor, 0, len(creds))
	for _, c := range creds {
		id, err := base64.RawURLEncoding.DecodeString(c.CredentialID)
		if err != nil {
			continue
		}
		out = append(out, protocol.CredentialDescriptor{
			Type:         protocol.PublicKeyCredentialType,
			CredentialID: protocol.URLEncodedBase64(id),
		})
	}
	return out
}

// aaguidString renders the 16-byte authenticator AAGUID as a canonical UUID string. An
// absent (wrong-length) or all-zero AAGUID — common for privacy-preserving platform
// authenticators — renders as "" so the display view omits it rather than showing a
// meaningless all-zero UUID.
func aaguidString(b []byte) string {
	if len(b) != 16 {
		return ""
	}
	allZero := true
	for _, x := range b {
		if x != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// webauthnUser adapts the seam's api.PasskeyUser to the webauthn.User interface. The user
// handle is the account's stable user id bytes; the same principal at begin and finish
// therefore yields the same handle, which is what lets go-webauthn's user/session identity
// check pass across the two calls.
type webauthnUser struct {
	u api.PasskeyUser
}

func (w webauthnUser) WebAuthnID() []byte          { return []byte(w.u.ID) }
func (w webauthnUser) WebAuthnName() string        { return w.u.Name }
func (w webauthnUser) WebAuthnDisplayName() string { return w.u.DisplayName }

// WebAuthnCredentials returns the principal's bound passkeys as webauthn.Credentials.
// Enrollment needs only the credential ids (for identity/exclusion bookkeeping); login
// (assertion) validation additionally needs the stored COSE public key (to verify the
// signature) and the last-seen signature counter (for clone detection), so both are
// populated when present. Filling them is backward-compatible with enrollment, which
// simply ignores the extra fields. A row whose id does not decode is skipped entirely; a
// row whose public key does not decode is still surfaced (so it counts for exclusion) but
// with a nil key, so an assertion against it cannot verify — it fails closed rather than
// silently accepting.
func (w webauthnUser) WebAuthnCredentials() []webauthn.Credential {
	out := make([]webauthn.Credential, 0, len(w.u.Credentials))
	for _, c := range w.u.Credentials {
		id, err := base64.RawURLEncoding.DecodeString(c.CredentialID)
		if err != nil {
			continue
		}
		cred := webauthn.Credential{ID: id}
		if key, err := base64.StdEncoding.DecodeString(c.PublicKey); err == nil {
			cred.PublicKey = key
		}
		cred.Authenticator.SignCount = c.SignCount
		out = append(out, cred)
	}
	return out
}
