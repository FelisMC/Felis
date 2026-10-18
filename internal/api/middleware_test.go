package api

import (
	"regexp"
	"strings"
	"testing"
)

var genRequestIDRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestRequestIDValidation(t *testing.T) {
	h := newTestAPI(newFakeRepo(), newFakeCluster()).ExternalHandler()

	assignedID := func(reqID string) string {
		var hdr map[string]string
		if reqID != "" {
			hdr = map[string]string{"X-Request-Id": reqID}
		}
		return do(h, "GET", "/healthz", "", hdr).Header().Get("X-Request-Id")
	}

	const good = "trace-abc_123.DEF"
	if got := assignedID(good); got != good {
		t.Fatalf("valid X-Request-Id: assigned %q, want it echoed as %q", got, good)
	}

	// Absent → the server mints one.
	if got := assignedID(""); !genRequestIDRE.MatchString(got) {
		t.Fatalf("absent X-Request-Id: assigned %q, want a generated 16-hex id", got)
	}

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
