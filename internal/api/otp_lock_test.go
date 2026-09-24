package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The per-code attempt cap resets on every resend; these pin the account-level
// budget that does not. The public login door must stay uniform (a locked
// account reads like a wrong code and mails nothing), tell the owner by mail
// once, and reopen when the window ends. The signed-in onboarding door answers
// 429 with Retry-After instead.

type noticeMailer struct {
	captureMailer
	notices []string // "to|subject|body"
}

func (m *noticeMailer) SendNotice(_ context.Context, email, subject, body string) error {
	m.notices = append(m.notices, email+"|"+subject+"|"+body)
	return nil
}

func TestLoginDoorLocksAfterDailyWrongCodeBudget(t *testing.T) {
	api, repo, _ := seedLoginEmailAPI(t)
	mailer := &noticeMailer{}
	api.Mailer = mailer
	clock := time.Unix(1_700_000_000, 0)
	api.Now = func() time.Time { return clock }
	eh := api.ExternalHandler()

	start := func() {
		t.Helper()
		clock = clock.Add(otpResendCooldown + time.Second)
		if w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"player@example.net"}`, jsonHeader); w.Code != http.StatusAccepted {
			t.Fatalf("start = %d (%s)", w.Code, w.Body.String())
		}
	}
	verify := func(code string) int {
		t.Helper()
		w := do(eh, "POST", "/api/v1/auth/email/verify", `{"email":"player@example.net","code":"`+code+`"}`, jsonHeader)
		if w.Code != http.StatusOK {
			if c, _ := errEnvelope(t, w); c != "invalid_code" {
				t.Fatalf("verify answered %d %s; the login door must only ever say invalid_code", w.Code, c)
			}
		}
		return w.Code
	}

	// Ten wrong codes over three sends; no single code reaches its own cap.
	for i := 0; i < otpFailureBudget; i++ {
		if i%4 == 0 {
			start()
		}
		if code := verify("not-the-code"); code != http.StatusBadRequest {
			t.Fatalf("wrong guess %d = %d", i+1, code)
		}
	}
	sends := mailer.calls
	liveCode := mailer.code

	if len(mailer.notices) != 1 || !strings.HasPrefix(mailer.notices[0], "Player@Example.NET|") {
		t.Fatalf("lock notices = %q, want one to the stored address", mailer.notices)
	}
	var lockAudits int
	for _, a := range repo.audits {
		if a.Action == "auth.otp.locked" {
			lockAudits++
			if a.Actor != "player" || !strings.Contains(string(a.Payload), `"purpose":"login_email"`) {
				t.Fatalf("lock audit = %+v", a)
			}
		}
	}
	if lockAudits != 1 {
		t.Fatalf("lock audits = %d, want 1", lockAudits)
	}

	// Locked: the right code fails like a wrong one, and a resend mails nothing.
	if code := verify(liveCode); code != http.StatusBadRequest {
		t.Fatalf("right code while locked = %d, want 400", code)
	}
	start()
	if mailer.calls != sends {
		t.Fatalf("a locked door mailed a code (%d sends, want %d)", mailer.calls, sends)
	}
	if len(mailer.notices) != 1 {
		t.Fatalf("a standing lock re-sent the notice: %d", len(mailer.notices))
	}

	// The window ends: a fresh send and its code work again.
	clock = clock.Add(otpFailureWindow)
	start()
	if mailer.calls != sends+1 {
		t.Fatalf("no code mailed after the window (%d sends)", mailer.calls)
	}
	if code := verify(mailer.code); code != http.StatusOK {
		t.Fatalf("right code after the window = %d, want 200", code)
	}
}

func TestOnboardDoorLockAnswers429(t *testing.T) {
	repo := newFakeRepo()
	repo.staff["player"] = &StaffUser{ID: "u1", Username: "player", Email: "old@example.net"}
	mailer := &noticeMailer{}
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}}
	api.Mailer = mailer
	clock := time.Unix(1_700_000_000, 0)
	api.Now = func() time.Time { return clock }
	eh := api.ExternalHandler()

	var last int
	for i := 0; i < otpFailureBudget; i++ {
		if i%4 == 0 {
			clock = clock.Add(otpResendCooldown + time.Second)
			if w := do(eh, "POST", "/api/v1/account/email/start", `{"email":"player@example.net"}`, nil); w.Code != http.StatusAccepted {
				t.Fatalf("start = %d (%s)", w.Code, w.Body.String())
			}
		}
		w := do(eh, "POST", "/api/v1/account/email/verify", `{"code":"not-the-code"}`, nil)
		last = w.Code
		if i == otpFailureBudget-1 {
			if c, _ := errEnvelope(t, w); w.Code != http.StatusTooManyRequests || c != "otp_account_locked" {
				t.Fatalf("10th wrong code = %d %s, want 429 otp_account_locked", w.Code, c)
			}
			if w.Header().Get("Retry-After") == "" {
				t.Fatal("locked answer carries no Retry-After")
			}
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("last = %d", last)
	}
	// The signed-in owner sees the 429; no notice mail is needed.
	if len(mailer.notices) != 0 {
		t.Fatalf("onboarding lock mailed a notice: %q", mailer.notices)
	}
	clock = clock.Add(otpResendCooldown + time.Second)
	sends := mailer.calls
	w := do(eh, "POST", "/api/v1/account/email/start", `{"email":"player@example.net"}`, nil)
	if c, _ := errEnvelope(t, w); w.Code != http.StatusTooManyRequests || c != "otp_account_locked" || mailer.calls != sends {
		t.Fatalf("start while locked = %d %s (sends %d→%d), want 429 and no mail", w.Code, c, sends, mailer.calls)
	}
}

func TestOTPLockEnd(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		failures int
		now      time.Time
		locked   bool
	}{
		{"under budget", otpFailureBudget - 1, t0.Add(time.Hour), false},
		{"budget spent", otpFailureBudget, t0.Add(time.Hour), true},
		{"window just ended", otpFailureBudget, t0.Add(otpFailureWindow), false},
	} {
		got := otpLockEnd(t0, tc.failures, tc.now)
		if got.IsZero() == tc.locked {
			t.Errorf("%s: otpLockEnd = %v, locked want %v", tc.name, got, tc.locked)
		}
		if tc.locked && !got.Equal(t0.Add(otpFailureWindow)) {
			t.Errorf("%s: lock ends %v, want window end", tc.name, got)
		}
	}
}

func TestOTPLockNoticeNamesDoorAndTime(t *testing.T) {
	subject, body := otpLockNotice(otpDoorName[otpPurposeLogin], time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC))
	for _, want := range []string{"邮箱验证码登录", "email-code sign-in"} {
		if !strings.Contains(subject, want) {
			t.Errorf("subject %q missing %q", subject, want)
		}
	}
	for _, want := range []string{"2026-09-25 08:00 UTC", "Passkey", "passkey", "10"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}
