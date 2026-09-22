package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// The request log prints text the caller chose. A bidi override must not reorder the line,
// an invalid byte must not make journald store the entry as a blob, and a huge query must
// not become a huge log line. serverId is left out so the handler answers without asking
// any source.
func TestNanoRequestLogIsQuotedAndCapped(t *testing.T) {
	const rlo = rune(0x202e) // RIGHT-TO-LEFT OVERRIDE
	var log bytes.Buffer
	h := nanoHandler(nil, &log)
	target := "/session/minecraft/hasJoined?username=" + string(rlo) + "evil" + string([]byte{0x9b}) + "31m" + strings.Repeat("a", 4096)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))

	line := log.String()
	if strings.ContainsRune(line, rlo) || !utf8.ValidString(line) {
		t.Fatalf("raw caller bytes reached the log: %q", line)
	}
	if escaped := strings.Trim(strconv.QuoteRune(rlo), "'"); !strings.Contains(line, escaped) {
		t.Fatalf("log line %q should show the override escaped as %s", line, escaped)
	}
	if len(line) > 2*nanoLogURIMax {
		t.Fatalf("log line is %d bytes for a %d-byte URI; want it capped", len(line), len(target))
	}
}
