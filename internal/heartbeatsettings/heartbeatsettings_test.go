package heartbeatsettings

import (
	"strings"
	"testing"
)

func TestCheckAndRedact(t *testing.T) {
	for _, ok := range []string{"https://hc-ping.com/abc", "http://monitor.lan:8000/ping/x"} {
		if err := Check(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "hc-ping.com/abc", "ftp://x/y", "https:///path", "https://x/a b", "https://x/a\n"} {
		err := Check(bad)
		if err == nil {
			t.Errorf("%q passed", bad)
		} else if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("the error repeats the URL: %v", err)
		}
	}
	if got := Redact("https://hc-ping.com/secret-uuid"); got != "https://hc-ping.com/..." {
		t.Errorf("redact = %q", got)
	}
	if got := Redact("nonsense"); strings.Contains(got, "nonsense") {
		t.Errorf("redact leaked %q", got)
	}
}

func TestParse(t *testing.T) {
	for _, raw := range []string{"", "null"} {
		if s, err := Parse([]byte(raw)); s != nil || err != nil {
			t.Errorf("Parse(%q) = %v, %v", raw, s, err)
		}
	}
	if _, err := Parse([]byte(`{"url":"ftp://x"}`)); err == nil {
		t.Error("a bad URL parsed")
	}
	if s, err := Parse([]byte(`{"url":"https://hc-ping.com/abc"}`)); err != nil || s.URL != "https://hc-ping.com/abc" {
		t.Errorf("Parse = %+v, %v", s, err)
	}
}
