package main

import (
	"net/http"
	"testing"

	"felis.lolicon.best/internal/config"
)

// TestAuthSourcesFromConfig pins the one place the hasJoined identity anchor is decided:
// Mojang is prepended in code, first, and is the only source whose UUIDs are trusted as-is.
// The empty case matters on its own — both `felis api` and `felis nano` call this with a
// config that has no [[auth_source]] at all, and that has to be a Mojang-only relay rather
// than an empty list that rejects every login.
func TestAuthSourcesFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured []config.AuthSourceConfig
	}{
		{"no configured sources", nil},
		{"configured sources", []config.AuthSourceConfig{
			{Tag: "littleskin", Prefix: "LS", URL: "https://littleskin.example/hasJoined"},
			{Tag: "guild", Prefix: "GD", URL: "https://guild.example/hasJoined"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := authSourcesFromConfig(tc.configured)
			if len(got) != len(tc.configured)+1 {
				t.Fatalf("got %d sources, want Mojang + %d configured", len(got), len(tc.configured))
			}
			if got[0].Tag != "mojang" || got[0].URL != mojangSessionServer || !got[0].Identity {
				t.Errorf("first source = %+v, want the Mojang identity anchor", got[0])
			}
			for i, c := range tc.configured {
				s := got[i+1]
				if s.Identity {
					t.Errorf("configured source %q is marked Identity; only Mojang may be", c.Tag)
				}
				if s.Tag != c.Tag || s.Prefix != c.Prefix || s.URL != c.URL {
					t.Errorf("source %d = %+v, want %+v in config order", i+1, s, c)
				}
			}
		})
	}
}

// TestNewAPIServerSetsHardenedTimeouts pins the gosec-G112 hardening on every
// felis-api listener: the shared factory must bound the header and idle phases
// (Slowloris + idle-connection exhaustion) while leaving WriteTimeout UNSET, because
// the external and https faces stream Server-Sent Events for the life of a client's
// console/build-log attachment and a WriteTimeout would sever a healthy long stream.
func TestNewAPIServerSetsHardenedTimeouts(t *testing.T) {
	srv := newAPIServer(":0", http.NewServeMux())

	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want a positive Slowloris bound", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want a positive idle-connection bound", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (unset) so long-lived SSE streams are not severed", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 (unset) so a slow SSE attach is not capped", srv.ReadTimeout)
	}
}
