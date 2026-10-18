package fileedit

import "testing"

func TestArchiveRule(t *testing.T) {
	for _, c := range []struct {
		name             string
		withhold, redact bool
	}{
		{"config/paper-global.yml", true, false},
		{"./config/paper-global.yml", true, false},
		{"config//paper-global.yml", true, false},
		{"server.properties", false, true},
		{"./server.properties", false, true},
		{"plugins/server.properties", false, false},
		{"plugins/config/paper-global.yml", false, false},
		{"config/paper.yml", false, false},
	} {
		if w, r := ArchiveRule(c.name); w != c.withhold || r != c.redact {
			t.Errorf("ArchiveRule(%q) = %v, %v; want %v, %v", c.name, w, r, c.withhold, c.redact)
		}
	}
}
