package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Pre-session DISCOVERABLE ("usernameless") passkey login tests (spec §14, task #40 — the
// truly from-zero console.<root_domain> door). These drive the two Public routes against the
// fakeRepo's non-user-keyed challenge store and a fake PasskeyVerifier, so what they PROVE is
// the handler + login state machine (opaque-handle stash → consume → userHandle-resolve →
// session mint), NOT the pgrepo SQL nor the cryptographic assertion verification (the latter is
// the parity subject of internal/passkey/verifier_test.go's TestDiscoverableLoginRoundTrip).
// The load-bearing properties, in flow order:
//
//   - No identifier crosses the wire: begin has no request body and finish carries only the
//     opaque login_id + the assertion. The account is revealed solely by the userHandle the
//     verifier surfaces, resolved server-side via UserByID — never anything the client names.
//   - Session-data round-trip: the stashed SessionData reaches FinishDiscoverableLogin only via
//     store-stash → consume (the finish body has no session data), so it is never client-echoed.
//   - Fail-closed anti-enumeration on finish: a bogus/expired/consumed handle, a failed
//     assertion, AND a userHandle that resolves to no account all collapse to ONE
//     passkey_login_invalid envelope — finish is never an existence/state oracle.
//   - Volumetric bound: begin has no per-caller identity to rate-limit (that is delegated to the
//     edge), so the server-side guard is the hard global cap → 429 too_many_challenges.

// seedDiscoverableLoginAPI wires the public from-zero door: local sessions enabled, a single
// verified player "player" (id u1) with one bound passkey, and a verifier primed with fixed
// options, a verified assertion, and a discoverableUserHandle of "u1" — so the default resolve
// path surfaces that account exactly as a real resident credential's userHandle would. Both
// routes are Public (no External principal), proving they are truly pre-session.
func seedDiscoverableLoginAPI(t *testing.T) (*API, *fakeRepo, *fakePasskeyVerifier) {
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
		options:                json.RawMessage(`{"publicKey":{"challenge":"ZGlzYw"}}`),
		assertion:              VerifiedAssertion{CredentialID: "cred-1", UserVerified: true},
		discoverableUserHandle: []byte("u1"),
	}
	api := newTestAPI(repo, newFakeCluster())
	api.Passkey = v
	return api, repo, v
}

// TestPasskeyDiscoverableLoginVertical walks the whole from-zero slice across the external face:
// begin stashes one challenge under an opaque login_id and returns the assertion options with
// that handle merged in; finish consumes the handle, verifies the assertion against the
// SERVER-STASHED session data, resolves the account from the authenticator-revealed userHandle
// (NOT from anything typed), and mints the same host-only felis_session as the other doors. The
// decisive assertion is the session-data round-trip: the finish body carries only login_id +
// assertion, so the only path for the stashed blob into FinishDiscoverableLogin is store-stash →
// consume — the challenge is never client-echoed.
func TestPasskeyDiscoverableLoginVertical(t *testing.T) {
	api, repo, v := seedDiscoverableLoginAPI(t)
	eh := api.ExternalHandler()

	// 1) begin: usernameless — no request body by design (the caller supplies no identifier),
	// only the JSON Content-Type CSRF guard. The response is one {"publicKey":{...},"login_id":
	// "..."} document, and exactly one challenge is stashed, keyed by the returned handle.
	w := do(eh, "POST", "/api/v1/auth/passkey/login/discoverable/begin", `{}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("begin: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	body := acctBody(t, w)
	if body["publicKey"] == nil {
		t.Errorf("begin must return the assertion options verbatim, got %s", w.Body.String())
	}
	loginID, _ := body["login_id"].(string)
	if loginID == "" {
		t.Fatalf("begin must return a non-empty login_id, got %s", w.Body.String())
	}
	if len(repo.discoverableChallenges) != 1 {
		t.Fatalf("begin must stash exactly one discoverable challenge, got %d", len(repo.discoverableChallenges))
	}
	if _, ok := repo.discoverableChallenges[loginID]; !ok {
		t.Errorf("the stashed challenge must be keyed by the returned login_id %q", loginID)
	}

	// 2) finish: the body carries ONLY the login_id and the assertion — no identifier and no
	// session data. The account is revealed by the userHandle the verifier surfaces (u1).
	w = do(eh, "POST", "/api/v1/auth/passkey/login/discoverable/finish",
		`{"login_id":"`+loginID+`","assertion":{"id":"cred-1","type":"public-key"}}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("finish: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	// THE security assertion: the session data the verifier saw is exactly what begin stashed —
	// it travelled store-stash → consume, never the client (the finish body has no session data).
	if !bytes.Equal(v.lastSession, []byte("disc-session")) {
		t.Fatalf("finish session data = %q, want the server-stashed %q (challenge must not be client-echoed)",
			v.lastSession, "disc-session")
	}
	vb := acctBody(t, w)
	if vb["user_id"] != "u1" || vb["role"] != "user" {
		t.Fatalf("finish body = %v, want user_id:u1 role:user", vb)
	}
	// The host-only HttpOnly cookie is the whole point — same contract as the other doors.
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
	// Audited once, by the RESOLVED account's username (there is no principal yet); begin is silent.
	if n := len(repo.audits); n != 1 {
		t.Fatalf("want exactly 1 audit (passkey_login_discoverable), got %d: %+v", n, repo.audits)
	}
	if repo.audits[0].Action != "auth.passkey_login_discoverable" || repo.audits[0].Actor != "player" {
		t.Errorf("audit = %+v, want auth.passkey_login_discoverable by player", repo.audits[0])
	}

	// 3) single-use: the consumed login_id buys nothing a second time.
	if w := do(eh, "POST", "/api/v1/auth/passkey/login/discoverable/finish",
		`{"login_id":"`+loginID+`","assertion":{"id":"cred-1","type":"public-key"}}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_login_invalid" {
		t.Fatalf("replay of consumed login_id: code = %d body %s, want 400 passkey_login_invalid", w.Code, w.Body.String())
	}
}

// TestPasskeyDiscoverableLoginBeginCapped pins the store-wide bound: with the store at its hard
// cap, begin answers 429 too_many_challenges and stashes nothing. A reap alone cannot bound a
// burst, since freshly-inserted rows are not yet expired; the per-source bound below keeps one
// network from reaching this cap.
func TestPasskeyDiscoverableLoginBeginCapped(t *testing.T) {
	api, repo, _ := seedDiscoverableLoginAPI(t)
	repo.discoverableFull = true
	eh := api.ExternalHandler()

	w := do(eh, "POST", "/api/v1/auth/passkey/login/discoverable/begin", `{}`, jsonHeader)
	if w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "too_many_challenges" {
		t.Fatalf("capped begin: code = %d body %s, want 429 too_many_challenges", w.Code, w.Body.String())
	}
	if len(repo.discoverableChallenges) != 0 {
		t.Errorf("a capped begin must stash nothing, got %d", len(repo.discoverableChallenges))
	}
}

// TestPasskeyDiscoverableLoginBeginPerSourceCap: the usernameless begin has no account to key
// on, so the store holds each network (IPv4 address, IPv6 /48) to 32 live challenges. The
// begins come from 33 different /64s inside one /48, so the per-/64 auth-door bucket never
// trips and only the /48 bound refuses the last; another network still begins.
func TestPasskeyDiscoverableLoginBeginPerSourceCap(t *testing.T) {
	api, repo, _ := seedDiscoverableLoginAPI(t)
	api.ClientIPHeader = "CF-Connecting-IP"
	eh := api.ExternalHandler()
	from := func(ip string) *httptest.ResponseRecorder {
		return do(eh, "POST", "/api/v1/auth/passkey/login/discoverable/begin", `{}`,
			map[string]string{"Content-Type": "application/json", "CF-Connecting-IP": ip})
	}
	for i := 1; i <= 32; i++ {
		if w := from(fmt.Sprintf("2001:db8:7:%x::1", i)); w.Code != http.StatusOK {
			t.Fatalf("begin %d: code = %d, want 200 (%s)", i, w.Code, w.Body.String())
		}
	}
	w := from("2001:db8:7:ff::1")
	if w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "too_many_challenges" {
		t.Fatalf("33rd begin from the /48: code = %d body %s, want 429 too_many_challenges", w.Code, w.Body.String())
	}
	if len(repo.discoverableChallenges) != 32 {
		t.Errorf("stashed = %d, want 32", len(repo.discoverableChallenges))
	}
	if w := from("2001:db8:8::1"); w.Code != http.StatusOK {
		t.Fatalf("begin from another /48: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// TestPasskeyDiscoverableLoginBeginVerifierError pins the verifier-fault path: a
// BeginDiscoverableLogin failure is a server-side fault, answered passkey_login_failed, and
// stashes no challenge (there is nothing to stash — the ceremony never started).
func TestPasskeyDiscoverableLoginBeginVerifierError(t *testing.T) {
	api, repo, v := seedDiscoverableLoginAPI(t)
	v.beginLoginErr = errors.New("cannot begin discoverable")
	eh := api.ExternalHandler()

	w := do(eh, "POST", "/api/v1/auth/passkey/login/discoverable/begin", `{}`, jsonHeader)
	if w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_login_failed" {
		t.Fatalf("verifier error: code = %d body %s, want 400 passkey_login_failed", w.Code, w.Body.String())
	}
	if len(repo.discoverableChallenges) != 0 {
		t.Errorf("a failed begin must stash nothing, got %d", len(repo.discoverableChallenges))
	}
}

// TestPasskeyDiscoverableLoginGates covers the shared front doors of both halves: the
// fail-closed local-auth toggle, graceful degradation when no verifier is wired, the CSRF
// Content-Type guard (these are Public, credential-minting routes), and the finish input gates
// that must reject before any consume or resolve.
func TestPasskeyDiscoverableLoginGates(t *testing.T) {
	const beginPath = "/api/v1/auth/passkey/login/discoverable/begin"
	const finishPath = "/api/v1/auth/passkey/login/discoverable/finish"
	const goodFinish = `{"login_id":"x","assertion":{"id":"cred-1"}}`

	t.Run("local auth disabled -> 403 on both halves", func(t *testing.T) {
		api := newTestAPI(newFakeRepo(), newFakeCluster()) // no LocalAuthEnabledKey: fails closed
		api.Passkey = &fakePasskeyVerifier{}
		eh := api.ExternalHandler()
		if w := do(eh, "POST", beginPath, `{}`, jsonHeader); w.Code != http.StatusForbidden || decodeErr(t, w) != "local_auth_disabled" {
			t.Errorf("begin: code = %d body %s, want 403 local_auth_disabled", w.Code, w.Body.String())
		}
		if w := do(eh, "POST", finishPath, goodFinish, jsonHeader); w.Code != http.StatusForbidden || decodeErr(t, w) != "local_auth_disabled" {
			t.Errorf("finish: code = %d body %s, want 403 local_auth_disabled", w.Code, w.Body.String())
		}
	})

	t.Run("no verifier wired -> 503 passkey_unavailable on both halves", func(t *testing.T) {
		api, _, _ := seedDiscoverableLoginAPI(t)
		api.Passkey = nil // unwire it: the degraded path must be a clean 503, not a panic
		eh := api.ExternalHandler()
		if w := do(eh, "POST", beginPath, `{}`, jsonHeader); w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "passkey_unavailable" {
			t.Errorf("begin: code = %d body %s, want 503 passkey_unavailable", w.Code, w.Body.String())
		}
		if w := do(eh, "POST", finishPath, goodFinish, jsonHeader); w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "passkey_unavailable" {
			t.Errorf("finish: code = %d body %s, want 503 passkey_unavailable", w.Code, w.Body.String())
		}
	})

	t.Run("non-JSON content type -> 415 on both halves", func(t *testing.T) {
		api, _, _ := seedDiscoverableLoginAPI(t)
		eh := api.ExternalHandler()
		for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
			if w := do(eh, "POST", beginPath, `{}`, ctHeader(ct)); w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("begin with Content-Type %q: code = %d, want 415", ct, w.Code)
			}
			if w := do(eh, "POST", finishPath, goodFinish, ctHeader(ct)); w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("finish with Content-Type %q: code = %d, want 415", ct, w.Code)
			}
		}
	})

	t.Run("finish missing inputs -> 400 bad_request, nothing minted", func(t *testing.T) {
		cases := map[string]string{
			"missing login_id":  `{"assertion":{"id":"cred-1"}}`,
			"empty login_id":    `{"login_id":"","assertion":{"id":"cred-1"}}`,
			"missing assertion": `{"login_id":"x"}`,
		}
		for name, body := range cases {
			api, repo, _ := seedDiscoverableLoginAPI(t)
			w := do(api.ExternalHandler(), "POST", finishPath, body, jsonHeader)
			if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
				t.Errorf("%s: code = %d body %s, want 400 bad_request", name, w.Code, w.Body.String())
			}
			if len(repo.sessions) != 0 {
				t.Errorf("%s: a rejected finish must mint no session (%d)", name, len(repo.sessions))
			}
		}
	})
}

// TestPasskeyDiscoverableLoginFinishRejections is the redeem-side failure matrix and the anchor
// for from-zero anti-enumeration: a bogus handle, an expired challenge, an assertion that fails
// verification, AND a userHandle that resolves to no account must ALL answer the byte-identical
// passkey_login_invalid envelope (code AND message) and mint no session. The last case is the
// one unique to this door — the account is chosen by the authenticator, so an unresolvable
// handle must fail exactly like a bad signature, never leaking that the handle was well-formed.
func TestPasskeyDiscoverableLoginFinishRejections(t *testing.T) {
	const finishPath = "/api/v1/auth/passkey/login/discoverable/finish"
	finish := func(eh http.Handler, loginID string) *httptest.ResponseRecorder {
		return do(eh, "POST", finishPath,
			`{"login_id":"`+loginID+`","assertion":{"id":"cred-1","type":"public-key"}}`, jsonHeader)
	}

	cases := []struct {
		name    string
		loginID string
		setup   func(repo *fakeRepo, v *fakePasskeyVerifier)
	}{
		{"no live challenge", "ghost", func(repo *fakeRepo, v *fakePasskeyVerifier) {}},
		{"expired challenge", "ex", func(repo *fakeRepo, v *fakePasskeyVerifier) {
			repo.discoverableChallenges["ex"] = &fakeDiscoverableChallenge{
				sessionData: []byte("disc-session"), expiresAt: frozenNow.Add(-time.Second),
			}
		}},
		{"assertion fails verification", "live", func(repo *fakeRepo, v *fakePasskeyVerifier) {
			repo.discoverableChallenges["live"] = &fakeDiscoverableChallenge{
				sessionData: []byte("disc-session"), expiresAt: frozenNow.Add(passkeyChallengeTTL),
			}
			v.failErr = errors.New("bad assertion")
		}},
		{"userHandle resolves to no account", "live", func(repo *fakeRepo, v *fakePasskeyVerifier) {
			repo.discoverableChallenges["live"] = &fakeDiscoverableChallenge{
				sessionData: []byte("disc-session"), expiresAt: frozenNow.Add(passkeyChallengeTTL),
			}
			v.discoverableUserHandle = []byte("nonexistent")
		}},
	}

	var envelopes [][2]string
	for _, c := range cases {
		api, repo, v := seedDiscoverableLoginAPI(t)
		c.setup(repo, v)
		w := finish(api.ExternalHandler(), c.loginID)
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
	// The anchor: every envelope is identical (code AND message), so no branch — including the
	// unresolvable-handle branch — is distinguishable from another.
	for i := 1; i < len(envelopes); i++ {
		if envelopes[i] != envelopes[0] {
			t.Errorf("envelope for %q %v differs from %q %v — all rejections must be identical",
				cases[i].name, envelopes[i], cases[0].name, envelopes[0])
		}
	}
}

// TestPasskeyDiscoverableLoginFaceSeparation enforces that both halves are web-only: the
// internal (service-token) face must 404 them, never serve them.
func TestPasskeyDiscoverableLoginFaceSeparation(t *testing.T) {
	api, _, _ := seedDiscoverableLoginAPI(t)
	ih := api.InternalHandler()
	if w := do(ih, "POST", "/api/v1/auth/passkey/login/discoverable/begin", `{}`, jsonHeader); w.Code != http.StatusNotFound {
		t.Errorf("begin on internal face: code = %d, want 404", w.Code)
	}
	if w := do(ih, "POST", "/api/v1/auth/passkey/login/discoverable/finish", `{"login_id":"x","assertion":{"id":"y"}}`, jsonHeader); w.Code != http.StatusNotFound {
		t.Errorf("finish on internal face: code = %d, want 404", w.Code)
	}
}

// beginDiscoverableLogin runs the from-zero begin and returns the stashed login_id, so the
// counter/clone tests below need not re-inline the begin ceremony each time.
func beginDiscoverableLogin(t *testing.T, eh http.Handler) string {
	t.Helper()
	w := do(eh, "POST", "/api/v1/auth/passkey/login/discoverable/begin", `{}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("begin: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	loginID, _ := acctBody(t, w)["login_id"].(string)
	if loginID == "" {
		t.Fatalf("begin returned empty login_id: %s", w.Body.String())
	}
	return loginID
}

// TestPasskeyDiscoverableLoginAdvancesSignCount proves the success half of task #40 item 5: a
// verified from-zero assertion advances the stored signature counter to the value the
// authenticator reported and stamps last_used_at. Without this the stored counter would sit at
// the enrollment-time 0 forever, leaving the clone check below no moving baseline to judge a
// later regression against.
func TestPasskeyDiscoverableLoginAdvancesSignCount(t *testing.T) {
	api, repo, v := seedDiscoverableLoginAPI(t)
	// A clean (non-clone) assertion reporting an advanced counter.
	v.assertion = VerifiedAssertion{CredentialID: "cred-1", UserVerified: true, SignCount: 42}
	eh := api.ExternalHandler()

	loginID := beginDiscoverableLogin(t, eh)
	w := do(eh, "POST", "/api/v1/auth/passkey/login/discoverable/finish",
		`{"login_id":"`+loginID+`","assertion":{"id":"cred-1","type":"public-key"}}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("finish: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	got := repo.passkeyCreds["row1"]
	if got.SignCount != 42 {
		t.Errorf("stored SignCount = %d, want 42 (advanced to the asserted counter)", got.SignCount)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(frozenNow) {
		t.Errorf("stored LastUsedAt = %v, want %v (stamped on a successful assertion)", got.LastUsedAt, frozenNow)
	}
}

// TestPasskeyDiscoverableLoginCloneRejected proves the fail-closed half of task #40 item 5: a
// verified assertion carrying a CloneWarning (signature-counter regression — a possible cloned
// authenticator) is refused. The refusal (1) collapses into the same passkey_login_invalid
// envelope as any other finish failure so a prober gets no clone oracle, (2) mints NO session,
// (3) does NOT advance or stamp the stored credential, and (4) is audited distinctly for the
// operator under the resolved account.
func TestPasskeyDiscoverableLoginCloneRejected(t *testing.T) {
	api, repo, v := seedDiscoverableLoginAPI(t)
	v.assertion = VerifiedAssertion{CredentialID: "cred-1", UserVerified: true, SignCount: 3, CloneWarning: true}
	eh := api.ExternalHandler()

	loginID := beginDiscoverableLogin(t, eh)
	w := do(eh, "POST", "/api/v1/auth/passkey/login/discoverable/finish",
		`{"login_id":"`+loginID+`","assertion":{"id":"cred-1","type":"public-key"}}`, jsonHeader)

	if w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_login_invalid" {
		t.Fatalf("clone finish: code = %d body %s, want 400 passkey_login_invalid (opaque refusal)", w.Code, w.Body.String())
	}
	if len(repo.sessions) != 0 {
		t.Errorf("clone refusal must mint no session, got %d", len(repo.sessions))
	}
	if len(w.Result().Cookies()) != 0 {
		t.Errorf("clone refusal must set no session cookie, got %v", w.Result().Cookies())
	}
	// The stored credential is untouched: still at enrollment-time counter 0, never stamped.
	if got := repo.passkeyCreds["row1"]; got.SignCount != 0 || got.LastUsedAt != nil {
		t.Errorf("clone refusal must not advance/stamp the credential, got SignCount=%d LastUsedAt=%v", got.SignCount, got.LastUsedAt)
	}
	// Audited distinctly, under the resolved account, so the operator sees the clone signal.
	if n := len(repo.audits); n != 1 || repo.audits[0].Action != "auth.passkey_clone_rejected" {
		t.Fatalf("want exactly 1 auth.passkey_clone_rejected audit, got %+v", repo.audits)
	}
	if repo.audits[0].Actor != "player" {
		t.Errorf("clone audit actor = %q, want player (the resolved account)", repo.audits[0].Actor)
	}
}
