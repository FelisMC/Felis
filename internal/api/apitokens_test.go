package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	tokenOpHost     = "op.console." + testRoot
	tokenPlayerHost = "console." + testRoot
)

type tokenFixture struct {
	api  *API
	repo *fakeRepo
}

func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.settings[APITokensEnabledKey] = []byte("true")
	repo.staff["owner"] = &StaffUser{ID: "owner1", Username: "owner", Email: "owner@" + testRoot, Role: "owner", EmailVerified: true}
	repo.staff["player"] = &StaffUser{ID: "u1", Username: "player", Email: "u1@example.net", Role: "user", EmailVerified: true}
	api := newTestAPI(repo, claimCluster())
	api.AdminHostname = tokenOpHost
	api.External = SessionAuth{Repo: repo, RootDomain: testRoot, AdminHostname: tokenOpHost, Now: api.now}
	for cookie, id := range map[string]string{"owner-session": "owner1", "player-session": "u1"} {
		repo.sessions[hashCookie(cookie)] = &fakeSession{userID: id, expiresAt: api.now().Add(time.Hour)}
	}
	return &tokenFixture{api, repo}
}

func (f *tokenFixture) withSession(method, host, path, body, cookie string) *httptest.ResponseRecorder {
	return do(f.api.ExternalHandler(), method, "https://"+host+path, body,
		map[string]string{"Content-Type": "application/json", "Cookie": sessionCookieName + "=" + cookie})
}

func (f *tokenFixture) withKey(method, host, path, body, key string, extra ...string) *httptest.ResponseRecorder {
	h := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + key}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return do(f.api.ExternalHandler(), method, "https://"+host+path, body, h)
}

func (f *tokenFixture) create(t *testing.T, host, cookie, body string) (key string, tok APIToken) {
	t.Helper()
	w := f.withSession("POST", host, "/api/v1/account/api-tokens", body, cookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}
	var got struct {
		APIToken
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Token, got.APIToken
}

func TestAPITokenLifecycle(t *testing.T) {
	f := newTokenFixture(t)
	key, tok := f.create(t, tokenPlayerHost, "player-session", `{"name":"ci"}`)
	if !strings.HasPrefix(key, "felis_pat_") || tok.Prefix != key[:18] {
		t.Fatalf("key %q prefix %q", key, tok.Prefix)
	}

	list := f.withSession("GET", tokenPlayerHost, "/api/v1/account/api-tokens", "", "player-session")
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), key) || !strings.Contains(list.Body.String(), tok.Prefix) {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}

	if w := f.withKey("GET", tokenPlayerHost, "/api/v1/me", "", key); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"user_id":"u1"`) {
		t.Fatalf("key /me = %d %s", w.Code, w.Body.String())
	}

	f.repo.linked["u1"], f.repo.quota["u1"], f.repo.claimOK["survival"] = true, true, true
	w := f.withKey("POST", tokenPlayerHost, "/api/v1/servers/survival/claim", "", key, "Origin", "https://elsewhere.example")
	if w.Code != http.StatusOK {
		t.Fatalf("key-only write from a foreign origin = %d %s", w.Code, w.Body.String())
	}
	last := f.repo.audits[len(f.repo.audits)-1]
	if last.Action != "claim" || last.Source != "api_key:"+tok.Prefix || last.Actor != "u1@example.net" {
		t.Fatalf("audit = %+v", last)
	}

	for _, req := range [][2]string{{"POST", "/api/v1/account/api-tokens"}, {"DELETE", "/api/v1/account/api-tokens/" + tok.ID}} {
		if w := f.withKey(req[0], tokenPlayerHost, req[1], `{"name":"more"}`, key); w.Code != http.StatusForbidden || decodeErr(t, w) != "session_required" {
			t.Fatalf("key managing keys %s = %d %s", req[0], w.Code, w.Body.String())
		}
	}

	both := f.withKey("GET", tokenPlayerHost, "/api/v1/me", "", key, "Cookie", sessionCookieName+"=player-session")
	if both.Code != http.StatusUnauthorized {
		t.Fatalf("key plus cookie = %d", both.Code)
	}

	if w := f.withSession("DELETE", tokenOpHost, "/api/v1/account/api-tokens/"+tok.ID, "", "owner-session"); w.Code != http.StatusNotFound {
		t.Fatalf("revoking another user's key = %d", w.Code)
	}
	if w := f.withSession("DELETE", tokenPlayerHost, "/api/v1/account/api-tokens/"+tok.ID, "", "player-session"); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", w.Code, w.Body.String())
	}
	if w := f.withKey("GET", tokenPlayerHost, "/api/v1/me", "", key); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key = %d", w.Code)
	}
}

func TestAPITokensSwitchedOff(t *testing.T) {
	f := newTokenFixture(t)
	key, _ := f.create(t, tokenPlayerHost, "player-session", `{"name":"ci"}`)

	get := f.withSession("GET", tokenOpHost, "/api/v1/settings/api-tokens", "", "owner-session")
	var view apiTokenSettingsView
	if err := json.Unmarshal(get.Body.Bytes(), &view); err != nil || !view.Enabled {
		t.Fatalf("settings = %d %s", get.Code, get.Body.String())
	}
	if w := f.withSession("PUT", tokenOpHost, "/api/v1/settings/api-tokens", `{"enabled":false,"revision":"stale"}`, "owner-session"); w.Code != http.StatusConflict {
		t.Fatalf("stale revision = %d", w.Code)
	}
	put := f.withSession("PUT", tokenOpHost, "/api/v1/settings/api-tokens", `{"enabled":false,"revision":"`+view.Revision+`"}`, "owner-session")
	if put.Code != http.StatusOK {
		t.Fatalf("switch off = %d %s", put.Code, put.Body.String())
	}

	if w := f.withKey("GET", tokenPlayerHost, "/api/v1/me", "", key); w.Code != http.StatusUnauthorized {
		t.Fatalf("key while switched off = %d", w.Code)
	}
	w := f.withSession("POST", tokenPlayerHost, "/api/v1/account/api-tokens", `{"name":"ci"}`, "player-session")
	if w.Code != http.StatusForbidden || decodeErr(t, w) != "api_tokens_disabled" {
		t.Fatalf("create while switched off = %d %s", w.Code, w.Body.String())
	}
}

func TestAPITokenLimit(t *testing.T) {
	f := newTokenFixture(t)
	for range maxAPITokensPerUser {
		f.create(t, tokenPlayerHost, "player-session", `{"name":"ci"}`)
	}
	w := f.withSession("POST", tokenPlayerHost, "/api/v1/account/api-tokens", `{"name":"ci"}`, "player-session")
	if w.Code != http.StatusConflict || decodeErr(t, w) != "api_token_limit" {
		t.Fatalf("over the limit = %d %s", w.Code, w.Body.String())
	}
}

func TestAdminAPIToken(t *testing.T) {
	f := newTokenFixture(t)
	f.repo.passkeyCreds["owner-key"] = PasskeyCredential{ID: "owner-key", UserID: "owner1", UserVerified: true}
	expiry := func(d time.Duration) string { return f.api.now().Add(d).Format(time.RFC3339) }
	adminBody := func(d time.Duration) string { return `{"name":"ops","admin":true,"expires_at":"` + expiry(d) + `"}` }

	if w := f.withSession("POST", tokenPlayerHost, "/api/v1/account/api-tokens", adminBody(24*time.Hour), "player-session"); w.Code != http.StatusForbidden {
		t.Fatalf("admin key for a player = %d", w.Code)
	}
	if w := f.withSession("POST", tokenOpHost, "/api/v1/account/api-tokens", adminBody(24*time.Hour), "owner-session"); w.Code != http.StatusForbidden || decodeErr(t, w) != "reauth_required" {
		t.Fatalf("admin key without reauth = %d %s", w.Code, w.Body.String())
	}
	f.repo.sessions[hashCookie("owner-session")].reauthAt = f.api.now()
	for _, body := range []string{`{"name":"ops","admin":true}`, adminBody(91 * 24 * time.Hour)} {
		if w := f.withSession("POST", tokenOpHost, "/api/v1/account/api-tokens", body, "owner-session"); w.Code != http.StatusBadRequest {
			t.Fatalf("admin key %s = %d", body, w.Code)
		}
	}

	key, _ := f.create(t, tokenOpHost, "owner-session", adminBody(30*24*time.Hour))
	if w := f.withKey("GET", tokenOpHost, "/api/v1/settings/api-tokens", "", key); w.Code != http.StatusOK {
		t.Fatalf("admin key on the operator console = %d %s", w.Code, w.Body.String())
	}
	if w := f.withKey("GET", tokenPlayerHost, "/api/v1/settings/api-tokens", "", key); w.Code != http.StatusForbidden {
		t.Fatalf("admin key on the player console = %d", w.Code)
	}
	if w := f.withKey("PUT", tokenOpHost, "/api/v1/settings/api-tokens", `{"enabled":false}`, key); w.Code != http.StatusForbidden || decodeErr(t, w) != "session_required" {
		t.Fatalf("admin key on a reauth-gated change = %d %s", w.Code, w.Body.String())
	}

	plain, _ := f.create(t, tokenOpHost, "owner-session", `{"name":"read"}`)
	if w := f.withKey("GET", tokenOpHost, "/api/v1/me", "", plain); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin key on the operator console = %d", w.Code)
	}
}
