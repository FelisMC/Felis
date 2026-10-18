package passkey

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	virtualwebauthn "github.com/descope/virtualwebauthn"

	"felis.lolicon.best/internal/api"
)

// Test relying party: RPID is a bare host, the origin is that host as an https URL.
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

// virtualRP mirrors the verifier's RP so the authenticator signs the origin go-webauthn checks.
func virtualRP() virtualwebauthn.RelyingParty {
	return virtualwebauthn.RelyingParty{ID: testRPID, Name: testRPName, Origin: testOrigin}
}

func TestRegisterRoundTrip(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	options, sessionData, err := v.BeginRegistration(testUser())
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	// The virtual authenticator's parser, like a browser, reads the publicKey member.
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
	if !vc.UserVerified {
		t.Error("UserVerified = false; a verified enrollment must record UV=true")
	}
}

func TestRegisterRequiresUserVerification(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserNotVerified: true})
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
		t.Fatal("FinishRegistration accepted a presence-only (no user verification) attestation; want rejection")
	}
}

func TestRegisterOriginMismatchRejected(t *testing.T) {
	v := newTestVerifier(t)
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
	if !slices.Contains(attestationOpts.ExcludeCredentials, existingID) {
		t.Errorf("excludeCredentials = %v, want it to contain %q", attestationOpts.ExcludeCredentials, existingID)
	}
}

func TestNewRejectsEmptyRPID(t *testing.T) {
	if _, err := New("", testRPName, []string{testOrigin}); err == nil {
		t.Fatal("New accepted an empty RP id; want an error")
	}
}

// enrollCredential runs a real enrollment and returns the stored credential view that login tests validate against.
func enrollCredential(t *testing.T, v *Verifier, rp virtualwebauthn.RelyingParty, auth virtualwebauthn.Authenticator, cred virtualwebauthn.Credential) api.PasskeyCredential {
	t.Helper()
	options, sessionData, err := v.BeginRegistration(testUser())
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	attestationOpts, err := virtualwebauthn.ParseAttestationOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAttestationOptions: %v", err)
	}
	attestationResponse := virtualwebauthn.CreateAttestationResponse(rp, auth, cred, *attestationOpts)
	vc, err := v.FinishRegistration(testUser(), sessionData, strings.NewReader(attestationResponse))
	if err != nil {
		t.Fatalf("FinishRegistration: %v", err)
	}
	return api.PasskeyCredential{CredentialID: vc.CredentialID, PublicKey: vc.PublicKey, SignCount: vc.SignCount,
		UserVerified: vc.UserVerified, BackupEligible: vc.BackupEligible, BackupState: vc.BackupState}
}

func TestLoginRoundTrip(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	stored := enrollCredential(t, v, rp, authenticator, cred)

	cred.Counter = 7

	options, sessionData, err := v.BeginLogin(testUser(stored))
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	assertionOpts, err := virtualwebauthn.ParseAssertionOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v (options=%s)", err, options)
	}
	if assertionOpts.RelyingPartyID != testRPID {
		t.Fatalf("options RP id = %q, want %q", assertionOpts.RelyingPartyID, testRPID)
	}
	wantID := base64.RawURLEncoding.EncodeToString(cred.ID)
	if !slices.Contains(assertionOpts.AllowCredentials, wantID) {
		t.Fatalf("allowCredentials = %v, want it to contain %q", assertionOpts.AllowCredentials, wantID)
	}

	assertionResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *assertionOpts)
	va, err := v.FinishLogin(testUser(stored), sessionData, strings.NewReader(assertionResponse))
	if err != nil {
		t.Fatalf("FinishLogin: %v", err)
	}
	if va.CredentialID != stored.CredentialID {
		t.Errorf("asserted CredentialID = %q, want %q", va.CredentialID, stored.CredentialID)
	}
	if va.SignCount != 7 {
		t.Errorf("SignCount = %d, want 7 (the authenticator's advanced counter)", va.SignCount)
	}
}

func TestLoginOriginMismatchRejected(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	stored := enrollCredential(t, v, rp, authenticator, cred)

	options, sessionData, err := v.BeginLogin(testUser(stored))
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	assertionOpts, err := virtualwebauthn.ParseAssertionOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v", err)
	}
	evil := virtualwebauthn.RelyingParty{ID: testRPID, Name: testRPName, Origin: "https://evil.example.net"}
	assertionResponse := virtualwebauthn.CreateAssertionResponse(evil, authenticator, cred, *assertionOpts)
	if _, err := v.FinishLogin(testUser(stored), sessionData, strings.NewReader(assertionResponse)); err == nil {
		t.Fatal("FinishLogin accepted an assertion signed for a foreign origin; want rejection")
	}
}

func TestLoginUnknownCredentialRejected(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	credA := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	stored := enrollCredential(t, v, rp, authenticator, credA)

	options, sessionData, err := v.BeginLogin(testUser(stored))
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	assertionOpts, err := virtualwebauthn.ParseAssertionOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v", err)
	}
	credB := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	assertionResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, credB, *assertionOpts)
	if _, err := v.FinishLogin(testUser(stored), sessionData, strings.NewReader(assertionResponse)); err == nil {
		t.Fatal("FinishLogin accepted an assertion from an unbound credential; want rejection")
	}
}

func TestDiscoverableLoginRoundTrip(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	stored := enrollCredential(t, v, rp, authenticator, cred)

	authenticator.Options.UserHandle = []byte(testUserID)
	cred.Counter = 9

	options, sessionData, err := v.BeginDiscoverableLogin()
	if err != nil {
		t.Fatalf("BeginDiscoverableLogin: %v", err)
	}
	assertionOpts, err := virtualwebauthn.ParseAssertionOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v (options=%s)", err, options)
	}
	if assertionOpts.RelyingPartyID != testRPID {
		t.Fatalf("options RP id = %q, want %q", assertionOpts.RelyingPartyID, testRPID)
	}
	if len(assertionOpts.AllowCredentials) != 0 {
		t.Fatalf("allowCredentials = %v, want empty (usernameless request names no credential)", assertionOpts.AllowCredentials)
	}

	assertionResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *assertionOpts)

	// resolve records the handle it receives and returns the stored credential.
	var gotHandle []byte
	resolve := func(userHandle []byte) (api.PasskeyUser, error) {
		gotHandle = userHandle
		return testUser(stored), nil
	}
	va, err := v.FinishDiscoverableLogin(resolve, sessionData, strings.NewReader(assertionResponse))
	if err != nil {
		t.Fatalf("FinishDiscoverableLogin: %v", err)
	}
	if string(gotHandle) != testUserID {
		t.Errorf("resolver received userHandle %q, want %q (the account is revealed by the assertion)", gotHandle, testUserID)
	}
	if va.CredentialID != stored.CredentialID {
		t.Errorf("asserted CredentialID = %q, want %q", va.CredentialID, stored.CredentialID)
	}
	if va.SignCount != 9 {
		t.Errorf("SignCount = %d, want 9 (the authenticator's advanced counter)", va.SignCount)
	}
}

func TestDiscoverableLoginResolveFailsClosed(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	enrollCredential(t, v, rp, authenticator, cred)
	authenticator.Options.UserHandle = []byte("nonexistent-account")

	options, sessionData, err := v.BeginDiscoverableLogin()
	if err != nil {
		t.Fatalf("BeginDiscoverableLogin: %v", err)
	}
	assertionOpts, err := virtualwebauthn.ParseAssertionOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v", err)
	}
	assertionResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *assertionOpts)

	resolve := func(userHandle []byte) (api.PasskeyUser, error) {
		return api.PasskeyUser{}, api.ErrNotFound
	}
	if _, err := v.FinishDiscoverableLogin(resolve, sessionData, strings.NewReader(assertionResponse)); err == nil {
		t.Fatal("FinishDiscoverableLogin accepted an assertion whose account could not be resolved; want rejection")
	}
}

func TestDiscoverableLoginUnboundCredentialRejected(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	credA := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	stored := enrollCredential(t, v, rp, authenticator, credA)

	authenticator.Options.UserHandle = []byte(testUserID)

	options, sessionData, err := v.BeginDiscoverableLogin()
	if err != nil {
		t.Fatalf("BeginDiscoverableLogin: %v", err)
	}
	assertionOpts, err := virtualwebauthn.ParseAssertionOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v", err)
	}
	credB := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	assertionResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, credB, *assertionOpts)

	resolve := func(userHandle []byte) (api.PasskeyUser, error) {
		return testUser(stored), nil
	}
	if _, err := v.FinishDiscoverableLogin(resolve, sessionData, strings.NewReader(assertionResponse)); err == nil {
		t.Fatal("FinishDiscoverableLogin accepted an assertion signed by a credential not bound to the resolved user; want rejection")
	}
}

func TestLoginCloneWarningSurfaced(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	stored := enrollCredential(t, v, rp, authenticator, cred)

	stored.SignCount = 100
	cred.Counter = 50

	options, sessionData, err := v.BeginLogin(testUser(stored))
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	assertionOpts, err := virtualwebauthn.ParseAssertionOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v", err)
	}
	assertionResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *assertionOpts)
	va, err := v.FinishLogin(testUser(stored), sessionData, strings.NewReader(assertionResponse))
	if err != nil {
		t.Fatalf("FinishLogin: %v (a counter regression must still VERIFY, only flag CloneWarning)", err)
	}
	if !va.CloneWarning {
		t.Fatal("va.CloneWarning = false, want true (presented counter at/below the stored counter is a clone signal)")
	}
}

func TestDiscoverableLoginCloneWarningSurfaced(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	stored := enrollCredential(t, v, rp, authenticator, cred)

	authenticator.Options.UserHandle = []byte(testUserID)
	stored.SignCount = 100
	cred.Counter = 50

	options, sessionData, err := v.BeginDiscoverableLogin()
	if err != nil {
		t.Fatalf("BeginDiscoverableLogin: %v", err)
	}
	assertionOpts, err := virtualwebauthn.ParseAssertionOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v (options=%s)", err, options)
	}
	assertionResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *assertionOpts)
	resolve := func(userHandle []byte) (api.PasskeyUser, error) {
		return testUser(stored), nil
	}
	va, err := v.FinishDiscoverableLogin(resolve, sessionData, strings.NewReader(assertionResponse))
	if err != nil {
		t.Fatalf("FinishDiscoverableLogin: %v (a counter regression must still VERIFY, only flag CloneWarning)", err)
	}
	if !va.CloneWarning {
		t.Fatal("va.CloneWarning = false, want true (presented counter at/below the stored counter is a clone signal)")
	}
}

func TestEnrollmentRequestsResidentKey(t *testing.T) {
	v := newTestVerifier(t)
	options, _, err := v.BeginRegistration(testUser())
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	var doc struct {
		PublicKey struct {
			AuthenticatorSelection struct {
				ResidentKey string `json:"residentKey"`
			} `json:"authenticatorSelection"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &doc); err != nil {
		t.Fatalf("unmarshal creation options: %v (options=%s)", err, options)
	}
	if got := doc.PublicKey.AuthenticatorSelection.ResidentKey; got != "preferred" {
		t.Errorf("authenticatorSelection.residentKey = %q, want %q", got, "preferred")
	}
}

func TestSyncedPasskeyLogsIn(t *testing.T) {
	for _, door := range []string{"username-first", "discoverable"} {
		t.Run(door, func(t *testing.T) {
			v := newTestVerifier(t)
			rp := virtualRP()
			authenticator := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{BackupEligible: true, BackupState: true})
			cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
			stored := enrollCredential(t, v, rp, authenticator, cred)
			if !stored.BackupEligible || !stored.BackupState {
				t.Fatalf("enrollment recorded BE=%v BS=%v, want both true", stored.BackupEligible, stored.BackupState)
			}

			var va api.VerifiedAssertion
			var err error
			if door == "username-first" {
				options, sessionData, berr := v.BeginLogin(testUser(stored))
				if berr != nil {
					t.Fatalf("BeginLogin: %v", berr)
				}
				opts, perr := virtualwebauthn.ParseAssertionOptions(string(options))
				if perr != nil {
					t.Fatalf("ParseAssertionOptions: %v", perr)
				}
				resp := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *opts)
				va, err = v.FinishLogin(testUser(stored), sessionData, strings.NewReader(resp))
			} else {
				authenticator.Options.UserHandle = []byte(testUserID)
				options, sessionData, berr := v.BeginDiscoverableLogin()
				if berr != nil {
					t.Fatalf("BeginDiscoverableLogin: %v", berr)
				}
				opts, perr := virtualwebauthn.ParseAssertionOptions(string(options))
				if perr != nil {
					t.Fatalf("ParseAssertionOptions: %v", perr)
				}
				resp := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *opts)
				va, err = v.FinishDiscoverableLogin(func([]byte) (api.PasskeyUser, error) { return testUser(stored), nil },
					sessionData, strings.NewReader(resp))
			}
			if err != nil {
				t.Fatalf("finish: %v (a synced passkey must log in)", err)
			}
			if !va.UserVerified {
				t.Error("va.UserVerified = false, want true (the authenticator verified the user)")
			}
		})
	}
}
