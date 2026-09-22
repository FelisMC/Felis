package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captureMailer is the OTPMailer seam under test: it records the last code so a
// test can read the digits that production would only ever email out of band.
type captureMailer struct {
	email, code string
	calls       int
	err         error
}

func (m *captureMailer) SendOTP(_ context.Context, email, code string) error {
	m.calls++
	if m.err != nil {
		return m.err
	}
	m.email, m.code = email, code
	return nil
}

// TestEmailOTPVertical walks the whole §B2 email-proof slice across the external
// face: a player asks for a code, the platform delivers it (here, into the test
// mailer), the player types it back, and only then does the user row flip verified.
// It proves the digits never ride the HTTP response and that both halves audit.
func TestEmailOTPVertical(t *testing.T) {
	const email = "player@example.net"
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}

	repo := newFakeRepo()
	// Seed the staff/user row the verify path flips, keyed (in the fake) by username
	// but matched by ID — exactly how PGRepo updates users by id.
	repo.staff["player"] = &StaffUser{ID: "u1", Email: "old@example.net"}

	mailer := &captureMailer{}
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: user}
	api.Mailer = mailer
	eh := api.ExternalHandler()

	// 1) start mints + delivers a code. The response says "sent" and when it expires
	// — but NEVER the code itself.
	w := do(eh, "POST", "/api/v1/account/email/start", `{"email":"`+email+`"}`, nil)
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
	if mailer.calls != 1 || mailer.email != email {
		t.Fatalf("mailer not invoked for %s: calls=%d email=%q", email, mailer.calls, mailer.email)
	}
	code := mailer.code
	if len(code) != otpCodeDigits {
		t.Fatalf("delivered code %q: len = %d, want %d", code, len(code), otpCodeDigits)
	}
	// Only the hash is persisted — the plaintext must not be findable in the store.
	for _, o := range repo.otps {
		if o.codeHash == code {
			t.Error("store holds the plaintext code, not its hash")
		}
	}

	// 2) the player submits the code. The email is written and verified flips true.
	w = do(eh, "POST", "/api/v1/account/email/verify", `{"code":"`+code+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if vb := acctBody(t, w); vb["verified"] != true || vb["email"] != email {
		t.Fatalf("verify body = %v, want verified:true email:%s", vb, email)
	}
	if su := repo.staff["player"]; !su.EmailVerified || su.Email != email {
		t.Fatalf("user row not flipped: verified=%v email=%q", su.EmailVerified, su.Email)
	}

	// Both halves are audited by the principal's Access email.
	if n := len(repo.audits); n != 2 {
		t.Fatalf("want 2 audits (otp_sent, verified), got %d: %+v", n, repo.audits)
	}
	if repo.audits[0].Action != "account.email.otp_sent" || repo.audits[0].Actor != "u1@example.net" {
		t.Errorf("first audit = %+v, want account.email.otp_sent by u1@example.net", repo.audits[0])
	}
	if repo.audits[1].Action != "account.email.verified" || repo.audits[1].Actor != "u1@example.net" {
		t.Errorf("second audit = %+v, want account.email.verified by u1@example.net", repo.audits[1])
	}

	// 3) the code is single-use: re-submitting the consumed code now fails.
	if w := do(eh, "POST", "/api/v1/account/email/verify", `{"code":"`+code+`"}`, nil); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("replay of consumed code: code = %d body %s, want 400 invalid_code", w.Code, w.Body.String())
	}
}

// TestWithRecoverLogsPanicStack pins the observability contract: a recovered panic
// must still leave the client an opaque 500 "panic", but the panic value, a stack,
// and the request path MUST be logged first — otherwise a 500 like the email-OTP
// report is untraceable (an operator has nothing to grep for). Regression guard for
// the silent recover that cost a live debugging session.
func TestWithRecoverLogsPanicStack(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	h := withRequestID(withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom-xyz")
	})))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/account/email/start", nil))

	if w.Code != http.StatusInternalServerError || errCode(w.Body.Bytes()) != "panic" {
		t.Fatalf("recovered response = %d %q, want 500 panic (%s)", w.Code, errCode(w.Body.Bytes()), w.Body.String())
	}
	logged := buf.String()
	// The panic value, a real stack (debug.Stack always opens with "goroutine"), and
	// the path — the three things a grep needs to find and place the fault.
	for _, want := range []string{"boom-xyz", "goroutine", "/api/v1/account/email/start"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("panic log missing %q; got:\n%s", want, logged)
		}
	}
}

// TestEmailOTPStartValidation covers the mint-side input gate and the no-mailer
// fallback (the demo path): a malformed address never mints, and a nil Mailer still
// persists a code (logged server-side) so the verify flow stays exercisable.
func TestEmailOTPStartValidation(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	mk := func(repo *fakeRepo) http.Handler {
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		return api.ExternalHandler() // no Mailer wired → demo fallback
	}

	bad := map[string]string{
		"missing email":    `{}`,
		"empty email":      `{"email":""}`,
		"whitespace email": `{"email":"   "}`,
		"no at-sign":       `{"email":"notanemail"}`,
		"at-sign at edge":  `{"email":"@example.net"}`,
		"no dot in domain": `{"email":"a@bcd"}`,
		"two at-signs":     `{"email":"a@b@example.net"}`,
		"unknown field":    `{"email":"a@example.net","x":1}`,
		"trailing at":      `{"email":"player@"}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			repo := newFakeRepo()
			w := do(mk(repo), "POST", "/api/v1/account/email/start", body, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if len(repo.otps) != 0 {
				t.Errorf("a rejected start must not mint a code, got %d", len(repo.otps))
			}
		})
	}

	t.Run("no mailer still persists a code (demo fallback)", func(t *testing.T) {
		repo := newFakeRepo()
		w := do(mk(repo), "POST", "/api/v1/account/email/start", `{"email":"player@example.net"}`, nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		if len(repo.otps) != 1 {
			t.Fatalf("want exactly 1 persisted code, got %d", len(repo.otps))
		}
	})
}

// TestEmailOTPStartRateLimited closes the email-bomb vector: handleEmailOTPStart is
// an authenticated primitive that mails arbitrary addresses, so a resend cooldown
// bounds it on both the caller (a fan-out across many addresses) and the recipient
// (a convergence of many accounts on one mailbox).
func TestEmailOTPStartRateLimited(t *testing.T) {
	const emailA, emailB = "a@example.net", "b@example.net"
	u1 := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	u2 := &Principal{UserID: "u2", Email: "u2@example.net", Role: "user"}
	start := func(eh http.Handler, email string) *httptest.ResponseRecorder {
		return do(eh, "POST", "/api/v1/account/email/start", `{"email":"`+email+`"}`, nil)
	}

	t.Run("same caller and recipient is throttled, then recovers after the cooldown", func(t *testing.T) {
		repo := newFakeRepo()
		mailer := &captureMailer{}
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: u1}
		api.Mailer = mailer
		clock := time.Unix(1_700_000_000, 0)
		api.Now = func() time.Time { return clock }
		eh := api.ExternalHandler()

		if w := start(eh, emailA); w.Code != http.StatusAccepted {
			t.Fatalf("first send: code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		// An immediate resend is refused with 429 — and mints/mails nothing.
		if w := start(eh, emailA); w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "otp_resend_cooldown" {
			t.Fatalf("immediate resend: code = %d body %s, want 429 otp_resend_cooldown", w.Code, w.Body.String())
		}
		if mailer.calls != 1 {
			t.Errorf("mailer calls = %d, want 1 (the throttled resend must not mail)", mailer.calls)
		}
		if len(repo.otps) != 1 {
			t.Errorf("persisted codes = %d, want 1 (the throttled resend must not mint)", len(repo.otps))
		}
		// Once the cooldown elapses the same address may be mailed again.
		clock = clock.Add(otpResendCooldown + time.Second)
		if w := start(eh, emailA); w.Code != http.StatusAccepted {
			t.Fatalf("post-cooldown send: code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("one caller cannot fan out across addresses", func(t *testing.T) {
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.External = staticExternal{p: u1}
		api.Mailer = &captureMailer{}
		eh := api.ExternalHandler()

		if w := start(eh, emailA); w.Code != http.StatusAccepted {
			t.Fatalf("send to A: code = %d, want 202", w.Code)
		}
		// A different recipient, same caller, same instant: the per-caller key throttles.
		if w := start(eh, emailB); w.Code != http.StatusTooManyRequests {
			t.Fatalf("fan-out to B: code = %d, want 429", w.Code)
		}
	})

	t.Run("many callers cannot converge on one recipient", func(t *testing.T) {
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.External = staticExternal{p: u1}
		api.Mailer = &captureMailer{}
		eh := api.ExternalHandler()

		if w := start(eh, emailA); w.Code != http.StatusAccepted {
			t.Fatalf("u1 send to A: code = %d, want 202", w.Code)
		}
		// A different caller targeting the same address, same instant: the per-recipient
		// key throttles even though u2 has never sent before.
		api.External = staticExternal{p: u2}
		if w := start(eh, emailA); w.Code != http.StatusTooManyRequests {
			t.Fatalf("u2 converge on A: code = %d, want 429", w.Code)
		}
	})
}

// TestEmailOTPVerifyRejections is the redeem-side failure matrix. The expired and
// locked cases plant rows directly: the test clock is frozen, so an already-expired
// or already-exhausted row is the only way to reach those branches deterministically.
func TestEmailOTPVerifyRejections(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	mk := func(repo *fakeRepo) http.Handler {
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		return api.ExternalHandler()
	}
	// live seeds an unconsumed onboarding code for u1 with the given hash/expiry.
	live := func(repo *fakeRepo, id, codeHash string, expiresAt time.Time, attempts int) {
		repo.otps[id] = &fakeEmailOTP{
			id: id, userID: "u1", email: "player@example.net", codeHash: codeHash,
			purpose: otpPurposeOnboard, attempts: attempts,
			expiresAt: expiresAt, createdAt: expiresAt,
		}
	}

	t.Run("empty code -> 400 bad_request", func(t *testing.T) {
		w := do(mk(newFakeRepo()), "POST", "/api/v1/account/email/verify", `{"code":""}`, nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("whitespace code -> 400 bad_request", func(t *testing.T) {
		w := do(mk(newFakeRepo()), "POST", "/api/v1/account/email/verify", `{"code":"   "}`, nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("unknown field -> 400", func(t *testing.T) {
		w := do(mk(newFakeRepo()), "POST", "/api/v1/account/email/verify", `{"token":"123456"}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("strict decode must reject unknown field, code = %d", w.Code)
		}
	})
	t.Run("no live code -> 400 invalid_code", func(t *testing.T) {
		w := do(mk(newFakeRepo()), "POST", "/api/v1/account/email/verify", `{"code":"000000"}`, nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("expired code -> 400 invalid_code, not consumed", func(t *testing.T) {
		repo := newFakeRepo()
		// One second before the frozen test clock (time.Unix(1_700_000_000, 0)).
		live(repo, "ex", otpCodeHash("424242"), time.Unix(1_699_999_999, 0), 0)
		w := do(mk(repo), "POST", "/api/v1/account/email/verify", `{"code":"424242"}`, nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if repo.otps["ex"].consumed {
			t.Error("an expired code must not be consumed")
		}
	})
	t.Run("wrong code -> 400 invalid_code, attempt charged, not consumed", func(t *testing.T) {
		repo := newFakeRepo()
		live(repo, "wr", otpCodeHash("123456"), time.Unix(1_700_000_600, 0), 0)
		w := do(mk(repo), "POST", "/api/v1/account/email/verify", `{"code":"654321"}`, nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if repo.otps["wr"].attempts != 1 {
			t.Errorf("a wrong guess must cost one attempt, got %d", repo.otps["wr"].attempts)
		}
		if repo.otps["wr"].consumed {
			t.Error("a wrong guess must not consume the code")
		}
	})
	t.Run("exhausted code -> 429 otp_locked", func(t *testing.T) {
		repo := newFakeRepo()
		// Attempt budget already spent: even the correct code must be refused.
		live(repo, "lk", otpCodeHash("123456"), time.Unix(1_700_000_600, 0), otpMaxAttempts)
		w := do(mk(repo), "POST", "/api/v1/account/email/verify", `{"code":"123456"}`, nil)
		if w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "otp_locked" {
			t.Fatalf("code = %d body %s, want 429 otp_locked", w.Code, w.Body.String())
		}
	})
	t.Run("address proven elsewhere -> 409 email_taken, not consumed", func(t *testing.T) {
		repo := newFakeRepo()
		live(repo, "tk", otpCodeHash("123456"), time.Unix(1_700_000_600, 0), 0)
		// Another account already proved the same address: login resolves accounts
		// BY verified email, so the second proof must be refused.
		repo.staff["other"] = &StaffUser{ID: "u2", Email: "player@example.net", EmailVerified: true}
		w := do(mk(repo), "POST", "/api/v1/account/email/verify", `{"code":"123456"}`, nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "email_taken" {
			t.Fatalf("code = %d body %s, want 409 email_taken", w.Code, w.Body.String())
		}
		if repo.otps["tk"].consumed {
			t.Error("a taken address must not consume the code")
		}
	})
}

// TestEmailOTPBruteForceLockout drives the lockout end-to-end through the handler:
// wrong guesses are charged one at a time until the budget is spent, after which
// even the correct code is refused with 429 — the brute-force ceiling in action.
func TestEmailOTPBruteForceLockout(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	repo := newFakeRepo()
	mailer := &captureMailer{}
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: user}
	api.Mailer = mailer
	eh := api.ExternalHandler()

	if w := do(eh, "POST", "/api/v1/account/email/start", `{"email":"player@example.net"}`, nil); w.Code != http.StatusAccepted {
		t.Fatalf("start: code = %d (%s)", w.Code, w.Body.String())
	}
	good := mailer.code

	// Exhaust the budget with wrong guesses; each is a plain invalid_code.
	for i := 0; i < otpMaxAttempts; i++ {
		w := do(eh, "POST", "/api/v1/account/email/verify", `{"code":"000000"}`, nil)
		// "000000" could, with 1-in-a-million odds, equal the real code; guard that.
		if good == "000000" {
			t.Skip("astronomically unlucky code collision; rerun")
		}
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
			t.Fatalf("guess %d: code = %d body %s, want 400 invalid_code", i, w.Code, w.Body.String())
		}
	}
	// Budget spent: the CORRECT code is now locked out, not accepted.
	w := do(eh, "POST", "/api/v1/account/email/verify", `{"code":"`+good+`"}`, nil)
	if w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "otp_locked" {
		t.Fatalf("post-lockout correct code: code = %d body %s, want 429 otp_locked", w.Code, w.Body.String())
	}
}

// TestEmailOTPSupersede proves a re-request invalidates the prior code: only the
// newest live code for (user, purpose) can be redeemed, so an intercepted-then-
// reissued code cannot be used after the player asks again.
func TestEmailOTPSupersede(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	repo := newFakeRepo()
	mailer := &captureMailer{}
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: user}
	api.Mailer = mailer
	// A re-request is a fresh send, so it must clear the resend cooldown: advance the
	// clock past it between the two starts (supersede is orthogonal to the throttle).
	clock := time.Unix(1_700_000_000, 0)
	api.Now = func() time.Time { return clock }
	eh := api.ExternalHandler()

	do(eh, "POST", "/api/v1/account/email/start", `{"email":"player@example.net"}`, nil)
	first := mailer.code
	clock = clock.Add(otpResendCooldown + time.Second)
	do(eh, "POST", "/api/v1/account/email/start", `{"email":"player@example.net"}`, nil)
	second := mailer.code

	if first == second {
		t.Skip("rng produced identical codes; rerun")
	}
	// Only the second code survives.
	if w := do(eh, "POST", "/api/v1/account/email/verify", `{"code":"`+first+`"}`, nil); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("superseded code: code = %d body %s, want 400 invalid_code", w.Code, w.Body.String())
	}
	if w := do(eh, "POST", "/api/v1/account/email/verify", `{"code":"`+second+`"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("current code: code = %d body %s, want 200", w.Code, w.Body.String())
	}
}

// TestEmailOTPFaceSeparation enforces that both halves are web-only: they require a
// logged-in principal the internal (service-token) face never carries, so crossing
// the face boundary must 404, not silently work.
func TestEmailOTPFaceSeparation(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{p: user}
	ih := api.InternalHandler()

	if w := do(ih, "POST", "/api/v1/account/email/start", `{"email":"a@example.net"}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("start on internal face: code = %d, want 404", w.Code)
	}
	if w := do(ih, "POST", "/api/v1/account/email/verify", `{"code":"123456"}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("verify on internal face: code = %d, want 404", w.Code)
	}
}

// TestEmailOTPMailerError pins the delivery-failure path: a Mailer that errors
// surfaces as a 5xx (the code was persisted but never delivered), and the failure
// is NOT audited as a successful send.
func TestEmailOTPMailerError(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: user}
	api.Mailer = &captureMailer{err: errors.New("smtp down")}
	eh := api.ExternalHandler()

	w := do(eh, "POST", "/api/v1/account/email/start", `{"email":"player@example.net"}`, nil)
	if w.Code < 500 {
		t.Fatalf("mailer error: code = %d, want 5xx (%s)", w.Code, w.Body.String())
	}
	for _, a := range repo.audits {
		if a.Action == "account.email.otp_sent" {
			t.Error("a failed delivery must not be audited as otp_sent")
		}
	}
}

// flakyMailer fails its first SendOTP, then succeeds, so a test can observe whether
// a failed delivery left the cooldown consumed (a retry would be wrongly throttled)
// or released (the retry is admitted, as it must be).
type flakyMailer struct {
	calls int
}

func (m *flakyMailer) SendOTP(_ context.Context, _, _ string) error {
	m.calls++
	if m.calls == 1 {
		return errors.New("smtp down")
	}
	return nil
}

// TestEmailOTPStartFailedDeliveryReleasesCooldown covers the new reserve→rollback
// path: a send that reserves the cooldown but then fails to deliver must release it,
// so the very next attempt at the same instant is admitted rather than 429'd. Without
// the deferred release a transient SMTP blip would lock a player out for the whole
// window — strictly worse than the throttle is meant to be.
func TestEmailOTPStartFailedDeliveryReleasesCooldown(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	mailer := &flakyMailer{}
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{p: user}
	api.Mailer = mailer
	api.Now = func() time.Time { return time.Unix(1_700_000_000, 0) } // frozen: same window
	eh := api.ExternalHandler()

	if w := do(eh, "POST", "/api/v1/account/email/start", `{"email":"player@example.net"}`, nil); w.Code < 500 {
		t.Fatalf("first send (mailer fails): code = %d, want 5xx (%s)", w.Code, w.Body.String())
	}
	// Same instant, same caller and recipient: had the failed send burned the window
	// this would be a 429. The rollback frees it, so the retry delivers.
	if w := do(eh, "POST", "/api/v1/account/email/start", `{"email":"player@example.net"}`, nil); w.Code != http.StatusAccepted {
		t.Fatalf("retry after failed delivery: code = %d, want 202 (the failed send must release the cooldown) (%s)", w.Code, w.Body.String())
	}
	if mailer.calls != 2 {
		t.Errorf("mailer calls = %d, want 2 (one failed, one delivered)", mailer.calls)
	}
}

// leakyMailer fails with the shape a real relay error carries: the SMTP account
// and the relay's numeric refusal are both in the text.
type leakyMailer struct{}

func (leakyMailer) SendOTP(_ context.Context, _, _ string) error {
	return errors.New("smtp: auth as ops@example.net: 535 5.7.8 bad credentials")
}

// TestEmailOTPStartRelayRefusalIsNotInternalError pins both halves of the
// delivery-failure contract. A relay that refuses the message is not a bug in
// Felis, so it must not answer "internal error" — that reads as a broken panel
// and sends the operator hunting through handler code instead of their [smtp]
// block. And the relay's own text must not ride along on the wire: it names the
// SMTP account, while this route is reachable by any signed-in player.
func TestEmailOTPStartRelayRefusalIsNotInternalError(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{p: user}
	api.Mailer = leakyMailer{}
	eh := api.ExternalHandler()

	w := do(eh, "POST", "/api/v1/account/email/start", `{"email":"player@example.net"}`, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502 (a relay refusal is not an internal error) (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "mail_undeliverable") {
		t.Errorf("want a distinct machine code the panel can explain, got: %s", body)
	}
	for _, leak := range []string{"ops@example.net", "535", "bad credentials"} {
		if strings.Contains(body, leak) {
			t.Errorf("relay text %q leaked to an unprivileged caller: %s", leak, body)
		}
	}
}

// gateMailer blocks every SendOTP until all concurrent callers have arrived, making
// any check-then-act window in the throttle deterministically observable instead of
// scheduler-dependent. It is the committed counterpart of the adversarial burst probe.
type gateMailer struct {
	calls   int64
	entered chan struct{}
	release chan struct{}
}

func (m *gateMailer) SendOTP(_ context.Context, _, _ string) error {
	atomic.AddInt64(&m.calls, 1)
	m.entered <- struct{}{}
	<-m.release
	return nil
}

// TestEmailOTPStartConcurrentBurstBounded is the regression guard for the email-bomb
// closure under concurrency. net/http serves each request on its own goroutine, so a
// throttle that peeks then records in two steps lets a burst of starts for one victim
// from one principal all slip through together. The atomic reserve admits exactly one;
// the rest get 429. (Runs without -race — the gate makes the race deterministic.)
func TestEmailOTPStartConcurrentBurstBounded(t *testing.T) {
	const n = 8
	mailer := &gateMailer{entered: make(chan struct{}, n), release: make(chan struct{})}
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{p: &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}}
	api.Mailer = mailer
	api.Now = func() time.Time { return time.Unix(1_700_000_000, 0) } // frozen: one shared window
	eh := api.ExternalHandler()

	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := do(eh, "POST", "/api/v1/account/email/start", `{"email":"victim@example.net"}`, nil)
			codes[i] = w.Code
		}(i)
	}

	// Drain whoever reached the mailer, then release them. A correct throttle admits
	// exactly one; a buggy one lets several arrive before any records, and they pile
	// up here within the deadline.
	deadline := time.After(2 * time.Second)
	reached := 0
loop:
	for reached < n {
		select {
		case <-mailer.entered:
			reached++
		case <-deadline:
			break loop
		}
	}
	close(mailer.release)
	wg.Wait()

	accepted := 0
	for _, c := range codes {
		if c == http.StatusAccepted {
			accepted++
		}
	}
	if got := atomic.LoadInt64(&mailer.calls); got != 1 {
		t.Errorf("burst delivered %d mails for one victim from one account in one window; want exactly 1 (accepted=%d, reached=%d)", got, accepted, reached)
	}
	if accepted != 1 {
		t.Errorf("burst accepted %d starts; want exactly 1 (the rest must be 429)", accepted)
	}
}
