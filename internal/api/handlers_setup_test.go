package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestSetupNoSMTPFlow pins the no-SMTP onboarding contract: setup completes on email
// RECORDED + passkey ENROLLED, never on email verification (the bootstrap has no SMTP,
// so the Owner's address is stored unverified). The lockdown must therefore lift on a
// passkey, not on email_verified — otherwise the unverified Owner could never leave the
// wizard to reach the Settings/SMTP page.
func TestSetupNoSMTPFlow(t *testing.T) {
	repo := newFakeRepo()
	// Fresh Owner: admin, no email, unverified, no passkey — exactly post-CompleteOwnerSetup.
	repo.staff["owner"] = &StaffUser{ID: "o1", Username: "owner", Role: "admin"}

	api := newTestAPI(repo, newFakeCluster())
	// A lockdown session principal: authenticated by session, email not yet verified.
	api.External = staticExternal{p: &Principal{UserID: "o1", Role: "admin", ViaSession: true}}
	h := api.ExternalHandler()

	status := func(t *testing.T) map[string]any {
		t.Helper()
		w := do(h, "GET", "/api/v1/auth/setup/status", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("status body not JSON: %v", err)
		}
		return got
	}

	// 1. Nothing done → setup required, no email, no passkey.
	if s := status(t); s["setup_required"] != true || s["email"] != "" || s["has_passkey"] != false {
		t.Fatalf("fresh owner status = %v, want setup_required=true email=\"\" has_passkey=false", s)
	}

	// 2. Record the email — NO OTP. The row is written but email_verified stays false.
	w := do(h, "POST", "/api/v1/account/email", `{"email":"me@flyemoji.moe"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("set-email code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if u := repo.staff["owner"]; u.Email != "me@flyemoji.moe" || u.EmailVerified {
		t.Fatalf("after record: email=%q verified=%v, want the address recorded and UNVERIFIED", u.Email, u.EmailVerified)
	}

	// 3. Email recorded but no passkey → STILL required (email verification is not the gate).
	if s := status(t); s["setup_required"] != true || s["email"] != "me@flyemoji.moe" {
		t.Fatalf("email-only status = %v, want setup_required=true (passkey still missing)", s)
	}

	// 4. The lockdown must still fence a non-SetupAllowed route: no passkey ⇒ 403 setup_required.
	w = do(h, "POST", "/api/v1/me/submissions", `{}`, jsonHeader)
	if w.Code != http.StatusForbidden || errCode(w.Body.Bytes()) != "setup_required" {
		t.Fatalf("pre-passkey locked route: code=%d err=%q, want 403 setup_required (%s)", w.Code, errCode(w.Body.Bytes()), w.Body.String())
	}

	// 5. Enroll a passkey (the Owner's only pre-SMTP credential).
	repo.passkeyCreds["pk1"] = PasskeyCredential{ID: "pk1", UserID: "o1", CredentialID: "cred1"}

	// 6. Passkey present ⇒ setup complete AND the lockdown lifts (the route no longer 403s setup_required).
	if s := status(t); s["setup_required"] != false || s["has_passkey"] != true {
		t.Fatalf("post-passkey status = %v, want setup_required=false has_passkey=true", s)
	}
	w = do(h, "POST", "/api/v1/me/submissions", `{}`, jsonHeader)
	if w.Code == http.StatusForbidden && errCode(w.Body.Bytes()) == "setup_required" {
		t.Fatalf("post-passkey the lockdown did NOT lift: route still 403 setup_required")
	}
}

// TestSetEmailClearsVerified pins the invariant that recording an unproven address
// drops any prior verification: /account/email is app-tier + SetupAllowed, so any
// authenticated session can reach it — an already-verified caller who changes their
// address must NOT keep email_verified=true asserting a proof they never gave. Only
// VerifyEmailOTP (which proves the address) may set that flag.
func TestSetEmailClearsVerified(t *testing.T) {
	repo := newFakeRepo()
	// A fully onboarded staff account: email already proven.
	repo.staff["u"] = &StaffUser{ID: "u1", Username: "u", Role: "admin", Email: "old@x.test", EmailVerified: true}

	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: &Principal{UserID: "u1", Role: "admin", ViaSession: true, EmailVerified: true}}
	h := api.ExternalHandler()

	w := do(h, "POST", "/api/v1/account/email", `{"email":"new@x.test"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("set-email code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if u := repo.staff["u"]; u.Email != "new@x.test" || u.EmailVerified {
		t.Fatalf("after record: email=%q verified=%v, want new address recorded and verification CLEARED", u.Email, u.EmailVerified)
	}
}

// errCode returns the error.code of a JSON error body, or "" if body is not one (a
// non-failing decodeErr for cases where the response may be a success).
func errCode(body []byte) string {
	var raw map[string]map[string]string
	if json.Unmarshal(body, &raw) != nil {
		return ""
	}
	return raw["error"]["code"]
}
