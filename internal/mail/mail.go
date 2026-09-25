// Package mail is the SMTP implementation of the api.OTPMailer seam: it
// delivers the email one-time codes the passwordless doors mint (onboarding,
// email login, op-login) through the relay configured in felis.toml [smtp].
// It is deliberately tiny — two message shapes, stdlib net/smtp — because the
// only mail Felis ever sends is a six-digit code plus the reaper's pre-deletion
// notice (SendNotice).
//
// TLS posture: port 465 dials implicit TLS; any other port dials plaintext and
// upgrades via STARTTLS. With RequireTLS set (the default for any relay not on
// this host, config.SMTPConfig.TLSRequired) a relay that does not offer
// STARTTLS is refused before a single address or code is sent, so a relay
// without TLS, or a path that strips the offer, fails loudly. AUTH is
// attempted only when a username is configured, and net/smtp's PlainAuth
// itself refuses to send credentials over an unencrypted connection.
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
	// RequireTLS refuses a relay on a port other than 465 that does not offer
	// STARTTLS. Callers set it from config.SMTPConfig.TLSRequired.
	RequireTLS bool
}

// SendOTP mails code to email as a small bilingual plain-text message. It is
// the api.OTPMailer implementation felis-api wires when [smtp] is configured.
func (s *SMTP) SendOTP(ctx context.Context, email, code string) error {
	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := s.deliver(c, email, message(s.From, email, code, time.Now())); err != nil {
		return err
	}
	return c.Quit()
}

// SendNotice mails one operator-composed notice to email — the reaper's
// pre-deletion warning is its only caller. Subject and body are the caller's;
// the body is CRLF-normalized so a multi-line string renders as one text/plain
// message. Delivery errors surface exactly like SendOTP's, so the caller can
// retry on its own cadence.
func (s *SMTP) SendNotice(ctx context.Context, email, subject, body string) error {
	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := s.deliver(c, email, notice(s.From, email, subject, body, time.Now())); err != nil {
		return err
	}
	return c.Quit()
}

// Ping proves the configured relay will actually ACCEPT mail from this sender,
// by running a complete transaction — connect, (STARTTLS,) AUTH, MAIL FROM,
// RCPT TO, DATA — and delivering a short self-test message to From itself. The
// setup wizard runs it before writing anything, so a bad relay fails at the
// keyboard instead of at the first code a player is waiting on.
//
// It really does send that one message, and it has to: a probe that stops at
// NOOP (or even at MAIL FROM) proves nothing about deliverability, because
// relays which validate sender identity answer MAIL FROM with an unconditional
// 250 and defer the verdict to end-of-DATA. Fastmail does exactly that, and a
// NOOP-only Ping green-lit a From on a domain the account could not send as —
// every OTP after it died at w.Close() with the wizard reporting success.
//
// What it does NOT prove is that the relay will let this From reach anyone
// else. The self-test is addressed to From, which is a mailbox inside the
// relay account, and a relay that gates sender identity at end-of-DATA gates
// it on the way OUT: Fastmail answers 250 for noreply@a.example → the
// account's own mailbox and 551 5.7.1 "Not authorised to send from this
// header address" for that same From → any external recipient. Only the
// account's exact authorized identity passes the second one — another
// local-part on the same domain is refused too. So a green Ping means
// connect/TLS/AUTH/message-shape are good; the operator still has to have
// authorized From as a sending identity with their provider, and the first
// player OTP is what proves they did. Addressing the self-test elsewhere
// would not fix this — the only mailbox an operator can check is usually
// inside the account as well — so the honest move is to say so here rather
// than to buy false confidence with a bigger probe.
func (s *SMTP) Ping(ctx context.Context) error {
	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := s.deliver(c, s.From, selfTest(s.From, time.Now())); err != nil {
		return err
	}
	return c.Quit()
}

// deliver runs one MAIL FROM → RCPT TO → DATA transaction on an established
// client. SendOTP and Ping share it so the wizard's check exercises the exact
// path a player's code takes — a check that skips a step is a check that can
// pass while the real send fails.
func (s *SMTP) deliver(c *smtp.Client, to string, msg []byte) error {
	if err := c.Mail(s.From); err != nil {
		return fmt.Errorf("smtp: MAIL FROM %s: %w", s.From, err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("smtp: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp: write message: %w", err)
	}
	// End-of-DATA is where a relay renders its real verdict on the sender, so
	// this error names From: "550 …" here almost always means the account is
	// not allowed to send as that address.
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: relay refused mail from %s: %w", s.From, err)
	}
	return nil
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
		} else if s.RequireTLS {
			c.Close()
			return nil, fmt.Errorf("smtp: %s does not offer STARTTLS, so mail to it would cross the network unencrypted; "+
				"use port 465 or a relay with STARTTLS, or set [smtp] require_tls = false for a relay you reach over a trusted link", addr)
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

// headers renders the RFC 5322 header block every Felis message shares: CRLF
// throughout, the subject Q-encoded because both subjects carry non-ASCII, and
// the blank line that ends the block.
func headers(from, to, subject string, now time.Time) string {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	return b.String()
}

// message renders the one mail players receive: a short bilingual plain-text
// body carrying the code. Split out from SendOTP so the shape is testable
// without a relay.
func message(from, to, code string, now time.Time) []byte {
	var b strings.Builder
	b.WriteString(headers(from, to, "Felis 验证码 · verification code", now))
	b.WriteString("Your Felis verification code / Felis 验证码:\r\n")
	b.WriteString("\r\n")
	b.WriteString("    " + code + "\r\n")
	b.WriteString("\r\n")
	b.WriteString("If you didn't request this, ignore this message. / 若非本人操作，请忽略此邮件。\r\n")
	return []byte(b.String())
}

// selfTest renders the message Ping delivers to the sender itself. It carries
// no code and says why it arrived, so an operator who finds it in the inbox
// reads it as the wizard's proof of delivery rather than a stray OTP.
func selfTest(from string, now time.Time) []byte {
	var b strings.Builder
	b.WriteString(headers(from, from, "Felis SMTP 自检 · relay self-test", now))
	b.WriteString("Felis accepted this relay because it accepted this message.\r\n")
	b.WriteString("Felis 已确认该邮件中继可用：本邮件即为投递证明。\r\n")
	b.WriteString("\r\n")
	b.WriteString("Sent by `felis setup` when the SMTP relay was configured. / 由 `felis setup` 配置 SMTP 时发出。\r\n")
	return []byte(b.String())
}

// notice renders an operator notice: the shared header block plus the caller's
// body, CRLF-normalized so every line obeys RFC 5322 regardless of which line
// endings the caller's format string produced.
func notice(from, to, subject, body string, now time.Time) []byte {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	if !strings.HasSuffix(body, "\r\n") {
		body += "\r\n"
	}
	return []byte(headers(from, to, subject, now) + body)
}
