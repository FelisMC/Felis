package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
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
	w := do(h, "POST", "/api/v1/account/email", `{"email":"owner@example.net"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("set-email code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if u := repo.staff["owner"]; u.Email != "owner@example.net" || u.EmailVerified {
		t.Fatalf("after record: email=%q verified=%v, want the address recorded and UNVERIFIED", u.Email, u.EmailVerified)
	}

	// 3. Email recorded but no passkey → STILL required (email verification is not the gate).
	if s := status(t); s["setup_required"] != true || s["email"] != "owner@example.net" {
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
	api.External = staticExternal{p: &Principal{UserID: "u1", Role: "admin", ViaSession: true, EmailVerified: true, ReauthAt: frozenNow}}
	h := api.ExternalHandler()

	w := do(h, "POST", "/api/v1/account/email", `{"email":"new@x.test"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("set-email code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if u := repo.staff["u"]; u.Email != "new@x.test" || u.EmailVerified {
		t.Fatalf("after record: email=%q verified=%v, want new address recorded and verification CLEARED", u.Email, u.EmailVerified)
	}
}

// TestSetupCompletesForNoEmailPlayer pins the console-tier player fix: a bind-code
// player joins with NO email (by design — no SMTP) and completes forced onboarding by
// enrolling a passkey alone. setup_required MUST then report false, in lockstep with
// requireOnboarded lifting (api.go:615). The OLD predicate (email == "" || !hasPasskey)
// trapped exactly this state — email is empty forever, so it looped the player.
func TestSetupCompletesForNoEmailPlayer(t *testing.T) {
	repo := newFakeRepo()
	// A console-tier player: role=user, no email ever, no passkey.
	repo.staff["p"] = &StaffUser{ID: "p1", Username: "player", Role: "user"}

	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: &Principal{UserID: "p1", Role: "user", ViaSession: true}}
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

	// No email, no passkey → forced onboarding still owed.
	if s := status(t); s["setup_required"] != true || s["email"] != "" {
		t.Fatalf("fresh player status = %v, want setup_required=true email=\"\"", s)
	}

	// Enroll a passkey — the ONLY step a no-email player can complete.
	repo.passkeyCreds["pk1"] = PasskeyCredential{ID: "pk1", UserID: "p1", CredentialID: "cred1"}

	// Setup is now COMPLETE even though email stays empty. The OLD predicate returned
	// true here (email == "") and looped the player forever.
	if s := status(t); s["setup_required"] != false || s["has_passkey"] != true || s["email"] != "" {
		t.Fatalf("post-passkey player status = %v, want setup_required=false has_passkey=true email=\"\"", s)
	}
}

// Redeeming a setup link signs the owner in once. A link the store does not know
// (unknown, spent, expired) is the uniform 400; a store that fails is an outage,
// answered as one, so the page does not tell the owner a link that still works
// was used up.
func TestSetupRedeem(t *testing.T) {
	const raw = "setup-token-raw"
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])
	body := `{"token":"` + raw + `"}`
	setup := func() (*fakeRepo, http.Handler) {
		repo := newFakeRepo()
		repo.settings[LocalAuthEnabledKey] = []byte("true")
		repo.staff["owner"] = &StaffUser{ID: "o1", Username: "owner", Role: "admin"}
		repo.setupTokens[hash] = fakeSetupToken{TokenHash: hash, UserID: "o1",
			ExpiresAt: time.Unix(1_700_000_000, 0).Add(10 * time.Minute)}
		return repo, newTestAPI(repo, newFakeCluster()).ExternalHandler()
	}

	t.Run("redeems once", func(t *testing.T) {
		repo, h := setup()
		w := do(h, "POST", "/api/v1/auth/setup/redeem", body, jsonHeader)
		if w.Code != http.StatusOK {
			t.Fatalf("redeem: code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		cookies := w.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Name != sessionCookieName {
			t.Fatalf("cookies = %v, want one %s", cookies, sessionCookieName)
		}
		if s, ok := repo.sessions[hashCookie(cookies[0].Value)]; !ok || s.userID != "o1" {
			t.Fatalf("session for the cookie = %+v (found %v), want one of o1", s, ok)
		}
		if w := do(h, "POST", "/api/v1/auth/setup/redeem", body, jsonHeader); w.Code != http.StatusBadRequest ||
			errCode(w.Body.Bytes()) != "setup_token_invalid" || len(w.Result().Cookies()) != 0 {
			t.Fatalf("replay: code = %d err = %q cookies = %v, want 400 setup_token_invalid and none",
				w.Code, errCode(w.Body.Bytes()), w.Result().Cookies())
		}
	})

	t.Run("store outage", func(t *testing.T) {
		repo, h := setup()
		repo.failRedeemSetup = errors.New("connection refused")
		w := do(h, "POST", "/api/v1/auth/setup/redeem", body, jsonHeader)
		if w.Code != http.StatusInternalServerError || errCode(w.Body.Bytes()) != "internal" || len(w.Result().Cookies()) != 0 {
			t.Fatalf("outage: code = %d err = %q cookies = %v, want 500 internal and no cookie",
				w.Code, errCode(w.Body.Bytes()), w.Result().Cookies())
		}
	})
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
