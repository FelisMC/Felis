package main

import (
	"strings"
	"testing"
)

func TestWrapDisplayURL(t *testing.T) {
	u := "https://op.console.example.net/setup?token=" + strings.Repeat("A", 43)
	lines := wrapDisplayURL(u, 70)
	if got := strings.Join(lines, ""); got != u {
		t.Fatalf("concatenated lines = %q, want the original URL back", got)
	}
	for i, l := range lines {
		if len(l) > 70 {
			t.Errorf("line %d is %d cols wide: %q", i, len(l), l)
		}
	}
	if len(lines) < 2 || !strings.HasSuffix(lines[0], "token=") {
		t.Fatalf("want the first line to end at the 'token=' boundary, got %q", lines)
	}
	short := "https://a/b"
	if got := wrapDisplayURL(short, 70); len(got) != 1 || got[0] != short {
		t.Errorf("short URL should pass through unsplit, got %q", got)
	}
}
