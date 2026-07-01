// Package passkey wraps github.com/go-webauthn/webauthn behind the api.PasskeyVerifier
// seam. The api package deliberately never imports go-webauthn — ceremony state crosses
// the boundary as opaque bytes, the attestation as an io.Reader, and the verified result
// as a plain api.VerifiedCredential — so the real relying-party crypto lives here and is
// wired in only at the composition root (cmd/felis). The dependency arrow points one way:
// this package imports api for the seam types; api never imports this package, which is
// what keeps the seam (and the api test suite's fake verifier) honest.
//
// Scope: ENROLLMENT only, matching handlers_passkey.go. This wraps BeginRegistration and
// CreateCredential (the credential-creation ceremony). The login/assertion path
// (BeginLogin/ValidateLogin) is a deferred slice and is intentionally not adapted here.
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
// Enrollment only needs the credential ids (for identity/exclusion bookkeeping), so only
// the id is populated; a row whose id does not decode is skipped.
func (w webauthnUser) WebAuthnCredentials() []webauthn.Credential {
	out := make([]webauthn.Credential, 0, len(w.u.Credentials))
	for _, c := range w.u.Credentials {
		id, err := base64.RawURLEncoding.DecodeString(c.CredentialID)
		if err != nil {
			continue
		}
		out = append(out, webauthn.Credential{ID: id})
	}
	return out
}
