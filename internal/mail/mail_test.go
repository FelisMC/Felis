package mail

import (
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
