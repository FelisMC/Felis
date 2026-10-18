// Package passkey adapts go-webauthn to the api.PasskeyVerifier seam; only the composition root imports it.
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

// Verifier implements api.PasskeyVerifier; ceremony state lives only in the session data it returns.
type Verifier struct {
	wa *webauthn.WebAuthn
}

var _ api.PasskeyVerifier = (*Verifier)(nil)

func New(rpID, displayName string, origins []string) (*Verifier, error) {
	// go-webauthn defers RP-id validation to the first ceremony.
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
		// User verification is required so a bound passkey proves possession and the user.
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
			// Preferred resident keys allow authenticators that support only username-first login.
			ResidentKey: protocol.ResidentKeyRequirementPreferred,
		},
	})
	if err != nil {
		return nil, err
	}
	return &Verifier{wa: wa}, nil
}

func (v *Verifier) BeginRegistration(user api.PasskeyUser) (json.RawMessage, []byte, error) {
	var opts []webauthn.RegistrationOption
	if excl := excludeDescriptors(user.Credentials); len(excl) > 0 {
		opts = append(opts, webauthn.WithExclusions(excl))
	}
	creation, session, err := v.wa.BeginRegistration(webauthnUser{u: user}, opts...)
	if err != nil {
		return nil, nil, err
	}
	options, err := json.Marshal(creation)
	if err != nil {
		return nil, nil, err
	}
	sessionData, err := json.Marshal(session)
	if err != nil {
		return nil, nil, err
	}
	return options, sessionData, nil
}

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
		CredentialID:   base64.RawURLEncoding.EncodeToString(cred.ID),
		PublicKey:      base64.StdEncoding.EncodeToString(cred.PublicKey),
		SignCount:      cred.Authenticator.SignCount,
		AAGUID:         aaguidString(cred.Authenticator.AAGUID),
		UserVerified:   cred.Flags.UserVerified,
		BackupEligible: cred.Flags.BackupEligible,
		BackupState:    cred.Flags.BackupState,
	}, nil
}

// BeginLogin returns the assertion options scoped to the user's credentials; it errors when the user has none.
func (v *Verifier) BeginLogin(user api.PasskeyUser) (json.RawMessage, []byte, error) {
	assertion, session, err := v.wa.BeginLogin(webauthnUser{u: user})
	if err != nil {
		return nil, nil, err
	}
	options, err := json.Marshal(assertion)
	if err != nil {
		return nil, nil, err
	}
	sessionData, err := json.Marshal(session)
	if err != nil {
		return nil, nil, err
	}
	return options, sessionData, nil
}

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
	return verifiedAssertion(cred), nil
}

// BeginDiscoverableLogin starts a usernameless assertion with no allowCredentials and user verification required.
func (v *Verifier) BeginDiscoverableLogin() (json.RawMessage, []byte, error) {
	assertion, session, err := v.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, nil, err
	}
	options, err := json.Marshal(assertion)
	if err != nil {
		return nil, nil, err
	}
	sessionData, err := json.Marshal(session)
	if err != nil {
		return nil, nil, err
	}
	return options, sessionData, nil
}

// FinishDiscoverableLogin calls resolveUser with the asserted user handle; a resolveUser error fails the ceremony.
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
	return verifiedAssertion(cred), nil
}

// verifiedAssertion's UserVerified reflects this assertion: go-webauthn overwrites cred.Flags with its flags.
func verifiedAssertion(cred *webauthn.Credential) api.VerifiedAssertion {
	return api.VerifiedAssertion{
		CredentialID: base64.RawURLEncoding.EncodeToString(cred.ID),
		SignCount:    cred.Authenticator.SignCount,
		CloneWarning: cred.Authenticator.CloneWarning,
		UserVerified: cred.Flags.UserVerified,
	}
}

// excludeDescriptors skips credential ids that do not decode, so one malformed row cannot block enrollment.
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

type webauthnUser struct {
	u api.PasskeyUser
}

func (w webauthnUser) WebAuthnID() []byte          { return []byte(w.u.ID) }
func (w webauthnUser) WebAuthnName() string        { return w.u.Name }
func (w webauthnUser) WebAuthnDisplayName() string { return w.u.DisplayName }

// WebAuthnCredentials skips ids that do not decode; an undecodable public key leaves a nil key so assertions against it fail.
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
		// ValidateLogin rejects an assertion whose backup-eligible flag differs from the stored one.
		cred.Flags = webauthn.CredentialFlags{
			UserPresent:    true,
			UserVerified:   c.UserVerified,
			BackupEligible: c.BackupEligible,
			BackupState:    c.BackupState,
		}
		out = append(out, cred)
	}
	return out
}
