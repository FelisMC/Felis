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
	// The default virtual authenticator performs user verification, and enrollment now
	// requires it — so the recorded UserVerified flag must be true. This proves the flag is
	// captured from the ceremony (not left at its zero value) end to end.
	if !vc.UserVerified {
		t.Error("UserVerified = false; a verified enrollment must record UV=true")
	}
}

// TestRegisterRequiresUserVerification proves the required-UV policy is enforced, not just
// advertised: an authenticator that tests presence but does NOT verify the user (no
// PIN/biometric) must be rejected at finish. go-webauthn stamps UV=required into the
// SessionData at begin and checks the UV flag at CreateCredential; without this guard a
// silent, presence-only passkey could be bound. Pairs with TestRegisterRoundTrip (which
// proves a UV-capable authenticator still succeeds), so the policy neither over- nor
// under-blocks.
func TestRegisterRequiresUserVerification(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	// UserNotVerified: the authenticator signs with the UV flag clear.
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
	if !slices.Contains(attestationOpts.ExcludeCredentials, existingID) {
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

// enrollCredential runs a real credential-creation ceremony and returns the verified
// credential as the persist-ready stored view a later login validates against. Starting
// the login tests from a GENUINE COSE public key (not a hand-built one) is what makes them
// exercise WebAuthnCredentials()' base64 decode path — the exact spot an adapter silently
// breaks.
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
	return api.PasskeyCredential{CredentialID: vc.CredentialID, PublicKey: vc.PublicKey, SignCount: vc.SignCount}
}

// TestLoginRoundTrip is the PARITY check for the assertion (login) half, deliberately
// chained onto a REAL enrollment so the login validates against a genuine COSE public key.
// A real go-webauthn RP (through our adapter) enrolls a virtual authenticator's credential;
// the verified public key + credential id are fed back as the user's STORED credential into
// BeginLogin → the virtual authenticator signs an assertion → FinishLogin verifies it. This
// exercises exactly the path a hand-built credential would skip: WebAuthnCredentials()
// decoding the base64 COSE key so ValidateLogin can check the signature against it. The
// authenticator's counter is advanced before the assertion so the test also proves
// FinishLogin surfaces the real signature counter rather than a hardcoded 0.
func TestLoginRoundTrip(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	stored := enrollCredential(t, v, rp, authenticator, cred)

	// The authenticator reports an advanced counter; a fresh enrollment stored 0, so this
	// is a strict increase (no clone warning) and must survive through to VerifiedAssertion.
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
	// The bound credential must be offered as an allowCredentials entry — username-first,
	// the ceremony names which credentials the known user may assert.
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

// TestLoginOriginMismatchRejected proves FinishLogin actually checks the origin: an
// assertion signed for an origin the RP does not permit must fail. Without this guard the
// round-trip test would be hollow — it would accept a signature from anywhere.
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
	// Sign the assertion for a foreign origin the verifier does not permit.
	evil := virtualwebauthn.RelyingParty{ID: testRPID, Name: testRPName, Origin: "https://evil.example.net"}
	assertionResponse := virtualwebauthn.CreateAssertionResponse(evil, authenticator, cred, *assertionOpts)
	if _, err := v.FinishLogin(testUser(stored), sessionData, strings.NewReader(assertionResponse)); err == nil {
		t.Fatal("FinishLogin accepted an assertion signed for a foreign origin; want rejection")
	}
}

// TestLoginUnknownCredentialRejected proves the credential-ownership binding: a valid
// signature over the right challenge is NOT enough — it must come from one of the user's
// own bound credentials. The user's stored credential is A, but the assertion is signed by
// a different, never-bound credential B; go-webauthn must reject it (B is not in the
// challenge's allowCredentials, nor among the user's credentials).
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

// TestDiscoverableLoginRoundTrip is the PARITY check for the usernameless (from-zero) half
// (task #40), and the proof its VERIFY path is real crypto rather than a stub. It differs from
// TestLoginRoundTrip in the two ways that define discoverable login: the begin names no user
// (so the request's allowCredentials must be EMPTY), and the account is revealed only by the
// userHandle the authenticator embeds in the signed assertion — the verifier hands that handle
// to a resolve callback that stands in for the handler's userHandle → UserByID lookup. Chained
// onto a REAL enrollment so the assertion validates against a genuine COSE key, and the
// authenticator's counter is advanced first so the surfaced SignCount is proven real, not a
// hardcoded 0. What this does NOT prove: that a real authenticator actually STORED a resident
// key — that residency is a device property (see TestEnrollmentRequestsResidentKey for the only
// thing a unit test can pin, the request the browser receives).
func TestDiscoverableLoginRoundTrip(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	stored := enrollCredential(t, v, rp, authenticator, cred)

	// The authenticator returns the user handle in the assertion — this is what a resident
	// credential does and what lets the account be resolved from nothing typed. It is the
	// account's stable user id (WebAuthnID), so the resolver must receive exactly these bytes.
	authenticator.Options.UserHandle = []byte(testUserID)
	// Advance the counter so a real (non-zero, strictly increasing) SignCount must survive.
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
	// The defining property of a usernameless request: no credential is named. If this were
	// non-empty the ceremony would be username-first and the test would prove nothing about #40.
	if len(assertionOpts.AllowCredentials) != 0 {
		t.Fatalf("allowCredentials = %v, want empty (usernameless request names no credential)", assertionOpts.AllowCredentials)
	}

	assertionResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *assertionOpts)

	// resolve stands in for the handler's userHandle → UserByID lookup: it records the handle
	// it was handed (to prove the account is revealed by the authenticator, not the client) and
	// returns the stored credential so ValidateDiscoverableLogin can verify the signature.
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

// TestDiscoverableLoginResolveFailsClosed proves the from-zero door fails CLOSED when the
// authenticator-revealed account cannot be resolved: a resolve callback that returns an error
// (the handler's UserByID found nothing — a handle for a deleted/unknown account) must abort
// the ceremony, never mint an assertion. Without this the usernameless path could be coaxed
// into treating an unresolvable handle as success. Pairs with the round-trip above so the
// resolver neither over- nor under-blocks.
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

// TestDiscoverableLoginUnboundCredentialRejected proves the credential-ownership binding for
// the usernameless door — the defense unique to it. In username-first login the server names
// allowCredentials, so an assertion must match a credential the server itself offered. The
// from-zero door names NOTHING: the authenticator reveals BOTH the userHandle and the signing
// credential, so the ONLY barrier stopping an attacker from signing with their own resident key
// while embedding a victim's userHandle is go-webauthn's check that the asserted credential id
// belongs to the resolved user. Here the resolve callback succeeds (the handle names a REAL
// account, u1, holding credential A) — unlike TestDiscoverableLoginResolveFailsClosed where it
// resolves to nothing — but the assertion is signed by credential B, never bound to u1.
// FinishDiscoverableLogin must reject: a good signature over the right challenge under a valid
// userHandle is still not enough without membership. This is the exact guard the "account is
// revealed by the assertion, never named by the client" claim leans on.
func TestDiscoverableLoginUnboundCredentialRejected(t *testing.T) {
	v := newTestVerifier(t)
	rp := virtualRP()
	authenticator := virtualwebauthn.NewAuthenticator()
	credA := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	stored := enrollCredential(t, v, rp, authenticator, credA)

	// A valid handle: it resolves to the real account u1, which holds credential A.
	authenticator.Options.UserHandle = []byte(testUserID)

	options, sessionData, err := v.BeginDiscoverableLogin()
	if err != nil {
		t.Fatalf("BeginDiscoverableLogin: %v", err)
	}
	assertionOpts, err := virtualwebauthn.ParseAssertionOptions(string(options))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v", err)
	}
	// Sign with a fresh credential never bound to u1. The resolver still returns u1's real
	// credential set (credential A) — so the ONLY thing that can reject this is the check that
	// the asserted credential (B) is among the resolved user's credentials.
	credB := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	assertionResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, credB, *assertionOpts)

	resolve := func(userHandle []byte) (api.PasskeyUser, error) {
		return testUser(stored), nil
	}
	if _, err := v.FinishDiscoverableLogin(resolve, sessionData, strings.NewReader(assertionResponse)); err == nil {
		t.Fatal("FinishDiscoverableLogin accepted an assertion signed by a credential not bound to the resolved user; want rejection")
	}
}

// TestEnrollmentRequestsResidentKey pins the ONLY server-side half of the from-zero enabler a
// unit test can prove: that enrollment ASKS the browser for a resident (discoverable) key, i.e.
// the creation options carry authenticatorSelection.residentKey = "preferred". Whether a real
// authenticator honors the request — actually persisting a resident key so it can later be
// asserted usernamelessly — is a device property no unit test can reach, which is exactly why
// the from-zero door is inert for a credential until its owner enrolls a NEW passkey against
// these options. "preferred" (not "required") is deliberate: an authenticator that cannot store
// a resident key still binds a working username-first passkey and falls back to email-OTP, so
// no one is locked out — asserting the exact string guards against a silent drop to "" (ask for
// nothing) or a tightening to "required" (which would break the no-lockout ethos).
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
