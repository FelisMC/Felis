// Package mail delivers one-time codes and notices through an SMTP relay.
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
	"syscall"
	"time"
)

// sendTimeout bounds the whole SMTP conversation when the caller has no deadline.
const sendTimeout = 30 * time.Second

// Password contains the resolved secret, not its configuration reference.
type SMTP struct {
	Host     string
	Port     int
	From     string
	Username string
	Password string
	// RequireTLS rejects non-465 relays without STARTTLS.
	RequireTLS bool
	// Control validates the resolved relay address before connecting.
	Control func(network, address string, c syscall.RawConn) error
}

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

// SendNotice normalizes body line endings to CRLF.
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

// Ping sends a self-test to From; success does not prove permission to send to external recipients.
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
	// Relays may reject the sender at end-of-DATA; include From in the error.
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: relay refused mail from %s: %w", s.From, err)
	}
	return nil
}

// connect uses implicit TLS on 465, STARTTLS otherwise, with one deadline for the entire conversation.
func (s *SMTP) connect(ctx context.Context) (*smtp.Client, error) {
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(sendTimeout)
	}
	dialer := &net.Dialer{Deadline: deadline, Control: s.Control}

	var conn net.Conn
	var err error
	if s.Port == 465 {
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
		// PlainAuth independently rejects credentials on an unencrypted connection.
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			c.Close()
			return nil, fmt.Errorf("smtp: auth as %s: %w", s.Username, err)
		}
	}
	return c, nil
}

// headers uses CRLF and Q-encodes non-ASCII subjects.
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

func selfTest(from string, now time.Time) []byte {
	var b strings.Builder
	b.WriteString(headers(from, from, "Felis SMTP 自检 · relay self-test", now))
	b.WriteString("Felis accepted this relay because it accepted this message.\r\n")
	b.WriteString("Felis 已确认该邮件中继可用：本邮件即为投递证明。\r\n")
	b.WriteString("\r\n")
	b.WriteString("Sent by `felis setup` when the SMTP relay was configured. / 由 `felis setup` 配置 SMTP 时发出。\r\n")
	return []byte(b.String())
}

func notice(from, to, subject, body string, now time.Time) []byte {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	if !strings.HasSuffix(body, "\r\n") {
		body += "\r\n"
	}
	return []byte(headers(from, to, subject, now) + body)
}
