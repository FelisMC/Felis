package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Pre-session Passkey (assertion) LOGIN tests (spec §B, console.<root_domain>
// returning-player door — the public sibling of the email-OTP login door). These drive
// the two Public routes against the fakeRepo challenge state machine and a fake
// PasskeyVerifier, so what they PROVE is the handler + login state machine (challenge
// stash → consume → session mint), not the pgrepo SQL nor the cryptographic assertion
// verification (both mirrored, not run here). The load-bearing properties, in flow order:
//
//   - Session-data round-trip: the finish body carries only email+assertion, so the only
//     path for the stashed blob into FinishLogin is store-stash → consume — the challenge
//     is never client-echoed.
//   - Anti-enumeration on finish: unknown email, no live challenge, expired challenge and
//     a bad assertion all collapse to ONE passkey_login_invalid envelope, so the finish
//     half is never an existence/state oracle.
//   - Cooldown seals the accepted begin-side trade-off: has-passkey (200) vs no_passkey
//     (400) is a status oracle, but one begin per recipient per window throttles probing.
//   - Staff admitted: unlike the email door's staff_account refusal, a passkey stands
//     alone (possession + user-verification), so role=admin mints a session here.

// seedLoginPasskeyAPI wires the public passkey-login door: local sessions enabled, a
// single verified player "player" (id u1) whose proven address is stored in MIXED case
// (so the resolve-on-typed-lowercase contract is exercised by default), one passkey
// credential bound to that account, and a verifier primed with fixed options + a verified
// assertion. Both routes are Public — no External principal wired, proving they are truly
// pre-session.
func seedLoginPasskeyAPI(t *testing.T) (*API, *fakeRepo, *fakePasskeyVerifier) {
	t.Helper()
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["player"] = &StaffUser{
		ID: "u1", Username: "player", Email: "Player@Example.NET",
		Role: "user", EmailVerified: true,
	}
	repo.passkeyCreds["row1"] = PasskeyCredential{
		ID: "row1", UserID: "u1", CredentialID: "cred-1", PublicKey: "k", CreatedAt: frozenNow,
	}
	v := &fakePasskeyVerifier{
		options:   json.RawMessage(`{"publicKey":{"challenge":"YXNzZXJ0"}}`),
		assertion: VerifiedAssertion{CredentialID: "cred-1", UserVerified: true},
	}
	api := newTestAPI(repo, newFakeCluster())
	api.Passkey = v
	return api, repo, v
}

// plantLoginChallenge seeds a stashed LOGIN-purpose challenge for u1 directly, so the
// finish-side branches (expired, verification failure) are reachable under the frozen
// clock without running begin first. Mirrors plantPasskeyChallenge, but scoped to
// passkeyPurposeLogin so it is only ever consumed by the login door.
func plantLoginChallenge(repo *fakeRepo, id string, expiresAt time.Time) {
	repo.passkeyChallenges[id] = &fakePasskeyChallenge{
		id: id, userID: "u1", purpose: passkeyPurposeLogin,
		sessionData: []byte("login-session:u1"), expiresAt: expiresAt, createdAt: expiresAt,
	}
}

// TestPasskeyLoginVertical walks the whole returning-player slice across the external
// face: begin resolves the mixed-case account from a lowercase-typed email, hands its
// bound credential to the verifier, returns the assertion options verbatim and stashes
// one login challenge; finish verifies the assertion against the SERVER-STASHED session
// data and mints the same host-only felis_session as the email door. The decisive
// assertion is the session-data round-trip: the finish body carries only email+assertion,
// so the only path for the stashed blob into FinishLogin is store-stash → consume,
// proving the challenge is never client-echoed.
func TestPasskeyLoginVertical(t *testing.T) {
	api, repo, v := seedLoginPasskeyAPI(t)
	eh := api.ExternalHandler()

	// 1) begin: options FLAT (envelope stripped), exactly one login-purpose challenge stashed for u1, and
	// the account's bound credential handed to the verifier (so the authenticator can be
	// asked to assert with a known key).
	w := do(eh, "POST", "/api/v1/auth/passkey/login/begin", `{"email":"player@example.net"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("begin: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if b := acctBody(t, w); b["challenge"] == nil || b["publicKey"] != nil {
		t.Errorf("begin must return FLAT assertion options (top-level challenge, no publicKey envelope), got %s", w.Body.String())
	}
	if v.lastUser.ID != "u1" {
		t.Errorf("begin passed user id %q, want u1", v.lastUser.ID)
	}
	if len(v.lastUser.Credentials) != 1 || v.lastUser.Credentials[0].CredentialID != "cred-1" {
		t.Errorf("begin must hand the account's bound credential to the verifier, got %+v", v.lastUser.Credentials)
	}
	if len(repo.passkeyChallenges) != 1 {
		t.Fatalf("begin must stash exactly one challenge, got %d", len(repo.passkeyChallenges))
	}
	for _, c := range repo.passkeyChallenges {
		if c.userID != "u1" || c.purpose != passkeyPurposeLogin {
			t.Errorf("stashed challenge = %+v, want user u1 purpose %q", c, passkeyPurposeLogin)
		}
	}

	// 2) finish — typed in yet another casing, proving the finish-side resolver is
	// case-insensitive too — verifies the assertion and mints the session. The body
	// carries NO challenge, only email+assertion.
	w = do(eh, "POST", "/api/v1/auth/passkey/login/finish",
		`{"email":"PLAYER@example.NET","assertion":{"id":"cred-1","type":"public-key"}}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("finish: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	// THE security assertion: the blob the verifier saw at finish is exactly what begin
	// stashed — it travelled store-stash → consume, never the client.
	if !bytes.Equal(v.lastSession, []byte("login-session:u1")) {
		t.Fatalf("finish session data = %q, want the server-stashed %q (challenge must not be client-echoed)",
			v.lastSession, "login-session:u1")
	}
	vb := acctBody(t, w)
	if vb["user_id"] != "u1" || vb["role"] != "user" {
		t.Fatalf("finish body = %v, want user_id:u1 role:user", vb)
	}
	// The host-only HttpOnly cookie is the whole point — same contract as the email door.
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName || cookies[0].Value == "" {
		t.Fatalf("want one non-empty %s cookie, got %v", sessionCookieName, cookies)
	}
	s, ok := repo.sessions[hashCookie(cookies[0].Value)]
	if !ok {
		t.Fatal("no session row for the issued cookie (must be stored hashed)")
	}
	if s.userID != "u1" {
		t.Errorf("session userID = %q, want u1", s.userID)
	}
	if want := frozenNow.Add(sessionTTL); !s.expiresAt.Equal(want) {
		t.Errorf("session expiresAt = %v, want now+sessionTTL = %v", s.expiresAt, want)
	}
	// Audited once, by the account's username (there is no principal yet); begin is silent.
	if n := len(repo.audits); n != 1 {
		t.Fatalf("want exactly 1 audit (passkey_login), got %d: %+v", n, repo.audits)
	}
	if repo.audits[0].Action != "auth.passkey_login" || repo.audits[0].Actor != "player" {
		t.Errorf("audit = %+v, want auth.passkey_login by player", repo.audits[0])
	}

	// 3) single-use: the consumed challenge buys nothing a second time.
	if w := do(eh, "POST", "/api/v1/auth/passkey/login/finish",
		`{"email":"player@example.net","assertion":{"id":"cred-1","type":"public-key"}}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_login_invalid" {
		t.Fatalf("replay of consumed challenge: code = %d body %s, want 400 passkey_login_invalid", w.Code, w.Body.String())
	}
}

// TestPasskeyLoginBeginNoPasskey pins the begin-side anti-enumeration floor: an unknown
// email and a KNOWN verified account that has enrolled no passkey answer the SAME
// no_passkey envelope, so the two are indistinguishable. (The remaining has-passkey-vs-not
// status split is the documented, accepted trade-off; the cooldown below makes probing
// it impractical.) Neither path stashes a challenge, and both KEEP the reservation.
func TestPasskeyLoginBeginNoPasskey(t *testing.T) {
	begin := func(eh http.Handler, email string) *httptest.ResponseRecorder {
		return do(eh, "POST", "/api/v1/auth/passkey/login/begin", `{"email":"`+email+`"}`, jsonHeader)
	}

	t.Run("unknown email and a passkey-less account answer the same no_passkey", func(t *testing.T) {
		// Unknown email: no account at all.
		apiU, repoU, _ := seedLoginPasskeyAPI(t)
		wGhost := begin(apiU.ExternalHandler(), "ghost@example.net")

		// Known verified account that has enrolled NO passkey.
		apiN, repoN, _ := seedLoginPasskeyAPI(t)
		delete(repoN.passkeyCreds, "row1")
		wNone := begin(apiN.ExternalHandler(), "player@example.net")

		if wGhost.Code != http.StatusBadRequest || wNone.Code != http.StatusBadRequest {
			t.Fatalf("codes = %d/%d, want 400/400", wGhost.Code, wNone.Code)
		}
		gc, gm := errEnvelope(t, wGhost)
		nc, nm := errEnvelope(t, wNone)
		if gc != "no_passkey" || gc != nc || gm != nm {
			t.Errorf("envelopes differ: unknown=(%s,%q) no-cred=(%s,%q) — must be identical no_passkey", gc, gm, nc, nm)
		}
		// Neither may stash a challenge or reach BeginLogin.
		if len(repoU.passkeyChallenges) != 0 || len(repoN.passkeyChallenges) != 0 {
			t.Errorf("no_passkey paths must stash nothing, got unknown=%d no-cred=%d",
				len(repoU.passkeyChallenges), len(repoN.passkeyChallenges))
		}
	})

	t.Run("the no_passkey path KEEPS the reservation so probing is throttled", func(t *testing.T) {
		api, _, _ := seedLoginPasskeyAPI(t)
		eh := api.ExternalHandler()
		if w := begin(eh, "ghost@example.net"); w.Code != http.StatusBadRequest || decodeErr(t, w) != "no_passkey" {
			t.Fatalf("first probe: code = %d body %s, want 400 no_passkey", w.Code, w.Body.String())
		}
		// Re-probing the same unknown address inside the window is throttled identically to
		// a real begin — the response is not the only channel; the throttle is sealed too.
		if w := begin(eh, "ghost@example.net"); w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "otp_resend_cooldown" {
			t.Fatalf("re-probe: code = %d body %s, want 429 otp_resend_cooldown", w.Code, w.Body.String())
		}
	})
}

// TestPasskeyLoginBeginVerifierError pins the reserve→rollback path: a BeginLogin failure
// is a server-side fault, not a probe signal, so it answers passkey_login_failed AND
// RELEASES the reservation — the immediate retry is admitted, not 429'd. Distinguishing
// 400-not-429 on the retry is what proves the release: a kept reservation would 429 before
// ever reaching BeginLogin.
func TestPasskeyLoginBeginVerifierError(t *testing.T) {
	api, repo, v := seedLoginPasskeyAPI(t)
	v.beginLoginErr = errors.New("no assertable credential")
	eh := api.ExternalHandler()

	if w := do(eh, "POST", "/api/v1/auth/passkey/login/begin", `{"email":"player@example.net"}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_login_failed" {
		t.Fatalf("verifier error: code = %d body %s, want 400 passkey_login_failed", w.Code, w.Body.String())
	}
	if len(repo.passkeyChallenges) != 0 {
		t.Errorf("a failed begin must stash no challenge, got %d", len(repo.passkeyChallenges))
	}
	if w := do(eh, "POST", "/api/v1/auth/passkey/login/begin", `{"email":"player@example.net"}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_login_failed" {
		t.Fatalf("retry after verifier error: code = %d body %s, want 400 passkey_login_failed (reservation must be released, not 429)", w.Code, w.Body.String())
	}
}

// TestPasskeyLoginGates covers the shared front doors of both halves: the fail-closed
// local-auth toggle, graceful degradation when no verifier is wired, the CSRF Content-Type
// guard (these are Public, credential-minting routes), and the input gates that must
// reject before any lookup or stash.
func TestPasskeyLoginGates(t *testing.T) {
	const beginPath = "/api/v1/auth/passkey/login/begin"
	const finishPath = "/api/v1/auth/passkey/login/finish"
	const goodBegin = `{"email":"player@example.net"}`
	const goodFinish = `{"email":"player@example.net","assertion":{"id":"cred-1"}}`

	t.Run("local auth disabled -> 403 on both halves", func(t *testing.T) {
		api := newTestAPI(newFakeRepo(), newFakeCluster()) // no LocalAuthEnabledKey: fails closed
		api.Passkey = &fakePasskeyVerifier{}
		eh := api.ExternalHandler()
		if w := do(eh, "POST", beginPath, goodBegin, jsonHeader); w.Code != http.StatusForbidden || decodeErr(t, w) != "local_auth_disabled" {
			t.Errorf("begin: code = %d body %s, want 403 local_auth_disabled", w.Code, w.Body.String())
		}
		if w := do(eh, "POST", finishPath, goodFinish, jsonHeader); w.Code != http.StatusForbidden || decodeErr(t, w) != "local_auth_disabled" {
			t.Errorf("finish: code = %d body %s, want 403 local_auth_disabled", w.Code, w.Body.String())
		}
	})

	t.Run("no verifier wired -> 503 passkey_unavailable on both halves", func(t *testing.T) {
		api, _, _ := seedLoginPasskeyAPI(t)
		api.Passkey = nil // unwire it: the degraded path must be a clean 503, not a panic
		eh := api.ExternalHandler()
		if w := do(eh, "POST", beginPath, goodBegin, jsonHeader); w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "passkey_unavailable" {
			t.Errorf("begin: code = %d body %s, want 503 passkey_unavailable", w.Code, w.Body.String())
		}
		if w := do(eh, "POST", finishPath, goodFinish, jsonHeader); w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "passkey_unavailable" {
			t.Errorf("finish: code = %d body %s, want 503 passkey_unavailable", w.Code, w.Body.String())
		}
	})

	t.Run("non-JSON content type -> 415 on both halves", func(t *testing.T) {
		api, _, _ := seedLoginPasskeyAPI(t)
		eh := api.ExternalHandler()
		for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
			if w := do(eh, "POST", beginPath, goodBegin, ctHeader(ct)); w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("begin with Content-Type %q: code = %d, want 415", ct, w.Code)
			}
			if w := do(eh, "POST", finishPath, goodFinish, ctHeader(ct)); w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("finish with Content-Type %q: code = %d, want 415", ct, w.Code)
			}
		}
	})

	t.Run("begin bad email -> 400, nothing stashed", func(t *testing.T) {
		bad := map[string]string{
			"missing email": `{}`,
			"empty email":   `{"email":""}`,
			"no at-sign":    `{"email":"notanemail"}`,
			"two at-signs":  `{"email":"a@b@example.net"}`,
			"unknown field": `{"email":"a@example.net","x":1}`,
		}
		for name, body := range bad {
			api, repo, _ := seedLoginPasskeyAPI(t)
			w := do(api.ExternalHandler(), "POST", beginPath, body, jsonHeader)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: code = %d, want 400 (%s)", name, w.Code, w.Body.String())
			}
			if len(repo.passkeyChallenges) != 0 {
				t.Errorf("%s: a rejected begin must stash nothing (%d)", name, len(repo.passkeyChallenges))
			}
		}
	})

	t.Run("finish bad inputs -> 400", func(t *testing.T) {
		// An assertion key of {} is non-empty (2 bytes), so it clears the len==0 gate and
		// fails later at passkey_login_invalid — the "missing assertion" gate is only the
		// absent key. These are the cases the input gate itself must catch.
		cases := []struct{ name, body, wantCode string }{
			{"bad email", `{"email":"notanemail","assertion":{"id":"x"}}`, "bad_request"},
			{"missing assertion", `{"email":"player@example.net"}`, "bad_request"},
			{"unknown field", `{"email":"player@example.net","assertion":{"id":"x"},"z":1}`, ""},
		}
		for _, c := range cases {
			api, _, _ := seedLoginPasskeyAPI(t)
			w := do(api.ExternalHandler(), "POST", finishPath, c.body, jsonHeader)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: code = %d, want 400 (%s)", c.name, w.Code, w.Body.String())
			}
			if c.wantCode != "" && decodeErr(t, w) != c.wantCode {
				t.Errorf("%s: error code = %q, want %q", c.name, decodeErr(t, w), c.wantCode)
			}
		}
	})
}

// TestPasskeyLoginFinishRejections is the redeem-side failure matrix and the anchor for
// anti-enumeration: an unknown email, a known account with no live challenge, an expired
// challenge and an assertion that fails verification must ALL answer the byte-identical
// passkey_login_invalid envelope (code AND message) and mint no session — so the finish
// half never doubles as an existence or ceremony-state oracle. Expired state is planted
// directly: the frozen clock makes that the only deterministic route to that branch.
func TestPasskeyLoginFinishRejections(t *testing.T) {
	const finishPath = "/api/v1/auth/passkey/login/finish"
	finish := func(eh http.Handler, email string) *httptest.ResponseRecorder {
		return do(eh, "POST", finishPath,
			`{"email":"`+email+`","assertion":{"id":"cred-1","type":"public-key"}}`, jsonHeader)
	}

	cases := []struct {
		name  string
		email string
		setup func(repo *fakeRepo, v *fakePasskeyVerifier)
	}{
		{"unknown email", "ghost@example.net", func(repo *fakeRepo, v *fakePasskeyVerifier) {}},
		{"known account, no live challenge", "player@example.net", func(repo *fakeRepo, v *fakePasskeyVerifier) {}},
		{"expired challenge", "player@example.net", func(repo *fakeRepo, v *fakePasskeyVerifier) {
			plantLoginChallenge(repo, "ex", frozenNow.Add(-time.Second))
		}},
		{"assertion fails verification", "player@example.net", func(repo *fakeRepo, v *fakePasskeyVerifier) {
			plantLoginChallenge(repo, "live", frozenNow.Add(passkeyChallengeTTL))
			v.failErr = errors.New("bad assertion")
		}},
	}

	var envelopes [][2]string
	for _, c := range cases {
		api, repo, v := seedLoginPasskeyAPI(t)
		c.setup(repo, v)
		w := finish(api.ExternalHandler(), c.email)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d, want 400 (%s)", c.name, w.Code, w.Body.String())
		}
		code, msg := errEnvelope(t, w)
		if code != "passkey_login_invalid" {
			t.Errorf("%s: error code = %q, want passkey_login_invalid", c.name, code)
		}
		if len(repo.sessions) != 0 {
			t.Errorf("%s: a rejected finish must mint no session (got %d)", c.name, len(repo.sessions))
		}
		if len(w.Result().Cookies()) != 0 {
			t.Errorf("%s: a rejected finish must set no cookie", c.name)
		}
		envelopes = append(envelopes, [2]string{code, msg})
	}
	// The anchor: every envelope is identical (code AND message), so no branch is
	// distinguishable from another.
	for i := 1; i < len(envelopes); i++ {
		if envelopes[i] != envelopes[0] {
			t.Errorf("envelope for %q %v differs from %q %v — all rejections must be identical",
				cases[i].name, envelopes[i], cases[0].name, envelopes[0])
		}
	}
}

// TestPasskeyLoginBeginRateLimited closes the unauthenticated probing/DoS vector on the
// public door: one begin per recipient per window, keyed case-insensitively (a recased
// retype is the same mailbox), recovering after the window elapses. This is what makes the
// accepted has-passkey-vs-not status oracle impractical to farm.
func TestPasskeyLoginBeginRateLimited(t *testing.T) {
	begin := func(eh http.Handler, email string) *httptest.ResponseRecorder {
		return do(eh, "POST", "/api/v1/auth/passkey/login/begin", `{"email":"`+email+`"}`, jsonHeader)
	}

	t.Run("same recipient is throttled, then recovers after the cooldown", func(t *testing.T) {
		api, _, _ := seedLoginPasskeyAPI(t)
		clock := frozenNow
		api.Now = func() time.Time { return clock }
		eh := api.ExternalHandler()

		if w := begin(eh, "player@example.net"); w.Code != http.StatusOK {
			t.Fatalf("first begin: code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		if w := begin(eh, "player@example.net"); w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "otp_resend_cooldown" {
			t.Fatalf("immediate re-begin: code = %d body %s, want 429 otp_resend_cooldown", w.Code, w.Body.String())
		}
		clock = clock.Add(otpResendCooldown + time.Second)
		if w := begin(eh, "player@example.net"); w.Code != http.StatusOK {
			t.Fatalf("post-cooldown begin: code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("throttle key is case-insensitive", func(t *testing.T) {
		api, _, _ := seedLoginPasskeyAPI(t)
		eh := api.ExternalHandler()
		if w := begin(eh, "Player@Example.NET"); w.Code != http.StatusOK {
			t.Fatalf("first begin: code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		if w := begin(eh, "player@example.net"); w.Code != http.StatusTooManyRequests {
			t.Fatalf("recased re-begin: code = %d, want 429 (key must be lowercased)", w.Code)
		}
	})
}

// TestPasskeyLoginAllowsStaff pins the deliberate contrast with the email door: that door
// refuses role != user with 403 staff_account (op.console keeps its Zero-Trust in-game
// gate), but the passkey door ADMITS staff — a passkey is a strong two-factor authenticator
// (possession + user-verification), enough to stand alone. This guards against a future
// "make the doors consistent" change silently locking admins out of passkey login.
func TestPasskeyLoginAllowsStaff(t *testing.T) {
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["boss"] = &StaffUser{
		ID: "a1", Username: "boss", Email: "boss@example.net",
		Role: "admin", EmailVerified: true,
	}
	repo.passkeyCreds["row1"] = PasskeyCredential{
		ID: "row1", UserID: "a1", CredentialID: "cred-a1", PublicKey: "k", CreatedAt: frozenNow,
	}
	v := &fakePasskeyVerifier{
		options:   json.RawMessage(`{"publicKey":{"challenge":"YXNzZXJ0"}}`),
		assertion: VerifiedAssertion{CredentialID: "cred-a1", UserVerified: true},
	}
	api := newTestAPI(repo, newFakeCluster())
	api.Passkey = v
	eh := api.ExternalHandler()

	if w := do(eh, "POST", "/api/v1/auth/passkey/login/begin", `{"email":"boss@example.net"}`, jsonHeader); w.Code != http.StatusOK {
		t.Fatalf("begin for staff: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	w := do(eh, "POST", "/api/v1/auth/passkey/login/finish",
		`{"email":"boss@example.net","assertion":{"id":"cred-a1","type":"public-key"}}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("finish for staff: code = %d, want 200 (passkey admits staff) (%s)", w.Code, w.Body.String())
	}
	if b := acctBody(t, w); b["user_id"] != "a1" || b["role"] != "admin" {
		t.Fatalf("finish body = %v, want user_id:a1 role:admin", b)
	}
	if len(repo.sessions) != 1 {
		t.Errorf("a staff passkey login must mint a session, got %d", len(repo.sessions))
	}
}

// TestPasskeyLoginFaceSeparation enforces that both halves are web-only: the internal
// (service-token) face must 404 them, never serve them.
func TestPasskeyLoginFaceSeparation(t *testing.T) {
	api, _, _ := seedLoginPasskeyAPI(t)
	ih := api.InternalHandler()
	if w := do(ih, "POST", "/api/v1/auth/passkey/login/begin", `{"email":"player@example.net"}`, jsonHeader); w.Code != http.StatusNotFound {
		t.Errorf("begin on internal face: code = %d, want 404", w.Code)
	}
	if w := do(ih, "POST", "/api/v1/auth/passkey/login/finish", `{"email":"player@example.net","assertion":{"id":"x"}}`, jsonHeader); w.Code != http.StatusNotFound {
		t.Errorf("finish on internal face: code = %d, want 404", w.Code)
	}
}

// TestPasskeyLoginFinishCloneRejected is the username-first mirror of the discoverable door's
// clone refusal (task #40 item 5). Both doors share applyAssertionCounter, but each WIRES it
// independently, so this proves the username-first finish also fails closed on a CloneWarning:
// the same opaque passkey_login_invalid envelope (no clone oracle), no session minted, the stored
// credential left untouched at its enrollment-time counter, and a distinct auth.passkey_clone_
// rejected audit under the account. Challenge is planted directly so finish is reachable under
// the frozen clock without a live begin.
func TestPasskeyLoginFinishCloneRejected(t *testing.T) {
	api, repo, v := seedLoginPasskeyAPI(t)
	v.assertion = VerifiedAssertion{CredentialID: "cred-1", UserVerified: true, SignCount: 3, CloneWarning: true}
	plantLoginChallenge(repo, "live", frozenNow.Add(passkeyChallengeTTL))
	eh := api.ExternalHandler()

	w := do(eh, "POST", "/api/v1/auth/passkey/login/finish",
		`{"email":"player@example.net","assertion":{"id":"cred-1","type":"public-key"}}`, jsonHeader)

	if w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_login_invalid" {
		t.Fatalf("clone finish: code = %d body %s, want 400 passkey_login_invalid (opaque refusal)", w.Code, w.Body.String())
	}
	if len(repo.sessions) != 0 {
		t.Errorf("clone refusal must mint no session, got %d", len(repo.sessions))
	}
	if len(w.Result().Cookies()) != 0 {
		t.Errorf("clone refusal must set no session cookie, got %v", w.Result().Cookies())
	}
	if got := repo.passkeyCreds["row1"]; got.SignCount != 0 || got.LastUsedAt != nil {
		t.Errorf("clone refusal must not advance/stamp the credential, got SignCount=%d LastUsedAt=%v", got.SignCount, got.LastUsedAt)
	}
	if n := len(repo.audits); n != 1 || repo.audits[0].Action != "auth.passkey_clone_rejected" {
		t.Fatalf("want exactly 1 auth.passkey_clone_rejected audit, got %+v", repo.audits)
	}
	if repo.audits[0].Actor != "player" {
		t.Errorf("clone audit actor = %q, want player (the resolved account)", repo.audits[0].Actor)
	}
}
