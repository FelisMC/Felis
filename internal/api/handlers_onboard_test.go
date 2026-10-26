package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const bindTestUUID = "11111111-1111-1111-1111-111111111111"

// seedBindAPI uses real SessionAuth so minted cookies can authenticate subsequent requests.
func seedBindAPI(t *testing.T) (*API, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	api := newTestAPI(repo, newFakeCluster())
	api.External = SessionAuth{Repo: repo, RootDomain: testRoot, Now: api.now}
	return api, repo
}

// mintBindCode stores a live Bind Code for uuid/authSource, mirroring the internal
// mint (handleCreateLinkCode → CreateLinkCode).
func mintBindCode(t *testing.T, api *API, repo *fakeRepo, code, uuid, authSource string) {
	t.Helper()
	if err := repo.CreateLinkCode(t.Context(), code, uuid, authSource, api.now().Add(LinkCodeTTL)); err != nil {
		t.Fatalf("mint bind code: %v", err)
	}
}

// sessionCookieValue returns the raw felis_session cookie value set on w, or fails.
func sessionCookieValue(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return c.Value
		}
	}
	t.Fatalf("no %s cookie set (%s)", sessionCookieName, w.Body.String())
	return ""
}

func TestBindRedeemBootstrapsPlayer(t *testing.T) {
	api, repo := seedBindAPI(t)
	mintBindCode(t, api, repo, "ABCD2345", bindTestUUID, authSourceMojang)

	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/bind", `{"code":"ABCD2345"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
	}
	userID, _ := got["user_id"].(string)
	if userID == "" || got["linked"] != true || got["mc_uuid"] != bindTestUUID || got["auth_source"] != authSourceMojang {
		t.Fatalf("body = %v, want a user_id, linked=true, mc_uuid+auth_source echoed", got)
	}

	// A fresh role=user player row was created and bound; the code was consumed.
	if u := repo.staff[bindTestUUID]; u == nil || u.Role != "user" || u.ID != userID {
		t.Fatalf("created row = %+v, want role=user, id=%s", u, userID)
	}
	if repo.links[bindTestUUID] != userID {
		t.Fatalf("account_links[%s] = %q, want %q", bindTestUUID, repo.links[bindTestUUID], userID)
	}
	if _, live := repo.linkCodes["ABCD2345"]; live {
		t.Fatal("the bind code must be consumed on success")
	}

	// The minted session must resolve through the external face: /me returns the
	// player identity, role=user, is_admin=false.
	token := sessionCookieValue(t, w)
	me := do(api.ExternalHandler(), "GET", "/api/v1/me", "",
		map[string]string{"Cookie": sessionCookieName + "=" + token})
	if me.Code != http.StatusOK {
		t.Fatalf("/me code = %d, want 200 (%s)", me.Code, me.Body.String())
	}
	var meBody map[string]any
	if err := json.Unmarshal(me.Body.Bytes(), &meBody); err != nil {
		t.Fatalf("/me body not JSON: %v", err)
	}
	if meBody["role"] != "user" || meBody["is_admin"] != false || meBody["user_id"] != userID {
		t.Fatalf("/me = %v, want role=user is_admin=false user_id=%s", meBody, userID)
	}
}

func TestBindRedeemRefusesStaffAccount(t *testing.T) {
	api, repo := seedBindAPI(t)
	// An admin already linked to this UUID (e.g. an Operator on the thirdparty Yggdrasil).
	repo.staff["op"] = &StaffUser{ID: "admin1", Username: "op", Role: "admin"}
	repo.links[bindTestUUID] = "admin1"
	mintBindCode(t, api, repo, "STAFF234", bindTestUUID, authSourceThirdParty)

	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/bind", `{"code":"STAFF234"}`, jsonHeader)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 (%s)", w.Code, w.Body.String())
	}
	if code := decodeErr(t, w); code != "staff_account" {
		t.Fatalf("error code = %q, want staff_account", code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("no session cookie may be set when a staff UUID is refused")
	}
	if _, live := repo.linkCodes["STAFF234"]; !live {
		t.Fatal("a refused staff redemption must NOT consume the code")
	}
}

func TestPlayerSessionRejectedOnAdminRoutes(t *testing.T) {
	api, repo := seedBindAPI(t)
	mintBindCode(t, api, repo, "PLAYER34", bindTestUUID, authSourceMojang)
	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/bind", `{"code":"PLAYER34"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("redeem code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	token := sessionCookieValue(t, w)

	adm := do(api.ExternalHandler(), "POST", "https://op.console.mc.example.net/api/v1/servers", `{}`,
		map[string]string{"Cookie": sessionCookieName + "=" + token})
	if adm.Code != http.StatusForbidden {
		t.Fatalf("player on Admin route: code = %d, want 403 (%s)", adm.Code, adm.Body.String())
	}

	auth := SessionAuth{Repo: repo, RootDomain: testRoot, Now: api.now}
	r := httptest.NewRequest("GET", "https://op.console.mc.example.net/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	p, err := auth.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Role != "user" || p.ViaAdminAccess || p.IsAdmin() {
		t.Fatalf("player principal = %+v, want role=user ViaAdminAccess=false IsAdmin=false", p)
	}
}

func TestBindRedeemInvalidCode(t *testing.T) {
	api, _ := seedBindAPI(t)
	for _, tc := range []struct{ name, body string }{
		{"unknown code", `{"code":"NOPE2345"}`},
		{"empty code", `{"code":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(api.ExternalHandler(), "POST", "/api/v1/auth/bind", tc.body, jsonHeader)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("no session cookie may be set on an invalid redeem")
			}
		})
	}
}

func TestBindRedeemExpiredCode(t *testing.T) {
	api, repo := seedBindAPI(t)
	repo.linkCodes["OLD23456"] = fakeLinkCode{
		mcUUID: bindTestUUID, authSource: authSourceMojang,
		expiresAt: api.now().Add(-time.Minute), // already expired
	}
	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/bind", `{"code":"OLD23456"}`, jsonHeader)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if code := decodeErr(t, w); code != "invalid_code" {
		t.Fatalf("error code = %q, want invalid_code", code)
	}
}

func TestBindRedeemLocalAuthDisabled(t *testing.T) {
	repo := newFakeRepo() // local_auth_enabled never set → fail closed
	api := newTestAPI(repo, newFakeCluster())
	mintBindCode(t, api, repo, "DEAD2345", bindTestUUID, authSourceMojang)
	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/bind", `{"code":"DEAD2345"}`, jsonHeader)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 (%s)", w.Code, w.Body.String())
	}
	if code := decodeErr(t, w); code != "local_auth_disabled" {
		t.Fatalf("error code = %q, want local_auth_disabled", code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("no session cookie may be minted while local auth is disabled")
	}
}

func TestBindRedeemContentTypeGuard(t *testing.T) {
	api, repo := seedBindAPI(t)
	mintBindCode(t, api, repo, "FORM2345", bindTestUUID, authSourceMojang)
	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/bind", `{"code":"FORM2345"}`,
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("code = %d, want 415 (%s)", w.Code, w.Body.String())
	}
	if _, live := repo.linkCodes["FORM2345"]; !live {
		t.Fatal("a content-type-rejected redeem must not consume the code")
	}
}
