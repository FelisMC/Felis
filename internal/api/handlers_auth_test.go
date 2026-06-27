package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Local-password auth handler tests (spec §B). These exercise the three-route
// surface — login, logout, change-password — against the in-memory fakeRepo, which
// mirrors the PG fail-closed contract. The load-bearing cases are the anti-
// enumeration uniformity (an unknown user and a wrong password are indistinguishable)
// and the requireJSONContentType guard that closes the cross-site login-forgery
// vector: a forged HTML-form POST cannot set application/json, so it is rejected
// before any credential check.

// seedAuthAPI returns an API whose repo has local auth enabled and a single admin
// "owner" (id u1) whose password is the given plaintext. Login is Public, so these
// tests need no External wiring.
func seedAuthAPI(t *testing.T, password string, mustChange bool) (*API, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		t.Fatalf("hash seed password: %v", err)
	}
	repo.staff["owner"] = &StaffUser{
		ID: "u1", Username: "owner", Email: "owner@" + testRoot,
		Role: "admin", PasswordHash: string(hash), MustChangePassword: mustChange,
	}
	return newTestAPI(repo, newFakeCluster()), repo
}

// seedAuthedAPI extends seedAuthAPI with an injected session principal so the
// authenticated change-password route resolves a caller. change-password opts out of
// the first-login lockdown (AllowDuringPasswordChange), so a must-change principal
// still reaches the handler.
func seedAuthedAPI(t *testing.T, password string, mustChange bool) (*API, *fakeRepo) {
	t.Helper()
	api, repo := seedAuthAPI(t, password, mustChange)
	api.External = staticExternal{p: &Principal{
		UserID: "u1", Email: "owner@" + testRoot, Role: "admin", MustChangePassword: mustChange,
	}}
	return api, repo
}

// ctHeader builds a headers map carrying the given Content-Type, or nil for the
// absent-header case (do() then sets no Content-Type at all).
func ctHeader(ct string) map[string]string {
	if ct == "" {
		return nil
	}
	return map[string]string{"Content-Type": ct}
}

var jsonHeader = map[string]string{"Content-Type": "application/json"}

func TestHandleLoginSuccess(t *testing.T) {
	api, _ := seedAuthAPI(t, "correct-horse-battery", true)
	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/login",
		`{"username":"owner","password":"correct-horse-battery"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	// The HttpOnly session cookie is the login's whole point — the panel never reads
	// it, the browser just carries it back.
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName || cookies[0].Value == "" {
		t.Fatalf("want one non-empty %s cookie, got %v", sessionCookieName, cookies)
	}
	if !cookies[0].HttpOnly {
		t.Fatalf("session cookie must be HttpOnly")
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
	}
	if got["user_id"] != "u1" || got["role"] != "admin" || got["must_change_password"] != true {
		t.Fatalf("got %v, want user_id=u1 role=admin must_change_password=true", got)
	}
}

// TestHandleLoginContentTypeGuard pins the confirmed login-CSRF fix: a body whose
// Content-Type is anything an HTML form (or a default cross-site fetch) can emit is
// rejected 415 BEFORE the credential check, so a forged off-origin login never even
// reaches bcrypt. Local auth is enabled and the credentials are valid here, proving
// the rejection is the content-type, not a bad password.
func TestHandleLoginContentTypeGuard(t *testing.T) {
	api, _ := seedAuthAPI(t, "correct-horse-battery", false)
	h := api.ExternalHandler()
	body := `{"username":"owner","password":"correct-horse-battery"}`

	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
		"text/plain;charset=UTF-8",
		"", // header absent entirely
	} {
		w := do(h, "POST", "/api/v1/auth/login", body, ctHeader(ct))
		if w.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: code = %d, want 415", ct, w.Code)
		}
		if code := decodeErr(t, w); code != "unsupported_media_type" {
			t.Fatalf("Content-Type %q: error code = %q, want unsupported_media_type", ct, code)
		}
		if len(w.Result().Cookies()) != 0 {
			t.Fatalf("Content-Type %q: no session cookie may be set on a rejected login", ct)
		}
	}

	// A JSON content-type with a charset parameter is still JSON and must pass.
	if w := do(h, "POST", "/api/v1/auth/login", body,
		map[string]string{"Content-Type": "application/json; charset=utf-8"}); w.Code != http.StatusOK {
		t.Fatalf("application/json; charset=utf-8: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// TestHandleLoginInvalidCredentials proves the anti-enumeration uniformity: a wrong
// password and an unknown username return the SAME 401 invalid_credentials with no
// cookie, so a caller cannot learn which usernames carry a password.
func TestHandleLoginInvalidCredentials(t *testing.T) {
	api, _ := seedAuthAPI(t, "correct-horse-battery", false)
	h := api.ExternalHandler()

	for _, tc := range []struct{ name, body string }{
		{"wrong password", `{"username":"owner","password":"wrong"}`},
		{"unknown user", `{"username":"ghost","password":"whatever"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(h, "POST", "/api/v1/auth/login", tc.body, jsonHeader)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("code = %d, want 401 (%s)", w.Code, w.Body.String())
			}
			if code := decodeErr(t, w); code != "invalid_credentials" {
				t.Fatalf("error code = %q, want invalid_credentials", code)
			}
			if len(w.Result().Cookies()) != 0 {
				t.Fatalf("no session cookie may be set on a failed login")
			}
		})
	}
}

// TestHandleLoginLocalAuthDisabled proves a deployment with no local_auth_enabled
// setting refuses every local login (403), so a Zero-Trust-only console never
// accepts a password.
func TestHandleLoginLocalAuthDisabled(t *testing.T) {
	repo := newFakeRepo() // local_auth_enabled never set → fail closed
	api := newTestAPI(repo, newFakeCluster())
	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/login",
		`{"username":"owner","password":"x"}`, jsonHeader)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 (%s)", w.Code, w.Body.String())
	}
	if code := decodeErr(t, w); code != "local_auth_disabled" {
		t.Fatalf("error code = %q, want local_auth_disabled", code)
	}
}

func TestHandleLoginMissingFields(t *testing.T) {
	api, _ := seedAuthAPI(t, "correct-horse-battery", false)
	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/login",
		`{"username":"","password":""}`, jsonHeader)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

// TestHandleLogout is idempotent: it clears the cookie and returns 200 even with no
// live session, and revokes the presented one when there is.
func TestHandleLogout(t *testing.T) {
	api, repo := seedAuthAPI(t, "correct-horse-battery", false)
	h := api.ExternalHandler()

	// No cookie: still 200, still clears.
	if w := do(h, "POST", "/api/v1/auth/logout", "", nil); w.Code != http.StatusOK {
		t.Fatalf("logout without session: code = %d, want 200", w.Code)
	}

	// With a live session cookie: the matching session is revoked.
	token, err := newSessionToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	repo.sessions[hashCookie(token)] = &fakeSession{userID: "u1", expiresAt: api.now().Add(time.Hour)}
	w := do(h, "POST", "/api/v1/auth/logout", "",
		map[string]string{"Cookie": sessionCookieName + "=" + token})
	if w.Code != http.StatusOK {
		t.Fatalf("logout with session: code = %d, want 200", w.Code)
	}
	if !repo.sessions[hashCookie(token)].revoked {
		t.Fatalf("presented session should be revoked")
	}
}

func TestHandleChangePasswordSuccess(t *testing.T) {
	api, repo := seedAuthedAPI(t, "old-password", true)
	// A second live session for u1: the change must revoke it. This request carries
	// no felis_session cookie, so keep="" and every session of u1 is revoked — the
	// safe direction the handler documents.
	repo.sessions["other-device"] = &fakeSession{userID: "u1", expiresAt: api.now().Add(time.Hour)}

	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/change-password",
		`{"current_password":"old-password","new_password":"brand-new-password"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	u := repo.staff["owner"]
	if u.MustChangePassword {
		t.Fatalf("must_change_password should be cleared after a change")
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("brand-new-password")) != nil {
		t.Fatalf("the new password does not verify against the stored hash")
	}
	if !repo.sessions["other-device"].revoked {
		t.Fatalf("other sessions should be revoked on a password change")
	}
}

// TestHandleChangePasswordContentTypeGuard pins the defense-in-depth guard on the
// authenticated change-password route.
func TestHandleChangePasswordContentTypeGuard(t *testing.T) {
	api, _ := seedAuthedAPI(t, "old-password", false)
	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/change-password",
		`{"current_password":"old-password","new_password":"brand-new-password"}`,
		map[string]string{"Content-Type": "text/plain"})
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("code = %d, want 415 (%s)", w.Code, w.Body.String())
	}
}

func TestHandleChangePasswordRejections(t *testing.T) {
	t.Run("weak new password", func(t *testing.T) {
		api, _ := seedAuthedAPI(t, "old-password", false)
		w := do(api.ExternalHandler(), "POST", "/api/v1/auth/change-password",
			`{"current_password":"old-password","new_password":"short"}`, jsonHeader)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
		if code := decodeErr(t, w); code != "weak_password" {
			t.Fatalf("error code = %q, want weak_password", code)
		}
	})

	t.Run("unchanged password", func(t *testing.T) {
		api, _ := seedAuthedAPI(t, "old-password", false)
		w := do(api.ExternalHandler(), "POST", "/api/v1/auth/change-password",
			`{"current_password":"old-password","new_password":"old-password"}`, jsonHeader)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
		if code := decodeErr(t, w); code != "password_unchanged" {
			t.Fatalf("error code = %q, want password_unchanged", code)
		}
	})

	t.Run("wrong current password", func(t *testing.T) {
		api, _ := seedAuthedAPI(t, "old-password", false)
		w := do(api.ExternalHandler(), "POST", "/api/v1/auth/change-password",
			`{"current_password":"wrong","new_password":"brand-new-password"}`, jsonHeader)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401", w.Code)
		}
		if code := decodeErr(t, w); code != "invalid_credentials" {
			t.Fatalf("error code = %q, want invalid_credentials", code)
		}
	})
}
