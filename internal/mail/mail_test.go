package mail

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMessageShape pins the one mail Felis sends: CRLF line endings throughout
// (RFC 5322 — a bare LF is how relays mangle or reject a message), the code in
// the body, a Q-encoded subject (it carries non-ASCII), and a blank line
// separating headers from body.
func TestMessageShape(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	msg := string(message("felis@example.net", "player@example.org", "042137", now))

	if strings.Contains(strings.ReplaceAll(msg, "\r\n", ""), "\n") {
		t.Error("message contains a bare LF; every line must end CRLF")
	}
	headers, body, ok := strings.Cut(msg, "\r\n\r\n")
	if !ok {
		t.Fatal("message has no blank line between headers and body")
	}
	for _, want := range []string{
		"From: felis@example.net",
		"To: player@example.org",
		"Date: Mon, 20 Jul 2026 12:00:00 +0000",
		"Content-Type: text/plain; charset=utf-8",
	} {
		if !strings.Contains(headers, want) {
			t.Errorf("headers missing %q:\n%s", want, headers)
		}
	}
	// The subject carries 验证码, so it must be MIME-encoded, never raw UTF-8.
	if !strings.Contains(headers, "Subject: =?utf-8?") {
		t.Errorf("subject must be Q-encoded, got headers:\n%s", headers)
	}
	if !strings.Contains(body, "042137") {
		t.Errorf("body missing the code:\n%s", body)
	}
}

// TestSelfTestCarriesNoCode keeps Ping's message distinguishable from a real
// OTP: an operator finding it in the inbox must read it as the wizard's proof
// of delivery, not as a code someone requested on their account.
func TestSelfTestCarriesNoCode(t *testing.T) {
	msg := string(selfTest("felis@example.net", time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)))
	if strings.Contains(msg, "verification code") {
		t.Errorf("self-test must not read as an OTP mail:\n%s", msg)
	}
	if !strings.Contains(msg, "To: felis@example.net") {
		t.Errorf("self-test must be addressed to the sender itself:\n%s", msg)
	}
}

// TestNoticeShape pins the second message shape — the reaper's pre-deletion
// warning: CRLF throughout even when the caller's body used bare LFs, a
// Q-encoded subject when it carries non-ASCII, and the caller's text rendered
// verbatim between the header block and the wire.
func TestNoticeShape(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	msg := string(notice("felis@example.net", "player@example.org",
		"Felis: 服务器 survival 将回收", "line one\nline two\n", now))

	if strings.Contains(strings.ReplaceAll(msg, "\r\n", ""), "\n") {
		t.Error("notice contains a bare LF; every line must end CRLF")
	}
	headers, body, ok := strings.Cut(msg, "\r\n\r\n")
	if !ok {
		t.Fatal("notice has no blank line between headers and body")
	}
	if !strings.Contains(headers, "To: player@example.org") {
		t.Errorf("headers missing To:\n%s", headers)
	}
	if !strings.Contains(headers, "Subject: =?utf-8?") {
		t.Errorf("non-ASCII subject must be Q-encoded:\n%s", headers)
	}
	if !strings.Contains(body, "line one\r\nline two\r\n") {
		t.Errorf("body must be CRLF-normalized verbatim text:\n%q", body)
	}
}

// fakeRelay speaks just enough SMTP for net/smtp, answering 250 to MAIL FROM
// and RCPT TO but dataVerdict at end-of-DATA. That split is the entire point:
// relays which validate sender identity (Fastmail among them) accept MAIL FROM
// unconditionally and only render their verdict after the message body, so a
// probe stopping short of end-of-DATA reports a green relay that cannot send.
func fakeRelay(t *testing.T, dataVerdict string) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeRelay(conn, dataVerdict)
		}
	}()
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func serveFakeRelay(conn net.Conn, dataVerdict string) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	io.WriteString(conn, "220 fake ESMTP\r\n")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			io.WriteString(conn, "250 fake\r\n")
		case strings.HasPrefix(cmd, "MAIL FROM"), strings.HasPrefix(cmd, "RCPT TO"):
			io.WriteString(conn, "250 2.1.0 Ok\r\n")
		case strings.HasPrefix(cmd, "DATA"):
			io.WriteString(conn, "354 go ahead\r\n")
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
			}
			io.WriteString(conn, dataVerdict+"\r\n")
		case strings.HasPrefix(cmd, "QUIT"):
			io.WriteString(conn, "221 bye\r\n")
			return
		default:
			io.WriteString(conn, "250 ok\r\n")
		}
	}
}

// TestPingRejectsRelayThatRefusesAtEndOfData is the regression this whole
// change exists for. A live install passed the wizard with a From on a domain
// the relay would not send as, then failed every OTP: the old Ping stopped at
// NOOP, and even a MAIL FROM probe would have been waved through with a 250.
// Only a full transaction sees the refusal.
func TestPingRejectsRelayThatRefusesAtEndOfData(t *testing.T) {
	host, port := fakeRelay(t, "550 5.7.1 sender identity not allowed")
	s := &SMTP{Host: host, Port: port, From: "noreply@example.net"}

	err := s.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping accepted a relay that refuses this sender at end-of-DATA")
	}
	// The operator has to be able to act on this, which means seeing both the
	// relay's own refusal and which address it refused.
	if !strings.Contains(err.Error(), "550") {
		t.Errorf("want the relay's refusal surfaced, got: %v", err)
	}
	if !strings.Contains(err.Error(), "noreply@example.net") {
		t.Errorf("want the rejected sender named, got: %v", err)
	}
}

// The counterpart: a relay that accepts the message must leave the wizard happy.
func TestPingAcceptsDeliverableRelay(t *testing.T) {
	host, port := fakeRelay(t, "250 2.0.0 Ok")
	s := &SMTP{Host: host, Port: port, From: "noreply@example.net"}

	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping rejected a relay that accepted the message: %v", err)
	}
}

// SendOTP must fail on the same relay Ping rejects — they share deliver(), and
// that shared path is what makes the wizard's check meaningful.
func TestSendOTPSurfacesEndOfDataRefusal(t *testing.T) {
	host, port := fakeRelay(t, "550 5.7.1 sender identity not allowed")
	s := &SMTP{Host: host, Port: port, From: "noreply@example.net"}

	err := s.SendOTP(context.Background(), "player@example.org", "042137")
	if err == nil {
		t.Fatal("SendOTP reported success against a relay that refused the message")
	}
	if !strings.Contains(err.Error(), "550") {
		t.Errorf("want the relay's refusal surfaced, got: %v", err)
	}
}
