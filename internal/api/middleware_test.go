package api

import (
	"regexp"
	"strings"
	"testing"
)

// genRequestIDRE matches a server-minted request id: newRequestID emits 8 random
// bytes as lowercase hex, i.e. exactly 16 hex characters.
var genRequestIDRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// TestRequestIDValidation pins the audit-integrity guard on withRequestID. A
// caller-supplied X-Request-Id lands in the response header, the error envelope,
// AND audit_logs.request_id, so a well-formed id is honored verbatim (the trace
// link is worth keeping) but anything oversized or carrying a stray byte is dropped
// and replaced with a fresh server-minted id — never echoed or persisted. /healthz
// is Public, so withRequestID (part of the shared baseChain) runs without auth.
func TestRequestIDValidation(t *testing.T) {
	h := newTestAPI(newFakeRepo(), newFakeCluster()).ExternalHandler()

	assignedID := func(reqID string) string {
		var hdr map[string]string
		if reqID != "" {
			hdr = map[string]string{"X-Request-Id": reqID}
		}
		return do(h, "GET", "/healthz", "", hdr).Header().Get("X-Request-Id")
	}

	// A well-formed inbound id is honored verbatim (cross-service trace link kept).
	const good = "trace-abc_123.DEF"
	if got := assignedID(good); got != good {
		t.Fatalf("valid X-Request-Id: assigned %q, want it echoed as %q", got, good)
	}

	// Absent → the server mints one.
	if got := assignedID(""); !genRequestIDRE.MatchString(got) {
		t.Fatalf("absent X-Request-Id: assigned %q, want a generated 16-hex id", got)
	}

	// Every malformed id is rejected and replaced with a fresh generated id, never
	// reflected back — the load-bearing property for audit-log integrity.
	for _, bad := range []struct{ name, id string }{
		{"too long", strings.Repeat("a", maxRequestIDLen+1)},
		{"space", "has space"},
		{"crlf injection", "abc\r\ndef"},
		{"path separator", "a/b"},
		{"multibyte", "trace-ünïcödé"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			got := assignedID(bad.id)
			if got == bad.id {
				t.Fatalf("rejected id was echoed back verbatim: %q", got)
			}
			if !genRequestIDRE.MatchString(got) {
				t.Fatalf("replacement %q is not a generated 16-hex id", got)
			}
		})
	}
}
