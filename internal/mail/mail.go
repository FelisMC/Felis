// Package mail is the SMTP implementation of the api.OTPMailer seam: it
// delivers the email one-time codes the passwordless doors mint (onboarding,
// email login, op-login) through the relay configured in felis.toml [smtp].
// It is deliberately tiny — one message shape, stdlib net/smtp — because the
// only mail Felis ever sends is a six-digit code.
//
// TLS posture: port 465 dials implicit TLS; any other port dials plaintext and
// upgrades via STARTTLS when the relay advertises it. AUTH is attempted only
// when a username is configured, and net/smtp's PlainAuth itself refuses to
// send credentials over an unencrypted connection — a relay that offers no
// TLS can carry unauthenticated mail but can never be handed the password.
package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// sendTimeout bounds one whole SMTP conversation when the caller's context
// carries no deadline of its own; codes are time-critical (the player is
// staring at a spinner), so a wedged relay must fail fast, not hang a handler.
const sendTimeout = 30 * time.Second

// SMTP delivers one-time codes through a single configured relay. Fields
// mirror felis.toml [smtp]; Password is the resolved secret (read from the
// env var password_ref names), never the ref itself.
type SMTP struct {
	Host     string
	Port     int
	From     string
	Username string
	Password string
}

// SendOTP mails code to email as a small bilingual plain-text message. It is
// the api.OTPMailer implementation felis-api wires when [smtp] is configured.
func (s *SMTP) SendOTP(ctx context.Context, email, code string) error {
	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Mail(s.From); err != nil {
		return fmt.Errorf("smtp: MAIL FROM %s: %w", s.From, err)
	}
	if err := c.Rcpt(email); err != nil {
		return fmt.Errorf("smtp: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write(message(s.From, email, code, time.Now())); err != nil {
		return fmt.Errorf("smtp: write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: deliver: %w", err)
	}
	return c.Quit()
}

// Ping proves the configured relay is reachable and the credentials work
// WITHOUT sending any mail: connect, (STARTTLS,) AUTH, NOOP, QUIT. The setup
// wizard runs it before writing anything, so a typo fails at the keyboard
// instead of at the first code a player is waiting on.
func (s *SMTP) Ping(ctx context.Context) error {
	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Noop(); err != nil {
		return fmt.Errorf("smtp: noop: %w", err)
	}
	return c.Quit()
}

// connect dials the relay, upgrades to TLS per the port's posture, and
// authenticates when a username is configured. The whole conversation shares
// one deadline (the context's, else sendTimeout from now).
func (s *SMTP) connect(ctx context.Context) (*smtp.Client, error) {
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(sendTimeout)
	}
	dialer := &net.Dialer{Deadline: deadline}

	var conn net.Conn
	var err error
	if s.Port == 465 {
		// Implicit TLS: the socket is TLS from byte zero (smtps submission).
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: s.Host}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("smtp: dial %s: %w", addr, err)
	}
	_ = conn.SetDeadline(deadline)

	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("smtp: handshake %s: %w", addr, err)
	}
	if s.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
				c.Close()
				return nil, fmt.Errorf("smtp: starttls: %w", err)
			}
		}
	}
	if s.Username != "" {
		// PlainAuth refuses an unencrypted connection on its own, so the password
		// can never leak to a relay that failed to negotiate TLS above.
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			c.Close()
			return nil, fmt.Errorf("smtp: auth as %s: %w", s.Username, err)
		}
	}
	return c, nil
}

// message renders the one mail shape Felis sends: RFC 5322 headers (CRLF, the
// subject Q-encoded for its non-ASCII half) over a short bilingual plain-text
// body carrying the code. Split out from SendOTP so the shape is testable
// without a relay.
func message(from, to, code string, now time.Time) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", "Felis 验证码 · verification code") + "\r\n")
	b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString("Your Felis verification code / Felis 验证码:\r\n")
	b.WriteString("\r\n")
	b.WriteString("    " + code + "\r\n")
	b.WriteString("\r\n")
	b.WriteString("If you didn't request this, ignore this message. / 若非本人操作，请忽略此邮件。\r\n")
	return []byte(b.String())
}
