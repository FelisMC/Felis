package naming_test

import (
	"testing"

	"felis.lolicon.best/internal/naming"
)

func TestValidateServerName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"survival", true},
		{"creative-2", true},
		{"abc", true},
		{"a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6", true}, // 32 chars
		{"ab", false}, // too short
		{"a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q", false}, // 33 chars
		{"Survival", false},                          // uppercase
		{"has_underscore", false},                    // illegal char
		{"has space", false},                         // illegal char
		{"-leading", false},                          // leading hyphen
		{"trailing-", false},                         // trailing hyphen
		{"login", false},                             // reserved system server
		{"lobby", false},                             // reserved
		{"admin", false},                             // reserved
		{"api", false},                               // reserved
		{"console", false},                           // reserved web console host
	}
	for _, c := range cases {
		err := naming.ValidateServerName(c.name)
		if c.ok && err != nil {
			t.Errorf("ValidateServerName(%q) = %v, want ok", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateServerName(%q) = nil, want error", c.name)
		}
	}
}

// ValidateSystemServerName keeps the format rule but drops the reservation
// check, so the platform can provision the reserved system names (login, lobby)
// that ValidateServerName correctly refuses to hand to users.
func TestIsSystemServer(t *testing.T) {
	// Exactly the platform's two system services answer true; a user server that
	// merely sounds systemic does not.
	for _, name := range []string{"login", "lobby"} {
		if !naming.IsSystemServer(name) {
			t.Errorf("IsSystemServer(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"survival", "admin", "login2", "", "lobby-"} {
		if naming.IsSystemServer(name) {
			t.Errorf("IsSystemServer(%q) = true, want false", name)
		}
	}
}

func TestValidateSystemServerName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"login", true}, // reserved, but a legal system service
		{"lobby", true}, // reserved, but a legal system service
		{"admin", true}, // reserved names are allowed on this path
		{"survival", true},
		{"ab", false},        // still too short
		{"Login", false},     // still case-sensitive
		{"-leading", false},  // still no leading hyphen
		{"has space", false}, // still no illegal chars
	}
	for _, c := range cases {
		err := naming.ValidateSystemServerName(c.name)
		if c.ok && err != nil {
			t.Errorf("ValidateSystemServerName(%q) = %v, want ok", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateSystemServerName(%q) = nil, want error", c.name)
		}
	}

	// The two paths must genuinely differ on system names: the user path
	// refuses them while the system path accepts them.
	for _, name := range []string{"login", "lobby"} {
		if naming.ValidateServerName(name) == nil {
			t.Errorf("ValidateServerName(%s) accepted; reserved name must be refused for users", name)
		}
		if naming.ValidateSystemServerName(name) != nil {
			t.Errorf("ValidateSystemServerName(%s) refused; system path must accept it", name)
		}
	}
}

func TestWorldPVCName(t *testing.T) {
	cases := map[string]string{
		"survival":   "world-survival-0",
		"creative-2": "world-creative-2-0",
	}
	for server, want := range cases {
		if got := naming.WorldPVCName(server); got != want {
			t.Errorf("WorldPVCName(%q) = %q, want %q", server, got, want)
		}
	}
}

func TestHostname(t *testing.T) {
	const root = "mc.example.net"
	got, err := naming.Hostname("survival", root)
	if err != nil {
		t.Fatalf("Hostname: %v", err)
	}
	if got != "survival.mc.example.net" {
		t.Errorf("Hostname = %q, want survival.mc.example.net", got)
	}
	if _, err := naming.Hostname("lobby", root); err == nil {
		t.Error("Hostname should reject a reserved subdomain")
	}
	if _, err := naming.Hostname("survival", ""); err == nil {
		t.Error("Hostname should reject an empty root domain")
	}
}

func TestValidateHostname(t *testing.T) {
	const root = "mc.example.net"
	cases := []struct {
		host string
		ok   bool
	}{
		{"survival.mc.example.net", true},
		{"a.mc.example.net", true},
		{"deep.sub.mc.example.net", false}, // not a single label under root
		{"survival.evil.example.org", false},
		{"mc.example.net", false}, // bare root, no label
		{".mc.example.net", false},
		{"-bad.mc.example.net", false},
	}
	for _, c := range cases {
		err := naming.ValidateHostname(c.host, root)
		if c.ok && err != nil {
			t.Errorf("ValidateHostname(%q) = %v, want ok", c.host, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateHostname(%q) = nil, want error", c.host)
		}
	}
	if err := naming.ValidateHostname("x.mc.example.net", ""); err == nil {
		t.Error("ValidateHostname should reject an empty root domain")
	}
}

func TestCleanDisplayName(t *testing.T) {
	str := func(rs ...rune) string { return string(rs) }
	rainbow := str(0x1F3F3, 0xFE0F, 0x200D, 0x1F308)
	cjk := func(n int) string {
		s := ""
		for range n {
			s += "生"
		}
		return s
	}
	for _, c := range []struct {
		name, in, want string
		ok             bool
	}{
		{"trimmed", "  Survival World  ", "Survival World", true},
		{"empty", "", "", true},
		{"only spaces", "   ", "", true},
		{"cjk and punctuation", "生存服 · 第二季!", "生存服 · 第二季!", true},
		{"emoji joined by zwj", "Pride " + rainbow, "Pride " + rainbow, true},
		{"ideographic space inside", "生存" + str(0x3000) + "服", "生存" + str(0x3000) + "服", true},
		{"64 characters of 3 bytes each", cjk(64), cjk(64), true},
		{"65 characters", cjk(65), "", false},
		{"tab", "a" + str(0x09) + "b", "", false},
		{"newline", "a" + str(0x0A) + "b", "", false},
		{"nul", "a" + str(0x00), "", false},
		{"right-to-left override", "abc" + str(0x202E) + "exe.txt", "", false},
		{"zero-width space", "ad" + str(0x200B) + "min", "", false},
		{"line separator", "a" + str(0x2028) + "b", "", false},
		{"private use", "a" + str(0xE000), "", false},
		{"invalid utf-8", string([]byte{'a', 0xff}), "", false},
	} {
		got, err := naming.CleanDisplayName(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("%s: CleanDisplayName(%q) = %q, %v; want %q, ok=%v", c.name, c.in, got, err, c.want, c.ok)
		}
	}
}
