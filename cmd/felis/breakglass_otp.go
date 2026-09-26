package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/platform"
)

// Recovery mode proves who is breaking the glass (#13). Naming a staff account is
// where it starts: the console then mails a one-time code to that account's verified
// address, and only that code, typed within recoveryCodeTTL, makes the run a
// recovery attributed to the account. Every other ending — no such account, no
// verified address, no relay, a send that fails, a wrong or late code, or the
// operator giving up on the mail — leads to the typed OVERRIDE, which the audit row
// records as an unverified root_override together with the reason (otp_skipped).
// The code goes through the same [smtp] relay as the panel's login codes, so with
// that relay down recovery still works, as an override that says why.

const (
	recoveryCodeTTL      = 10 * time.Minute
	recoveryCodeAttempts = 5
)

// The reasons a run fell back to the override, recorded as otp_skipped.
const (
	otpSkipUnknownAdmin    = "unknown_admin"
	otpSkipNoVerifiedEmail = "no_verified_email"
	otpSkipNoRelay         = "no_relay"
	otpSkipSendFailed      = "send_failed"
	otpSkipCodeExpired     = "code_expired"
	otpSkipCodeRejected    = "code_rejected"
	otpSkipByOperator      = "operator_skipped"
)

// verifiedByEmailOTP is the audit's verified_by for a recovery the mailed code proved.
const verifiedByEmailOTP = "email_otp"

// recoveryMailer is the one relay call a recovery code needs; *mail.SMTP has it.
type recoveryMailer interface {
	SendNotice(ctx context.Context, email, subject, body string) error
}

// recoveryConfig is what the console needs to mail a recovery code. open resolves
// the relay only when a code is about to go out, so a console used to halt a server
// never touches [smtp] or the cluster; its error says why no relay is available.
// host names this machine in the mail. The zero value has no relay.
type recoveryConfig struct {
	open func(ctx context.Context) (recoveryMailer, error)
	host string
	now  func() time.Time
}

func (r recoveryConfig) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// recoveryCode is one mailed code: its value, when it stops working, and how many
// wrong codes were typed against it.
type recoveryCode struct {
	value    string
	expires  time.Time
	failures int
}

func newRecoveryCode(now time.Time) (*recoveryCode, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return nil, fmt.Errorf("generate recovery code: %w", err)
	}
	return &recoveryCode{value: fmt.Sprintf("%06d", n.Int64()), expires: now.Add(recoveryCodeTTL)}, nil
}

type codeVerdict int

const (
	codeAccepted codeVerdict = iota
	codeWrong
	codeExpired
	codeExhausted
)

// check compares a typed code in constant time. Each wrong code counts; the one
// that reaches recoveryCodeAttempts exhausts the code, which then accepts nothing,
// and neither does an expired one.
func (c *recoveryCode) check(typed string, now time.Time) codeVerdict {
	if c.failures >= recoveryCodeAttempts {
		return codeExhausted
	}
	if !now.Before(c.expires) {
		return codeExpired
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(typed)), []byte(c.value)) == 1 {
		return codeAccepted
	}
	c.failures++
	if c.failures >= recoveryCodeAttempts {
		return codeExhausted
	}
	return codeWrong
}

func (c *recoveryCode) attemptsLeft() int { return recoveryCodeAttempts - c.failures }

// recoveryStart is where naming an admin led: a code on its way to that admin, or
// the reason the run has to fall back to the override.
type recoveryStart struct {
	admin  *api.StaffUser // the named staff account; nil when none matched
	code   *recoveryCode  // set when the code went out
	skip   string         // otpSkip* when it did not
	detail string         // what failed, for the override screen and the audit row
}

// resolveAdmin loads the staff account (admin or owner) a typed username names, or
// nil when there is none.
func resolveAdmin(ctx context.Context, s ownerStore, username string) (*api.StaffUser, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, nil
	}
	u, err := s.UserByUsername(ctx, username)
	if errors.Is(err, api.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Staff means admin or owner: the Owner is the primary break-glass identity.
	if u.Role != "admin" && u.Role != "owner" {
		return nil, nil
	}
	return u, nil
}

// beginRecovery resolves the named admin and mails it a recovery code. Only a
// datastore or entropy fault is an error; every other way the code cannot go out is
// a recoveryStart with skip set.
func beginRecovery(ctx context.Context, s ownerStore, rc recoveryConfig, username, osUser string, op bgOperation) (recoveryStart, error) {
	admin, err := resolveAdmin(ctx, s, username)
	if err != nil {
		return recoveryStart{}, err
	}
	if admin == nil {
		return recoveryStart{skip: otpSkipUnknownAdmin}, nil
	}
	st := recoveryStart{admin: admin}
	// An address nobody ever proved vouches for nobody.
	email := strings.TrimSpace(admin.Email)
	if email == "" || !admin.EmailVerified {
		st.skip = otpSkipNoVerifiedEmail
		return st, nil
	}
	if rc.open == nil {
		st.skip, st.detail = otpSkipNoRelay, "this console has no mail relay"
		return st, nil
	}
	relay, err := rc.open(ctx)
	if err != nil {
		st.skip, st.detail = otpSkipNoRelay, err.Error()
		return st, nil
	}
	code, err := newRecoveryCode(rc.clock())
	if err != nil {
		return recoveryStart{}, err
	}
	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	subject, body := recoveryMail(code.value, rc.host, osUser, admin.Username, op)
	if err := relay.SendNotice(sendCtx, email, subject, body); err != nil {
		st.skip, st.detail = otpSkipSendFailed, err.Error()
		return st, nil
	}
	st.code = code
	return st, nil
}

// recoveryMail words the code mail. It says where, by whom and for what the console
// was opened, so an admin who did not ask for it learns that root on that machine is
// in someone else's hands.
func recoveryMail(code, host, osUser, admin string, op bgOperation) (subject, body string) {
	what, whatZH := "reset the Owner account", "重置 Owner 账号"
	if op == bgAddOperator {
		what, whatZH = "add an Operator account", "添加 Operator 账号"
	}
	if host == "" {
		host = "the Felis host"
	}
	minutes := int(recoveryCodeTTL / time.Minute)
	subject = "Felis break-glass recovery code / 紧急恢复验证码"
	body = fmt.Sprintf(`Someone with root on %[1]s (OS user %[2]s) opened felis breakGlass and named your staff account %[3]q to %[4]s.

Recovery code: %[6]s
It works for %[7]d minutes.

If this was not you, root on that machine is in someone else's hands: change its credentials and read the audit log for break_glass entries.

有人在 %[1]s 上以 root 身份（系统用户 %[2]s）打开了 felis breakGlass，指名你的管理员账号 %[3]q 来%[5]s。

恢复验证码：%[6]s
%[7]d 分钟内有效。

如果不是你本人，这台机器的 root 已落入他人之手：请更换它的凭据，并查看审计日志中的 break_glass 记录。
`, host, osUser, admin, what, whatZH, code, minutes)
	return subject, body
}

// maskEmail keeps the first character of the local part and the domain, enough for
// the operator to recognise the address without putting it on screen whole.
func maskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return "***"
	}
	return email[:1] + strings.Repeat("*", max(at-1, 3)) + email[at:]
}

// hostRecoveryMailer opens the [smtp] relay from the host the way the watchdog does:
// the password is the env var password_ref names when that is set, else the host
// copy at passwordPath, else the felis-smtp Secret, whose absence means a relay
// without AUTH. The cluster is reached only when the host copy is missing, so a
// break-glass on a host whose k3s is down still gets its code.
func hostRecoveryMailer(c config.SMTPConfig, passwordPath, controlNS string) func(context.Context) (recoveryMailer, error) {
	return func(ctx context.Context) (recoveryMailer, error) {
		if strings.TrimSpace(c.Host) == "" {
			return nil, errors.New("[smtp] is not configured in felis.toml")
		}
		if ref := c.PasswordRef; ref != "" && os.Getenv(ref) != "" {
			return smtpRelay(c, os.Getenv(ref)), nil
		}
		if password, ok, err := readHostCredential(passwordPath); err != nil {
			return nil, fmt.Errorf("read the relay password: %w", err)
		} else if ok {
			return smtpRelay(c, password), nil
		}
		cl, err := buildSystemServerClient()
		if err != nil {
			return nil, fmt.Errorf("reach the cluster for the relay password: %w", err)
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		password, err := smtpSecretPassword(ctx, cl, controlNS)
		if err != nil {
			return nil, fmt.Errorf("read the relay password from %s/%s: %w", controlNS, platform.SMTPSecretName, err)
		}
		return smtpRelay(c, password), nil
	}
}
