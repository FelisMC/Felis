package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"felis.lolicon.best/internal/metrics"
)

// The account-level wrong-code budget (otpFailureBudget per otpFailureWindow)
// is enforced in the repo; this file is what the doors do with it. The two
// pre-session doors (email login, op-login) answer a locked account exactly
// like a wrong code, so they never become an existence oracle; the owner
// learns about the lock from a notice mail instead. The signed-in doors
// (onboarding, migration step-up) answer 429 otp_account_locked with the time
// the lock ends.

// noticeSender is the optional half of Mailer that sends a free-form notice;
// internal/mail.SMTP implements it (the reaper uses the same method).
type noticeSender interface {
	SendNotice(ctx context.Context, email, subject, body string) error
}

// otpDoorName names each purpose in the lock notice, in both languages.
var otpDoorName = map[string][2]string{
	otpPurposeLogin:   {"邮箱验证码登录", "email-code sign-in"},
	otpPurposeOpLogin: {"管理员登录", "staff sign-in"},
}

// writeOTPAccountLocked answers a signed-in door whose budget is spent.
func writeOTPAccountLocked(w http.ResponseWriter, r *http.Request, until, now time.Time) {
	secs := int64(until.Sub(now).Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	writeError(w, r, newError(http.StatusTooManyRequests, "otp_account_locked",
		"too many wrong codes on this account; email codes work again after %s",
		until.UTC().Format(time.RFC3339)))
}

// noteOTPLock handles a redeem that met the account lock. Only the guess that
// spent the budget (JustLocked) does anything: count, audit, log, and for the
// pre-session doors mail the account so its owner learns why the right code
// stopped working. Everything here is best effort; the lock already holds.
func (a *API) noteOTPLock(r *http.Request, err error, userID, purpose string) {
	var lock *OTPAccountLockedError
	if !errors.As(err, &lock) || !lock.JustLocked {
		return
	}
	metrics.OTPLockoutsTotal.WithLabelValues(purpose).Inc()
	log.Printf("auth: %s codes for user %s locked until %s after %d wrong codes (request_id=%s)",
		purpose, userID, lock.Until.UTC().Format(time.RFC3339), otpFailureBudget, requestIDFromContext(r.Context()))

	ctx := r.Context()
	actor := userID
	u, uerr := a.Repo.UserByID(ctx, userID)
	if uerr == nil && u.Username != "" {
		actor = u.Username
	}
	payload, _ := json.Marshal(map[string]any{
		"user_id": userID, "purpose": purpose, "until": lock.Until.UTC(), "failures": otpFailureBudget,
	})
	if aerr := a.Repo.Audit(ctx, AuditEntry{
		Actor: actor, Source: "external", Action: "auth.otp.locked",
		RequestID: requestIDFromContext(ctx), Payload: payload,
	}); aerr != nil {
		log.Printf("auth: audit of otp lock for user %s failed: %v", userID, aerr)
	}

	door, notify := otpDoorName[purpose]
	if !notify || uerr != nil || u.Email == "" {
		return
	}
	sender, ok := a.Mailer.(noticeSender)
	if !ok {
		log.Printf("auth: no notice mailer; user %s was not told their %s is locked", userID, purpose)
		return
	}
	subject, body := otpLockNotice(door, lock.Until)
	if err := sender.SendNotice(ctx, u.Email, subject, body); err != nil {
		log.Printf("auth: otp lock notice to user %s failed: %v", userID, err)
	}
}

// otpLockNotice renders the bilingual lock notice.
func otpLockNotice(door [2]string, until time.Time) (subject, body string) {
	at := until.UTC().Format("2006-01-02 15:04 MST")
	subject = "Felis " + door[0] + "已暂停 · " + door[1] + " paused"
	body = fmt.Sprintf(`Felis 在 24 小时内收到了 %[1]d 次错误的邮箱验证码，已暂停这个账户的%[2]s，%[3]s 自动恢复。
如果不是你本人在尝试，说明有人在猜你的验证码。账户仍然安全：暂停期间任何验证码都无法登录。
你仍然可以用已绑定的 Passkey 登录。

Felis received %[1]d wrong email codes for this account within 24 hours and paused %[4]s until %[3]s.
If this wasn't you, someone is guessing your code. Your account is safe: no code works while it is paused.
You can still sign in with a passkey you have registered.
`, otpFailureBudget, door[0], at, door[1])
	return subject, body
}
