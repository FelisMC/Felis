package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Pre-session Email-OTP LOGIN tests (spec §B, console.<root_domain> returning-player
// door). The load-bearing properties, in the order the flow meets them:
//
//   - Neutrality on start: an unknown address gets a byte-identical 202 to a real
//     send AND the same cooldown reservation, so neither the response nor the
//     throttle is an existence oracle.
//   - Purpose separation: login codes (otpPurposeLogin) and onboarding codes
//     (otpPurposeOnboard) never satisfy each other, even for one account holding
//     both live at once.
//   - Verify uniformity: unknown address and wrong code collapse to the same
//     invalid_code envelope, so a code-less caller learns nothing.
//   - Staff refusal AFTER redeem: only the mailbox owner, holding a live code, can
//     ever see the staff_account refusal — and the code is spent reaching it.

// seedLoginEmailAPI wires the public email-login door: local sessions enabled and a
// single verified player "player" (id u1) whose proven address is stored in MIXED
// case, so the case-insensitivity contracts (resolve on typed lowercase, mint against
// stored casing) are exercised by default. Both routes are Public — no External
// wiring needed.
func seedLoginEmailAPI(t *testing.T) (*API, *fakeRepo, *captureMailer) {
	t.Helper()
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["player"] = &StaffUser{
		ID: "u1", Username: "player", Email: "Player@Example.NET",
		Role: "user", EmailVerified: true,
	}
	mailer := &captureMailer{}
	api := newTestAPI(repo, newFakeCluster())
	api.Mailer = mailer
	return api, repo, mailer
}

// errEnvelope decodes the standard error body into its stable (code, message) pair —
// request_id varies per request, so uniformity assertions compare these two fields,
// never raw bytes.
func errEnvelope(t *testing.T, w *httptest.ResponseRecorder) (code, msg string) {
	t.Helper()
	var raw map[string]map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("error body not JSON: %v (%s)", err, w.Body.String())
	}
	return raw["error"]["code"], raw["error"]["message"]
}

// TestLoginEmailVertical walks the whole returning-player slice: a typed lowercase
// address resolves the mixed-case stored account, the code is mailed to the account's
// STORED casing (the address of record), and redeeming it mints the same host-only
// felis_session as the other session doors — single-use, audited on both halves by the
// account's username. The redeem never rewrites users.email (login re-proves an
// already-verified address via ConsumeLoginEmailOTP), so the stored casing is
// untouched by definition.
func TestLoginEmailVertical(t *testing.T) {
	api, repo, mailer := seedLoginEmailAPI(t)
	eh := api.ExternalHandler()

	// 1) start: 202 says "sent" and when it expires — never the code itself.
	w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"player@example.net"}`, jsonHeader)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: code = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	b := acctBody(t, w)
	if b["sent"] != true {
		t.Errorf("start body sent = %v, want true", b["sent"])
	}
	if _, leaked := b["code"]; leaked {
		t.Error("start response must NEVER carry the code")
	}
	if s, _ := b["expires_at"].(string); s == "" {
		t.Error("start must report expires_at")
	}
	// The mail goes to the account's STORED address, not the typed casing — the code
	// is delivered to the mailbox of record regardless of how the player typed it.
	if mailer.calls != 1 || mailer.email != "Player@Example.NET" {
		t.Fatalf("mailer: calls=%d email=%q, want 1 send to the STORED casing Player@Example.NET",
			mailer.calls, mailer.email)
	}
	code := mailer.code
	if len(code) != otpCodeDigits {
		t.Fatalf("delivered code %q: len = %d, want %d", code, len(code), otpCodeDigits)
	}
	// Exactly one row, scoped to the LOGIN purpose, holding a hash — not the digits.
	if len(repo.otps) != 1 {
		t.Fatalf("persisted codes = %d, want 1", len(repo.otps))
	}
	for _, o := range repo.otps {
		if o.purpose != otpPurposeLogin {
			t.Errorf("otp purpose = %q, want %q", o.purpose, otpPurposeLogin)
		}
		if o.codeHash == code {
			t.Error("store holds the plaintext code, not its hash")
		}
	}

	// 2) verify — typed in yet another casing, proving the verify-side resolver is
	// case-insensitive too — mints the session.
	w = do(eh, "POST", "/api/v1/auth/email/verify",
		`{"email":"PLAYER@example.NET","code":"`+code+`"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	vb := acctBody(t, w)
	if vb["user_id"] != "u1" || vb["role"] != "user" {
		t.Fatalf("verify body = %v, want user_id:u1 role:user", vb)
	}
	// The HttpOnly cookie is the whole point — same contract as every session door.
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
	if want := time.Unix(1_700_000_000, 0).Add(sessionTTL); !s.expiresAt.Equal(want) {
		t.Errorf("session expiresAt = %v, want now+sessionTTL = %v", s.expiresAt, want)
	}

	// Both halves audit by the account's username (there is no principal yet).
	if n := len(repo.audits); n != 2 {
		t.Fatalf("want 2 audits (otp_sent, login), got %d: %+v", n, repo.audits)
	}
	if repo.audits[0].Action != "auth.login_email.otp_sent" || repo.audits[0].Actor != "player" {
		t.Errorf("first audit = %+v, want auth.login_email.otp_sent by player", repo.audits[0])
	}
	if repo.audits[1].Action != "auth.login_email" || repo.audits[1].Actor != "player" {
		t.Errorf("second audit = %+v, want auth.login_email by player", repo.audits[1])
	}

	// 3) single-use: the consumed code buys nothing a second time.
	if w := do(eh, "POST", "/api/v1/auth/email/verify",
		`{"email":"player@example.net","code":"`+code+`"}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("replay of consumed code: code = %d body %s, want 400 invalid_code", w.Code, w.Body.String())
	}
}

// TestLoginEmailStartNeutralOnUnknownAddress pins the start-side anti-enumeration
// contract: an address with no verified account yields a 202 BYTE-IDENTICAL to a
// real send (frozen clock ⇒ same expires_at), mints and mails nothing, audits
// nothing — and a re-probe inside the cooldown answers the same 202 a resend does.
func TestLoginEmailStartNeutralOnUnknownAddress(t *testing.T) {
	// A real send for comparison.
	apiK, _, _ := seedLoginEmailAPI(t)
	wK := do(apiK.ExternalHandler(), "POST", "/api/v1/auth/email/start",
		`{"email":"player@example.net"}`, jsonHeader)
	if wK.Code != http.StatusAccepted {
		t.Fatalf("known-address start: code = %d (%s)", wK.Code, wK.Body.String())
	}

	// The unknown address: same 202, same bytes, nothing behind it.
	repoU := newFakeRepo()
	repoU.settings[LocalAuthEnabledKey] = []byte("true")
	mailerU := &captureMailer{}
	apiU := newTestAPI(repoU, newFakeCluster())
	apiU.Mailer = mailerU
	ehU := apiU.ExternalHandler()

	wU := do(ehU, "POST", "/api/v1/auth/email/start", `{"email":"ghost@example.net"}`, jsonHeader)
	if wU.Code != http.StatusAccepted {
		t.Fatalf("unknown-address start: code = %d, want 202 (%s)", wU.Code, wU.Body.String())
	}
	if wU.Body.String() != wK.Body.String() {
		t.Errorf("neutral 202 differs from a real send's:\n unknown: %s\n   known: %s",
			wU.Body.String(), wK.Body.String())
	}
	if len(repoU.otps) != 0 || mailerU.calls != 0 || len(repoU.audits) != 0 {
		t.Errorf("neutral path must mint/mail/audit nothing, got otps=%d mails=%d audits=%d",
			len(repoU.otps), mailerU.calls, len(repoU.audits))
	}
	// Re-probing inside the window answers exactly as the first probe did.
	if w := do(ehU, "POST", "/api/v1/auth/email/start", `{"email":"ghost@example.net"}`, jsonHeader); w.Code != http.StatusAccepted || w.Body.String() != wK.Body.String() {
		t.Fatalf("re-probe of unknown address: code = %d body %s, want the same 202 as a real send",
			w.Code, w.Body.String())
	}
	if len(repoU.otps) != 0 || mailerU.calls != 0 {
		t.Errorf("re-probe must mint/mail nothing, got otps=%d mails=%d", len(repoU.otps), mailerU.calls)
	}

	// An UNVERIFIED account is indistinguishable from no account: UserByEmail only
	// resolves proven addresses, so the door never mails one nobody controls.
	repoV := newFakeRepo()
	repoV.settings[LocalAuthEnabledKey] = []byte("true")
	repoV.staff["u"] = &StaffUser{ID: "u9", Username: "u", Email: "half@example.net", Role: "user"} // EmailVerified false
	mailerV := &captureMailer{}
	apiV := newTestAPI(repoV, newFakeCluster())
	apiV.Mailer = mailerV
	if w := do(apiV.ExternalHandler(), "POST", "/api/v1/auth/email/start",
		`{"email":"half@example.net"}`, jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("unverified-address start: code = %d, want neutral 202 (%s)", w.Code, w.Body.String())
	}
	if len(repoV.otps) != 0 || mailerV.calls != 0 {
		t.Errorf("unverified address must behave as absent, got otps=%d mails=%d",
			len(repoV.otps), mailerV.calls)
	}
}

// TestLoginEmailGates covers the shared front doors of both halves: the fail-closed
// local-auth toggle, the cross-site-forgery Content-Type guard (these are Public,
// credential-minting routes — same rationale as handleBindRedeem), and the input
// gates that must reject before any mint or lookup.
func TestLoginEmailGates(t *testing.T) {
	t.Run("local auth disabled -> 403 on both halves", func(t *testing.T) {
		api := newTestAPI(newFakeRepo(), newFakeCluster()) // no LocalAuthEnabledKey: fails closed
		eh := api.ExternalHandler()
		if w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"a@example.net"}`, jsonHeader); w.Code != http.StatusForbidden || decodeErr(t, w) != "local_auth_disabled" {
			t.Errorf("start: code = %d body %s, want 403 local_auth_disabled", w.Code, w.Body.String())
		}
		if w := do(eh, "POST", "/api/v1/auth/email/verify", `{"email":"a@example.net","code":"123456"}`, jsonHeader); w.Code != http.StatusForbidden || decodeErr(t, w) != "local_auth_disabled" {
			t.Errorf("verify: code = %d body %s, want 403 local_auth_disabled", w.Code, w.Body.String())
		}
	})

	t.Run("non-JSON content type -> 415 on both halves", func(t *testing.T) {
		api, _, _ := seedLoginEmailAPI(t)
		eh := api.ExternalHandler()
		for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
			if w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"a@example.net"}`, ctHeader(ct)); w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("start with Content-Type %q: code = %d, want 415", ct, w.Code)
			}
			if w := do(eh, "POST", "/api/v1/auth/email/verify", `{"email":"a@example.net","code":"123456"}`, ctHeader(ct)); w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("verify with Content-Type %q: code = %d, want 415", ct, w.Code)
			}
		}
	})

	t.Run("start bad email -> 400, nothing minted or mailed", func(t *testing.T) {
		bad := map[string]string{
			"missing email": `{}`,
			"empty email":   `{"email":""}`,
			"no at-sign":    `{"email":"notanemail"}`,
			"two at-signs":  `{"email":"a@b@example.net"}`,
			"unknown field": `{"email":"a@example.net","x":1}`,
		}
		for name, body := range bad {
			api, repo, mailer := seedLoginEmailAPI(t)
			w := do(api.ExternalHandler(), "POST", "/api/v1/auth/email/start", body, jsonHeader)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: code = %d, want 400 (%s)", name, w.Code, w.Body.String())
			}
			if len(repo.otps) != 0 || mailer.calls != 0 {
				t.Errorf("%s: a rejected start must not mint or mail (otps=%d mails=%d)",
					name, len(repo.otps), mailer.calls)
			}
		}
	})

	t.Run("verify bad inputs -> 400 bad_request", func(t *testing.T) {
		bad := map[string]string{
			"bad email":     `{"email":"notanemail","code":"123456"}`,
			"empty code":    `{"email":"a@example.net","code":""}`,
			"missing code":  `{"email":"a@example.net"}`,
			"unknown field": `{"email":"a@example.net","code":"123456","x":1}`,
		}
		for name, body := range bad {
			api, _, _ := seedLoginEmailAPI(t)
			w := do(api.ExternalHandler(), "POST", "/api/v1/auth/email/verify", body, jsonHeader)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: code = %d, want 400 (%s)", name, w.Code, w.Body.String())
			}
		}
	})
}

// TestLoginEmailStartRateLimited closes the unauthenticated email-bomb vector on the
// public door: one send per recipient per window, keyed case-insensitively, and
// namespaced apart from the authenticated onboarding throttle so neither door can
// starve the other. A start inside the window is answered like the one that sent,
// so whoever asks lands on the code screen with the mail already in the inbox.
func TestLoginEmailStartRateLimited(t *testing.T) {
	start := func(eh http.Handler, email string) *httptest.ResponseRecorder {
		return do(eh, "POST", "/api/v1/auth/email/start", `{"email":"`+email+`"}`, jsonHeader)
	}

	t.Run("a start inside the window mails nothing and keeps the first code", func(t *testing.T) {
		api, repo, mailer := seedLoginEmailAPI(t)
		clock := time.Unix(1_700_000_000, 0)
		api.Now = func() time.Time { return clock }
		eh := api.ExternalHandler()

		if w := start(eh, "player@example.net"); w.Code != http.StatusAccepted {
			t.Fatalf("first send: code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		code := mailer.code
		clock = clock.Add(30 * time.Second)
		w := start(eh, "player@example.net")
		if w.Code != http.StatusAccepted {
			t.Fatalf("resend inside the window: code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		// The expiry is the first code's, the one the inbox holds.
		if b := acctBody(t, w); b["sent"] != true || b["expires_at"] != "2023-11-14T22:23:20Z" {
			t.Errorf("resend body = %v, want sent:true expires_at:2023-11-14T22:23:20Z", b)
		}
		if mailer.calls != 1 || len(repo.otps) != 1 {
			t.Errorf("resend inside the window must not mint or mail: mails=%d otps=%d, want 1/1",
				mailer.calls, len(repo.otps))
		}
		if w := do(eh, "POST", "/api/v1/auth/email/verify",
			`{"email":"player@example.net","code":"`+code+`"}`, jsonHeader); w.Code != http.StatusOK {
			t.Fatalf("first code after a resend: code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		clock = clock.Add(otpResendCooldown)
		if w := start(eh, "player@example.net"); w.Code != http.StatusAccepted || mailer.calls != 2 {
			t.Fatalf("post-cooldown send: code = %d mails = %d, want 202 and a second mail (%s)",
				w.Code, mailer.calls, w.Body.String())
		}
	})

	t.Run("throttle key is case-insensitive", func(t *testing.T) {
		api, _, mailer := seedLoginEmailAPI(t)
		eh := api.ExternalHandler()
		if w := start(eh, "Player@Example.NET"); w.Code != http.StatusAccepted {
			t.Fatalf("first send: code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		// A recased retype is the same mailbox: it must hit the same window.
		if w := start(eh, "player@example.net"); w.Code != http.StatusAccepted || mailer.calls != 1 {
			t.Fatalf("recased resend: code = %d mails = %d, want 202 and no second mail (key must be lowercased)",
				w.Code, mailer.calls)
		}
	})

	t.Run("login and onboard cooldowns are namespaced apart", func(t *testing.T) {
		// The unauthenticated login door must not perturb the authenticated
		// onboarding throttle for the same mailbox — distinct keys, so both doors
		// admit one send each at the same instant.
		api, _, mailer := seedLoginEmailAPI(t)
		api.External = staticExternal{p: &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}}
		eh := api.ExternalHandler()

		if w := start(eh, "player@example.net"); w.Code != http.StatusAccepted {
			t.Fatalf("login start: code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		if w := do(eh, "POST", "/api/v1/account/email/start", `{"email":"player@example.net"}`, nil); w.Code != http.StatusAccepted {
			t.Fatalf("onboard start same mailbox, same instant: code = %d, want 202 — the doors must not share a throttle key (%s)",
				w.Code, w.Body.String())
		}
		if mailer.calls != 2 {
			t.Errorf("mailer calls = %d, want 2 (one per door)", mailer.calls)
		}
	})
}

// TestLoginStartsInsideTheWindowMatchExactly: with a clock that moves on every read,
// a start inside the cooldown still answers with the very expires_at the first start
// returned, on both email doors and for known and unknown addresses alike, so a
// repeat start is indistinguishable from the first down to the nanosecond.
func TestLoginStartsInsideTheWindowMatchExactly(t *testing.T) {
	ticking := func(api *API) {
		clock := time.Unix(1_700_000_000, 0)
		api.Now = func() time.Time {
			clock = clock.Add(time.Millisecond)
			return clock
		}
	}
	expiry := func(t *testing.T, w *httptest.ResponseRecorder) string {
		t.Helper()
		if w.Code != http.StatusAccepted {
			t.Fatalf("start: code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		e, _ := acctBody(t, w)["expires_at"].(string)
		return e
	}
	for _, email := range []string{"player@example.net", "ghost@example.net"} {
		api, _, _ := seedLoginEmailAPI(t)
		ticking(api)
		eh := api.ExternalHandler()
		start := func() *httptest.ResponseRecorder {
			return do(eh, "POST", "/api/v1/auth/email/start", `{"email":"`+email+`"}`, jsonHeader)
		}
		first, again := expiry(t, start()), expiry(t, start())
		if first != "2023-11-14T22:23:20.001Z" || again != first {
			t.Errorf("email door %s: expires_at %q then %q, want 2023-11-14T22:23:20.001Z twice", email, first, again)
		}
	}
	for _, email := range []string{"op@example.net", "ghost@example.net"} {
		api, _, _ := seedOpLoginAPI(t)
		ticking(api)
		eh := api.ExternalHandler()
		first, again := expiry(t, startOp(eh, email)), expiry(t, startOp(eh, email))
		if first != "2023-11-14T22:23:20.001Z" || again != first {
			t.Errorf("op door %s: expires_at %q then %q, want 2023-11-14T22:23:20.001Z twice", email, first, again)
		}
	}
}

// TestLoginEmailStartKeepsEarlierCodes is the stranger-keeps-starting case: someone
// who knows the address starts a login every cooldown. Each start adds a code to the
// owner's inbox and never cancels one, so the owner's own code keeps working until
// otpLiveLoginCodes newer ones exist; signing in spends every code still out.
func TestLoginEmailStartKeepsEarlierCodes(t *testing.T) {
	api, repo, mailer := seedLoginEmailAPI(t)
	clock := time.Unix(1_700_000_000, 0)
	api.Now = func() time.Time { return clock }
	eh := api.ExternalHandler()
	start := func() string {
		t.Helper()
		if w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"player@example.net"}`, jsonHeader); w.Code != http.StatusAccepted {
			t.Fatalf("start: code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		code := mailer.code
		clock = clock.Add(otpResendCooldown)
		return code
	}
	verify := func(code string) *httptest.ResponseRecorder {
		return do(eh, "POST", "/api/v1/auth/email/verify",
			`{"email":"player@example.net","code":"`+code+`"}`, jsonHeader)
	}

	owner := start()
	second := start()
	third := start()
	if mailer.calls != 3 || len(repo.otps) != 3 {
		t.Fatalf("mails=%d otps=%d, want 3/3 (a start must not cancel earlier codes)", mailer.calls, len(repo.otps))
	}
	fourth := start()
	if len(repo.otps) != 3 {
		t.Fatalf("otps = %d after a fourth start, want 3 (the oldest goes)", len(repo.otps))
	}
	if w := verify(owner); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("code with three newer ones: code = %d body %s, want 400 invalid_code", w.Code, w.Body.String())
	}
	if w := verify(second); w.Code != http.StatusOK {
		t.Fatalf("second code while two newer are live: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	for name, code := range map[string]string{"third": third, "fourth": fourth} {
		if w := verify(code); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
			t.Errorf("%s code after the sign-in: code = %d body %s, want 400 invalid_code", name, w.Code, w.Body.String())
		}
	}
}

// TestLoginEmailVerifyRejections is the redeem-side failure matrix. The anchor case
// is uniformity: an unknown address and a wrong code for a known address answer with
// the same (code, message) envelope, so the verify half never doubles as an
// existence oracle. Expired/locked rows are planted directly — the frozen clock
// makes that the only deterministic route to those branches.
func TestLoginEmailVerifyRejections(t *testing.T) {
	verify := func(eh http.Handler, email, code string) *httptest.ResponseRecorder {
		return do(eh, "POST", "/api/v1/auth/email/verify",
			`{"email":"`+email+`","code":"`+code+`"}`, jsonHeader)
	}
	// liveLogin plants an unconsumed LOGIN-purpose code for u1.
	liveLogin := func(repo *fakeRepo, id, codeHash string, expiresAt time.Time, attempts int) {
		repo.otps[id] = &fakeEmailOTP{
			id: id, userID: "u1", email: "player@example.net", codeHash: codeHash,
			purpose: otpPurposeLogin, attempts: attempts,
			expiresAt: expiresAt, createdAt: expiresAt,
		}
	}

	t.Run("unknown address is indistinguishable from a wrong code", func(t *testing.T) {
		// Known account, live code, wrong digits.
		apiW, repoW, _ := seedLoginEmailAPI(t)
		liveLogin(repoW, "lg", otpCodeHash("123456"), time.Unix(1_700_000_600, 0), 0)
		wWrong := verify(apiW.ExternalHandler(), "player@example.net", "654321")
		// No account at all.
		apiU, _, _ := seedLoginEmailAPI(t)
		wGhost := verify(apiU.ExternalHandler(), "ghost@example.net", "654321")

		if wWrong.Code != http.StatusBadRequest || wGhost.Code != http.StatusBadRequest {
			t.Fatalf("codes = %d/%d, want 400/400", wWrong.Code, wGhost.Code)
		}
		wc, wm := errEnvelope(t, wWrong)
		gc, gm := errEnvelope(t, wGhost)
		if wc != "invalid_code" || wc != gc || wm != gm {
			t.Errorf("envelopes differ: known=(%s,%q) unknown=(%s,%q) — must be identical", wc, wm, gc, gm)
		}
	})

	t.Run("known address, no live code -> 400 invalid_code", func(t *testing.T) {
		api, repo, _ := seedLoginEmailAPI(t)
		w := verify(api.ExternalHandler(), "player@example.net", "123456")
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if len(repo.sessions) != 0 {
			t.Error("no session may be minted on a failed verify")
		}
	})

	t.Run("wrong code charges an attempt, does not consume", func(t *testing.T) {
		api, repo, _ := seedLoginEmailAPI(t)
		liveLogin(repo, "wr", otpCodeHash("123456"), time.Unix(1_700_000_600, 0), 0)
		w := verify(api.ExternalHandler(), "player@example.net", "654321")
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if repo.otps["wr"].attempts != 1 || repo.otps["wr"].consumed {
			t.Errorf("attempts=%d consumed=%v, want 1/false", repo.otps["wr"].attempts, repo.otps["wr"].consumed)
		}
	})

	t.Run("expired code -> 400 invalid_code", func(t *testing.T) {
		api, repo, _ := seedLoginEmailAPI(t)
		// One second before the frozen clock (time.Unix(1_700_000_000, 0)).
		liveLogin(repo, "ex", otpCodeHash("123456"), time.Unix(1_699_999_999, 0), 0)
		w := verify(api.ExternalHandler(), "player@example.net", "123456")
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})

	t.Run("exhausted code is invisible: same invalid_code as a wrong code, no locked oracle", func(t *testing.T) {
		// A locked row (attempts == cap) must NOT surface as a distinct 429 otp_locked
		// on this public, pre-session door: that status would be an existence oracle —
		// a code-less prober could mail a victim address a code, burn its attempt budget,
		// and read otp_locked as "this address has an account." It collapses to the SAME
		// 400 invalid_code a wrong code returns, byte-for-byte. (The authenticated
		// onboarding door keeps otp_locked as actionable feedback; this one cannot.)
		api, repo, _ := seedLoginEmailAPI(t)
		liveLogin(repo, "lk", otpCodeHash("123456"), time.Unix(1_700_000_600, 0), otpMaxAttempts)
		wLocked := verify(api.ExternalHandler(), "player@example.net", "123456") // correct digits, but exhausted

		// A plain wrong code on a fresh known account, for the byte-for-byte comparison.
		apiW, repoW, _ := seedLoginEmailAPI(t)
		liveLogin(repoW, "wr", otpCodeHash("123456"), time.Unix(1_700_000_600, 0), 0)
		wWrong := verify(apiW.ExternalHandler(), "player@example.net", "654321")

		if wLocked.Code != http.StatusBadRequest {
			t.Fatalf("exhausted code: code = %d body %s, want 400 invalid_code (NOT 429 otp_locked)",
				wLocked.Code, wLocked.Body.String())
		}
		lc, lm := errEnvelope(t, wLocked)
		wc, wm := errEnvelope(t, wWrong)
		if lc != "invalid_code" || lc != wc || lm != wm {
			t.Errorf("locked envelope must be identical to a wrong code's: locked=(%s,%q) wrong=(%s,%q)",
				lc, lm, wc, wm)
		}
		if len(repo.sessions) != 0 {
			t.Error("no session may be minted from a locked code")
		}
	})
}

// TestLoginEmailPurposeSeparation proves the email_otps purpose column does its one
// job in both directions: a live ONBOARDING code cannot open the login door, a live
// LOGIN code cannot satisfy onboarding verify. Two complementary properties fall out
// and both are pinned: the OTHER flow's row is never consumed or charged (the
// per-purpose query never sees it), while the door's OWN live code IS charged one
// attempt — cross-door digits are just a wrong guess, never a free brute-force try.
func TestLoginEmailPurposeSeparation(t *testing.T) {
	api, repo, _ := seedLoginEmailAPI(t)
	api.External = staticExternal{p: &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}}
	eh := api.ExternalHandler()

	// Both purposes live at once for u1, distinct digits.
	repo.otps["ob"] = &fakeEmailOTP{
		id: "ob", userID: "u1", email: "player@example.net", codeHash: otpCodeHash("111111"),
		purpose: otpPurposeOnboard, expiresAt: time.Unix(1_700_000_600, 0), createdAt: time.Unix(1_700_000_600, 0),
	}
	repo.otps["lg"] = &fakeEmailOTP{
		id: "lg", userID: "u1", email: "Player@Example.NET", codeHash: otpCodeHash("222222"),
		purpose: otpPurposeLogin, expiresAt: time.Unix(1_700_000_600, 0), createdAt: time.Unix(1_700_000_600, 0),
	}

	// Onboarding code at the LOGIN door: refused. The onboard row is untouched (the
	// login-purpose query never saw it); the LIVE LOGIN code is charged one attempt —
	// to the login door these are simply wrong digits.
	w := do(eh, "POST", "/api/v1/auth/email/verify",
		`{"email":"player@example.net","code":"111111"}`, jsonHeader)
	if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("onboard code at login door: code = %d body %s, want 400 invalid_code", w.Code, w.Body.String())
	}
	if o := repo.otps["ob"]; o.consumed || o.attempts != 0 {
		t.Errorf("onboard row must be untouched by a login verify: consumed=%v attempts=%d", o.consumed, o.attempts)
	}
	if o := repo.otps["lg"]; o.consumed || o.attempts != 1 {
		t.Errorf("cross-door digits must cost the login code one attempt (never a free guess): consumed=%v attempts=%d",
			o.consumed, o.attempts)
	}
	if len(repo.sessions) != 0 {
		t.Fatal("a cross-purpose code must never mint a session")
	}

	// Login code at the ONBOARDING door: refused symmetrically — the login row is not
	// consumed and not charged further (the onboard-purpose query never saw it; its
	// one attempt above stands), while the onboard code eats the wrong guess...
	w = do(eh, "POST", "/api/v1/account/email/verify", `{"code":"222222"}`, nil)
	if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("login code at onboard door: code = %d body %s, want 400 invalid_code", w.Code, w.Body.String())
	}
	if o := repo.otps["lg"]; o.consumed || o.attempts != 1 {
		t.Errorf("login row must not be consumed or re-charged by an onboard verify: consumed=%v attempts=%d",
			o.consumed, o.attempts)
	}
	if o := repo.otps["ob"]; o.consumed || o.attempts != 1 {
		t.Errorf("cross-door digits must cost the onboard code one attempt: consumed=%v attempts=%d",
			o.consumed, o.attempts)
	}
	// ...and the login code is still redeemable at its own door afterwards.
	w = do(eh, "POST", "/api/v1/auth/email/verify",
		`{"email":"player@example.net","code":"222222"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("login code at its own door after the cross attempts: code = %d, want 200 (%s)",
			w.Code, w.Body.String())
	}
}

// TestLoginEmailVerifyRefusesStaff pins the staff refusal AND its ordering. The
// public door provably never mints a session for role=admin (op.console keeps its
// Zero-Trust gate) — but the refusal must be reachable only by the mailbox owner:
// a wrong code for a staff address answers the same invalid_code as for anyone, and
// the 403 costs the valid code (verify-then-refuse), so it cannot be farmed as an
// is-this-address-staff oracle.
// Staff means admin AND owner (migration 0011): both must be refused at the
// player door, after the code proves mailbox control.
func TestLoginEmailVerifyRefusesStaff(t *testing.T) {
	for _, role := range []string{"admin", "owner"} {
		t.Run(role, func(t *testing.T) { verifyStaffRefusedAtPlayerDoor(t, role) })
	}
}

func verifyStaffRefusedAtPlayerDoor(t *testing.T, role string) {
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["owner"] = &StaffUser{
		ID: "a1", Username: "owner", Email: "boss@example.net",
		Role: role, EmailVerified: true,
	}
	mailer := &captureMailer{}
	api := newTestAPI(repo, newFakeCluster())
	api.Mailer = mailer
	eh := api.ExternalHandler()

	// Start happily mails a staff address — the refusal lives at verify, after the
	// code proves mailbox control, so start stays neutral.
	if w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"boss@example.net"}`, jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("start for staff address: code = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	good := mailer.code
	if good == "000000" {
		t.Skip("astronomically unlucky code collision; rerun")
	}

	// Without the code, staffness is invisible: plain invalid_code.
	if w := do(eh, "POST", "/api/v1/auth/email/verify",
		`{"email":"boss@example.net","code":"000000"}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("wrong code for staff: code = %d body %s, want 400 invalid_code", w.Code, w.Body.String())
	}

	// With the code: 403 staff_account — and no cookie, no session row, no success audit.
	w := do(eh, "POST", "/api/v1/auth/email/verify",
		`{"email":"boss@example.net","code":"`+good+`"}`, jsonHeader)
	if w.Code != http.StatusForbidden || decodeErr(t, w) != "staff_account" {
		t.Fatalf("valid code for staff: code = %d body %s, want 403 staff_account", w.Code, w.Body.String())
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("no session cookie may be set for a refused staff login")
	}
	if len(repo.sessions) != 0 {
		t.Error("no session row may be minted for a refused staff login")
	}
	for _, a := range repo.audits {
		if a.Action == "auth.login_email" {
			t.Error("a refused staff login must not be audited as a successful login")
		}
	}
	// Ordering pin: the refusal consumed the code (verify ran BEFORE the staff
	// check), so replaying it now collapses to invalid_code.
	if w := do(eh, "POST", "/api/v1/auth/email/verify",
		`{"email":"boss@example.net","code":"`+good+`"}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("replay after staff refusal: code = %d body %s, want 400 invalid_code (code must be spent)",
			w.Code, w.Body.String())
	}
}

// TestLoginEmailFaceSeparation enforces that both halves are web-only: the internal
// (service-token) face must 404 them, never serve them.
func TestLoginEmailFaceSeparation(t *testing.T) {
	api, _, _ := seedLoginEmailAPI(t)
	ih := api.InternalHandler()
	if w := do(ih, "POST", "/api/v1/auth/email/start", `{"email":"a@example.net"}`, jsonHeader); w.Code != http.StatusNotFound {
		t.Errorf("start on internal face: code = %d, want 404", w.Code)
	}
	if w := do(ih, "POST", "/api/v1/auth/email/verify", `{"email":"a@example.net","code":"123456"}`, jsonHeader); w.Code != http.StatusNotFound {
		t.Errorf("verify on internal face: code = %d, want 404", w.Code)
	}
}

// TestLoginEmailStartFailedDeliveryReleasesCooldown covers the reserve→rollback
// path on the login door: a send that reserves the window but fails to deliver must
// release it, so the immediate retry is admitted instead of 429'd — a transient SMTP
// blip must not lock a returning player out for the whole window. The failure is
// also not audited as a send.
func TestLoginEmailStartFailedDeliveryReleasesCooldown(t *testing.T) {
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["player"] = &StaffUser{
		ID: "u1", Username: "player", Email: "player@example.net",
		Role: "user", EmailVerified: true,
	}
	mailer := &flakyMailer{}
	api := newTestAPI(repo, newFakeCluster()) // frozen clock: both attempts share one window
	api.Mailer = mailer
	eh := api.ExternalHandler()

	if w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"player@example.net"}`, jsonHeader); w.Code < 500 {
		t.Fatalf("first send (mailer fails): code = %d, want 5xx (%s)", w.Code, w.Body.String())
	}
	for _, a := range repo.audits {
		if a.Action == "auth.login_email.otp_sent" {
			t.Error("a failed delivery must not be audited as otp_sent")
		}
	}
	if w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"player@example.net"}`, jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("retry after failed delivery: code = %d, want 202 (the failed send must release the cooldown) (%s)",
			w.Code, w.Body.String())
	}
	if mailer.calls != 2 {
		t.Errorf("mailer calls = %d, want 2 (one failed, one delivered)", mailer.calls)
	}
}

// A disabled or soft-deleted account is DEAD at every door: the pre-session
// resolvers refuse it (uniformly, so the door stays no-oracle), a session that
// was live a moment ago stops authenticating, DeleteUser severs the account's
// passkeys and Minecraft links, and the bind door refuses to reuse the retired
// identity instead of minting a session for it. Audit #33 found the opposite
// live: a deleted user re-logged-in through the email door and GET /me answered
// 200 — deletion and the disable lockout were both bypassable by logging in again.
func TestDeadAccountsCannotLogInOrKeepSessions(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)

	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.seedUser(UserView{ID: "u-dead", Username: "dead", Email: "dead@example.net", Role: "user"})
	repo.staff["dead"].EmailVerified = true
	mailer := &captureMailer{}
	api := newTestAPI(repo, newFakeCluster())
	api.Mailer = mailer
	eh := api.ExternalHandler()

	// Control: alive — the door resolves the account and mails a real code, and a
	// session minted for it authenticates.
	if w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"dead@example.net"}`, jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("alive start: code = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	if mailer.calls != 1 {
		t.Fatalf("alive start mailed %d codes, want 1", mailer.calls)
	}
	repo.sessions["h-live"] = &fakeSession{userID: "u-dead", expiresAt: now.Add(time.Hour)}
	if _, err := repo.SessionUser(ctx, "h-live", now); err != nil {
		t.Fatalf("live SessionUser: %v", err)
	}

	// Disabled: the start is neutral (no mail), verify refuses, session dies.
	if err := repo.SetUserDisabled(ctx, "u-dead", true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// The alive start's reservation must not mask the neutral branch: clear the
	// throttle's window (test-only; the limiter itself is rebuilt lazily once).
	lim := api.otpLimiter()
	lim.mu.Lock()
	lim.last = map[string]time.Time{}
	lim.mu.Unlock()
	if w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"dead@example.net"}`, jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("disabled start: code = %d, want 202 neutral (%s)", w.Code, w.Body.String())
	}
	if mailer.calls != 1 {
		t.Fatalf("disabled start mailed a code (%d calls) — a dead account must resolve to nothing", mailer.calls)
	}
	if w := do(eh, "POST", "/api/v1/auth/email/verify", `{"email":"dead@example.net","code":"000000"}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("disabled verify: code = %d body %s, want 400 invalid_code", w.Code, w.Body.String())
	}
	if _, err := repo.SessionUser(ctx, "h-live", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled SessionUser = %v, want ErrNotFound", err)
	}

	// Deleted: same refusals; assets severed (links released, passkeys dropped).
	if err := repo.SetUserDisabled(ctx, "u-dead", false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	uuid := "11111111-2222-3333-4444-555555555555"
	repo.links[uuid] = "u-dead"
	repo.passkeyCreds["pk-1"] = PasskeyCredential{ID: "pk-1", UserID: "u-dead", CredentialID: "cred-1"}
	if err := repo.DeleteUser(ctx, "u-dead", "test"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if w := do(eh, "POST", "/api/v1/auth/email/verify", `{"email":"dead@example.net","code":"000000"}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("deleted verify: code = %d body %s, want 400 invalid_code", w.Code, w.Body.String())
	}
	if _, err := repo.SessionUser(ctx, "h-live", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted SessionUser = %v, want ErrNotFound", err)
	}
	if _, ok := repo.links[uuid]; ok {
		t.Error("DeleteUser left the Minecraft link: the UUID stays claimed forever")
	}
	if _, ok := repo.passkeyCreds["pk-1"]; ok {
		t.Error("DeleteUser left the passkey: a login credential outlives the account")
	}

	// The bind door refuses to reuse the retired identity (a stale link that
	// predates the fix, or a username squatted by the deleted row).
	repo.links[uuid] = "u-dead"
	repo.linkCodes["CODE1234"] = fakeLinkCode{mcUUID: uuid, authSource: "mojang", expiresAt: now.Add(time.Hour)}
	if _, _, _, err := repo.RedeemPlayerBindCode(ctx, "u-new", "CODE1234", now); !errors.Is(err, ErrPlayerAccountRetired) {
		t.Fatalf("bind redeem onto a deleted account = %v, want ErrPlayerAccountRetired", err)
	}
	if _, ok := repo.linkCodes["CODE1234"]; !ok {
		t.Error("refused redeem consumed the code; re-enabling the account must stay retryable within TTL")
	}
}
