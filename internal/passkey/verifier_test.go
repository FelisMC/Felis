package passkey

import (
	"encoding/base64"
	"strings"
	"testing"

	virtualwebauthn "github.com/descope/virtualwebauthn"

	"felis.lolicon.best/internal/api"
)

// Test relying party. RPID is a bare host; the origin is that host as an https URL. Both
// the go-webauthn config (via New) and the virtual authenticator (via RelyingParty) must
// agree on these, exactly as a real deployment's config and a real browser must agree.
const (
	testRPID    = "mc.example.net"
	testRPName  = "Felis"
	testOrigin  = "https://mc.example.net"
	testUserID  = "u1"
	testUserEml = "u1@example.net"
)

func newTestVerifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := New(testRPID, testRPName, []string{testOrigin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

func testUser(creds ...api.PasskeyCredential) api.PasskeyUser {
	return api.PasskeyUser{ID: testUserID, Name: testUserEml, DisplayName: testUserEml, Credentials: creds}
}

// virtualRP mirrors the verifier's RP so the authenticator signs clientDataJSON with the
// origin go-webauthn will check against.
func virtualRP() virtualwebauthn.RelyingParty {
	return virtualwebauthn.RelyingParty{ID: testRPID, Name: testRPName, Origin: testOrigin}
}

// TestRegisterRoundTrip is the PARITY check: a real go-webauthn relying party (through our
// adapter) issues a creation challenge, a virtual authenticator produces a real attestation
// response, and the adapter verifies it end to end. This exercises the pieces a CODE-ONLY
// adapter can silently get wrong: the {"publicKey":{...}} options marshal, the SessionData
// []byte round-trip through the seam, and the base64url/base64 encoding of the verified
// credential id and public key. A green run means the adapter's ceremony logic and the
// underlying crypto agree — it does NOT prove production works: the RP id/origin here are
// the test's, so whether cfg.Auth.PanelHostname matches the real browser origin stays an
// integration concern, and the pgrepo persistence remains unverified until a real DB run.
func TestRegisterRoundTrip(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	options, sessionData, err := v.BeginRegistration(testUser())
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	// The options the browser receives must be a WebAuthn creation document. The virtual
	// authenticator's parser (like a browser) reads the publicKey member.
	attestationOpts, err := virtualwebauthn.ParseAttestationOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAttestationOptions: %v (options=%s)", err, options)
	}
	if attestationOpts.RelyingPartyID != testRPID {
		t.Fatalf("options RP id = %q, want %q", attestationOpts.RelyingPartyID, testRPID)
	}

	attestationResponse := virtualwebauthn.CreateAttestationResponse(rp, authenticator, cred, *attestationOpts)

	vc, err := v.FinishRegistration(testUser(), sessionData, strings.NewReader(attestationResponse))
	if err != nil {
		t.Fatalf("FinishRegistration: %v", err)
	}

	// The verified credential id must be the authenticator's credential id, base64url.
	wantID := base64.RawURLEncoding.EncodeToString(cred.ID)
	if vc.CredentialID != wantID {
		t.Errorf("CredentialID = %q, want %q", vc.CredentialID, wantID)
	}
	if vc.PublicKey == "" {
		t.Error("PublicKey is empty; expected the COSE public key")
	}
	if _, err := base64.StdEncoding.DecodeString(vc.PublicKey); err != nil {
		t.Errorf("PublicKey is not valid base64: %v", err)
	}
}

// TestRegisterOriginMismatchRejected proves the adapter is really checking the origin: an
// authenticator that signs a DIFFERENT origin than the RP is configured for must fail
// verification. If this passed, the round-trip test above would be meaningless (it would
// accept anything).
func TestRegisterOriginMismatchRejected(t *testing.T) {
	v := newTestVerifier(t)
	// Authenticator signs an origin the verifier does not permit.
	rp := virtualwebauthn.RelyingParty{ID: testRPID, Name: testRPName, Origin: "https://evil.example.net"}
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	options, sessionData, err := v.BeginRegistration(testUser())
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	attestationOpts, err := virtualwebauthn.ParseAttestationOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAttestationOptions: %v", err)
	}
	attestationResponse := virtualwebauthn.CreateAttestationResponse(rp, authenticator, cred, *attestationOpts)

	if _, err := v.FinishRegistration(testUser(), sessionData, strings.NewReader(attestationResponse)); err == nil {
		t.Fatal("FinishRegistration accepted an attestation signed for a foreign origin; want rejection")
	}
}

// TestRegisterUserMismatchRejected proves the user handle is bound across the ceremony:
// finishing as a different principal than began must fail (go-webauthn checks
// bytes.Equal(user.WebAuthnID(), session.UserID)). This is the guard that a stashed
// challenge cannot be redeemed for a different account.
func TestRegisterUserMismatchRejected(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	options, sessionData, err := v.BeginRegistration(testUser())
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	attestationOpts, err := virtualwebauthn.ParseAttestationOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAttestationOptions: %v", err)
	}
	attestationResponse := virtualwebauthn.CreateAttestationResponse(rp, authenticator, cred, *attestationOpts)

	other := api.PasskeyUser{ID: "u2", Name: "u2@example.net", DisplayName: "u2@example.net"}
	if _, err := v.FinishRegistration(other, sessionData, strings.NewReader(attestationResponse)); err == nil {
		t.Fatal("FinishRegistration accepted a finish by a different principal; want rejection")
	}
}

// TestBeginExcludesBoundCredentials proves an already-bound passkey is surfaced to the
// authenticator as an excludeCredentials entry, so a device cannot double-bind. The
// exclusion id must be the stored credential id, base64url.
func TestBeginExcludesBoundCredentials(t *testing.T) {
	v := newTestVerifier(t)
	existingRaw := []byte("existing-credential-id-bytes")
	existingID := base64.RawURLEncoding.EncodeToString(existingRaw)

	options, _, err := v.BeginRegistration(testUser(api.PasskeyCredential{CredentialID: existingID}))
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	attestationOpts, err := virtualwebauthn.ParseAttestationOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAttestationOptions: %v", err)
	}
	found := false
	for _, ex := range attestationOpts.ExcludeCredentials {
		if ex == existingID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("excludeCredentials = %v, want it to contain %q", attestationOpts.ExcludeCredentials, existingID)
	}
}

// TestNewRejectsEmptyRPID proves a misconfigured deployment fails loudly at construction
// rather than minting challenges no browser can honor.
func TestNewRejectsEmptyRPID(t *testing.T) {
	if _, err := New("", testRPName, []string{testOrigin}); err == nil {
		t.Fatal("New accepted an empty RP id; want an error")
	}
}
