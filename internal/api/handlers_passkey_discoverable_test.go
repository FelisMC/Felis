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

// seedDiscoverableLoginAPI supplies a resident-credential userHandle resolving to the seeded account.
func seedDiscoverableLoginAPI(t *testing.T) (*API, *fakeRepo, *fakePasskeyVerifier) {
	t.Helper()
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["player"] = &StaffUser{
		ID: "u1", Username: "player", Email: "Player@Example.NET",
		Role: "user", EmailVerified: true,
	}
	repo.passkeyCreds["row1"] = PasskeyCredential{
		ID: "row1", UserID: "u1", CredentialID: "cred-1", PublicKey: "k", UserVerified: true, CreatedAt: frozenNow,
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

func TestPasskeyDiscoverableLoginVertical(t *testing.T) {
	api, repo, v := seedDiscoverableLoginAPI(t)
	eh := api.ExternalHandler()

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

	if n := len(repo.audits); n != 1 || repo.audits[0].Action != "auth.passkey_clone_rejected" {
		t.Fatalf("want exactly 1 auth.passkey_clone_rejected audit, got %+v", repo.audits)
	}
	if repo.audits[0].Actor != "player" {
		t.Errorf("clone audit actor = %q, want player (the resolved account)", repo.audits[0].Actor)
	}
}
