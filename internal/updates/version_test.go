package updates

import "testing"

func TestParseTolerant(t *testing.T) {
	cases := []struct {
		in                  string
		major, minor, patch int
		pre                 string
	}{
		{"1.2.3", 1, 2, 3, ""},
		{"v1.2.3", 1, 2, 3, ""},                       // leading v
		{"V1.2.3", 1, 2, 3, ""},                       // leading V
		{"v1.30.2+k3s1", 1, 30, 2, ""},                // k3s build suffix ignored
		{"1.30.2+k3s1", 1, 30, 2, ""},                 // build suffix, no v
		{"2024.2.1", 2024, 2, 1, ""},                  // cloudflared calendar version
		{"1.2.3-rc.1", 1, 2, 3, "rc.1"},               // prerelease
		{"v3.3.0-SNAPSHOT", 3, 3, 0, "SNAPSHOT"},      // velocity-style
		{"1.2.3-rc.1+build.9", 1, 2, 3, "rc.1"},       // prerelease AND build
		{"v0.0.0+g1a2b3c4", 0, 0, 0, ""},              // stamp of a build pinned to a ref with no tag behind it
		{"v2", 2, 0, 0, ""},                           // missing minor/patch fill 0
		{"2.0", 2, 0, 0, ""},                          // missing patch fills 0
		{"  v1.2.3  ", 1, 2, 3, ""},                   // surrounding whitespace
	}
	for _, c := range cases {
		v, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", c.in, err)
			continue
		}
		if v.Major != c.major || v.Minor != c.minor || v.Patch != c.patch || v.Prerelease != c.pre {
			t.Errorf("Parse(%q) = {%d.%d.%d-%q}, want {%d.%d.%d-%q}",
				c.in, v.Major, v.Minor, v.Patch, v.Prerelease, c.major, c.minor, c.patch, c.pre)
		}
	}
}

// TestParseFailsClosed proves a garbled version is an error, never a silent 0.0.0
// that would read as "older than everything" and trigger a spurious upgrade.
func TestParseFailsClosed(t *testing.T) {
	bad := []string{"", "   ", "vx.y.z", "1.2.x", "1.2.3.4", "abc", "-1.2.3", "1.-2.3"}
	for _, in := range bad {
		if v, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %+v, want error", in, v)
		}
	}
}

func TestCompareAndAfter(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.4", "1.2.3", 1},
		{"1.2.3", "1.2.4", -1},
		{"1.3.0", "1.2.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"v1.30.2+k3s1", "v1.30.2+k3s2", 0}, // build metadata ignored for ordering
		{"1.30.3+k3s1", "1.30.2+k3s9", 1},   // core wins over build
		{"1.2.3", "1.2.3-rc.1", 1},          // release > prerelease
		{"1.2.3-rc.1", "1.2.3", -1},         // prerelease < release
		{"1.2.3-rc.1", "1.2.3-rc.2", -1},    // numeric prerelease identifiers
		{"1.2.3-rc.2", "1.2.3-rc.10", -1},   // numeric, not lexical (2 < 10)
		{"1.2.3-alpha", "1.2.3-beta", -1},   // alphanumeric lexical
		{"1.2.3-rc.1", "1.2.3-rc.1.1", -1},  // longer identifier set is higher
		{"1.2.3-1", "1.2.3-alpha", -1},      // numeric identifier sorts below alphanumeric

		// A dev build's own stamp, "<tag>+g<sha>", against the tag it is built past.
		// It must read EQUAL, never newer: deploy/bootstrap.sh's dev channel stamps the
		// binary this way, so if metadata counted for ordering every dev install would
		// report an upgrade onto a release it already contains. The "+" spelling exists
		// precisely to buy this, and the mixed case -- metadata on one side only -- is
		// the one the k3s pair above does not exercise. The last pair carries the
		// metadata after a prerelease tail, the order a real earlyAccess build stamps.
		{"v1.2.3+g1a2b3c4", "v1.2.3", 0},
		{"v1.2.3", "v1.2.3+g1a2b3c4", 0},
		{"v1.2.3+g1a2b3c4", "v1.2.4", -1},
		{"v1.0.0-earlyAccess+g1a2b3c4", "v1.0.0-earlyAccess", 0},
	}
	for _, c := range cases {
		va, err := Parse(c.a)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.a, err)
		}
		vb, err := Parse(c.b)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.b, err)
		}
		if got := va.Compare(vb); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		// After is the strict-newer predicate the plan engine relies on.
		if got := va.After(vb); got != (c.want > 0) {
			t.Errorf("After(%q, %q) = %v, want %v", c.a, c.b, got, c.want > 0)
		}
	}
}

func TestIsPrerelease(t *testing.T) {
	for _, in := range []string{"1.2.3-rc.1", "v3.3.0-SNAPSHOT", "1.0.0-beta"} {
		v, _ := Parse(in)
		if !v.IsPrerelease() {
			t.Errorf("IsPrerelease(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"1.2.3", "v1.30.2+k3s1", "2024.2.1"} {
		v, _ := Parse(in)
		if v.IsPrerelease() {
			t.Errorf("IsPrerelease(%q) = true, want false", in)
		}
	}
}

// TestStringRoundTrips proves a report shows exactly what upstream published,
// including the "+k3s1" a human needs to recognize the build.
func TestStringRoundTrips(t *testing.T) {
	for _, in := range []string{"v1.30.2+k3s1", "1.2.3-rc.1", "2024.2.1"} {
		v, _ := Parse(in)
		if v.String() != in {
			t.Errorf("String() = %q, want round-trip of %q", v.String(), in)
		}
	}
}
