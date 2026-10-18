package mail

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

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
	// The subject carries a non-ASCII word, so it must be MIME-encoded.
	if !strings.Contains(headers, "Subject: =?utf-8?") {
		t.Errorf("subject must be Q-encoded, got headers:\n%s", headers)
	}
	if !strings.Contains(body, "042137") {
		t.Errorf("body missing the code:\n%s", body)
	}
}

func TestSelfTestCarriesNoCode(t *testing.T) {
	msg := string(selfTest("felis@example.net", time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)))
	if strings.Contains(msg, "verification code") {
		t.Errorf("self-test must not read as an OTP mail:\n%s", msg)
	}
	if !strings.Contains(msg, "To: felis@example.net") {
		t.Errorf("self-test must be addressed to the sender itself:\n%s", msg)
	}
}

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

// Decide sender acceptance at end-of-DATA so SMTP probes must complete the transaction.
func fakeRelay(t *testing.T, dataVerdict string) (string, int) {
	t.Helper()
	return startRelay(t, &relayOpts{dataVerdict: dataVerdict})
}

// relayOpts shapes a fake relay: its end-of-DATA verdict, whether its EHLO
// offers STARTTLS (it cannot complete one: it answers 220 and hangs up), and
// the commands it received, in order.
type relayOpts struct {
	dataVerdict string
	starttls    bool

	mu   sync.Mutex
	seen []string
}

func (o *relayOpts) record(cmd string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, cmd)
}

func (o *relayOpts) commands() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.seen...)
}

func startRelay(t *testing.T, opts *relayOpts) (string, int) {
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
			go serveFakeRelay(conn, opts)
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

func serveFakeRelay(conn net.Conn, opts *relayOpts) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	io.WriteString(conn, "220 fake ESMTP\r\n")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			opts.record("<closed>")
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		if f := strings.Fields(cmd); len(f) > 0 {
			opts.record(f[0])
		}
		switch {
		case strings.HasPrefix(cmd, "EHLO") && opts.starttls:
			io.WriteString(conn, "250-fake\r\n250 STARTTLS\r\n")
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			io.WriteString(conn, "250 fake\r\n")
		case strings.HasPrefix(cmd, "STARTTLS"):
			io.WriteString(conn, "220 ready\r\n")
			return
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
			io.WriteString(conn, opts.dataVerdict+"\r\n")
		case strings.HasPrefix(cmd, "QUIT"):
			io.WriteString(conn, "221 bye\r\n")
			return
		default:
			io.WriteString(conn, "250 ok\r\n")
		}
	}
}

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

func TestPingAcceptsDeliverableRelay(t *testing.T) {
	host, port := fakeRelay(t, "250 2.0.0 Ok")
	s := &SMTP{Host: host, Port: port, From: "noreply@example.net"}

	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping rejected a relay that accepted the message: %v", err)
	}
}

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

func TestRequireTLSRefusesPlaintextRelay(t *testing.T) {
	relay := &relayOpts{dataVerdict: "250 2.0.0 Ok"}
	host, port := startRelay(t, relay)
	s := &SMTP{Host: host, Port: port, From: "noreply@example.net", RequireTLS: true}

	err := s.SendOTP(context.Background(), "player@example.org", "042137")
	if err == nil {
		t.Fatal("SendOTP sent a code through a relay without STARTTLS")
	}
	for _, want := range []string{"does not offer STARTTLS", host + ":" + strconv.Itoa(port), "require_tls = false"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// The client hangs up instead of leaving the connection to the garbage
	// collector; the relay sees that shortly after SendOTP returns.
	deadline := time.Now().Add(2 * time.Second)
	for len(relay.commands()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := strings.Join(relay.commands(), " "); got != "EHLO <closed>" {
		t.Errorf("relay received %q, want EHLO and then the connection closed", got)
	}
	if err := s.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Errorf("Ping = %v, want the STARTTLS refusal", err)
	}
}

func TestPlaintextRelayAllowedWithoutRequireTLS(t *testing.T) {
	relay := &relayOpts{dataVerdict: "250 2.0.0 Ok"}
	host, port := startRelay(t, relay)
	s := &SMTP{Host: host, Port: port, From: "noreply@example.net"}

	if err := s.SendOTP(context.Background(), "player@example.org", "042137"); err != nil {
		t.Fatalf("SendOTP: %v", err)
	}
	if got := strings.Join(relay.commands(), " "); got != "EHLO MAIL RCPT DATA QUIT" {
		t.Errorf("relay received %q, want EHLO MAIL RCPT DATA QUIT", got)
	}
}

func TestRequireTLSTakesAnOfferedStartTLS(t *testing.T) {
	relay := &relayOpts{dataVerdict: "250 2.0.0 Ok", starttls: true}
	host, port := startRelay(t, relay)
	s := &SMTP{Host: host, Port: port, From: "noreply@example.net", RequireTLS: true}

	err := s.SendOTP(context.Background(), "player@example.org", "042137")
	if err == nil || !strings.HasPrefix(err.Error(), "smtp: starttls:") {
		t.Fatalf("SendOTP = %v, want the STARTTLS upgrade failure", err)
	}
	if got := strings.Join(relay.commands(), " "); got != "EHLO STARTTLS" {
		t.Errorf("relay received %q, want EHLO STARTTLS", got)
	}
}
