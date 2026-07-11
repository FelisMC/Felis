package api

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// fakeYgg stands in for one upstream Yggdrasil root. It answers hasJoined with the
// given profile, or 204 when id == "" ("not my player") or a query field is missing —
// the same contract Mojang's real sessionserver honors.
func fakeYgg(t *testing.T, id, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if id == "" || q.Get("username") == "" || q.Get("serverId") == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": name,
			"properties": []map[string]string{{"name": "textures", "value": "SKIN", "signature": "SIG"}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func getHasJoined(h http.Handler, username, serverID string) *httptest.ResponseRecorder {
	return do(h, "GET", "/session/minecraft/hasJoined?username="+username+"&serverId="+serverID, "", nil)
}

func profileOf(t *testing.T, w *httptest.ResponseRecorder) sessionProfile {
	t.Helper()
	var p sessionProfile
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("profile body not JSON: %v (%q)", err, w.Body.String())
	}
	return p
}

// undashed is the 32-hex form the resolver must emit (authlib's GameProfile format).
func undashed(u uuid.UUID) string { return hex.EncodeToString(u[:]) }

const notchMojangID = "069a79f444e94726a5befca90e38aaf5" // a real Mojang-space UUID, undashed

func TestHasJoined(t *testing.T) {
	// A trusted (Mojang) source passes its UUID through byte-for-byte.
	t.Run("mojang identity passthrough", func(t *testing.T) {
		mojang := fakeYgg(t, notchMojangID, "Notch")
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{{Tag: "mojang", URL: mojang.URL, Identity: true}}

		w := getHasJoined(api.InternalHandler(), "Notch", "abc")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%q)", w.Code, w.Body.String())
		}
		p := profileOf(t, w)
		if p.ID != notchMojangID {
			t.Fatalf("mojang id = %q, want unchanged %q", p.ID, notchMojangID)
		}
		if len(p.Properties) != 1 {
			t.Fatalf("properties not relayed: %v", p.Properties)
		}
	})

	// THE security invariant: a self-asserted source claiming a Mojang-space UUID must
	// NOT be emitted as-is — it is rewritten into the per-source namespace. Without this,
	// a malicious third-party could impersonate any Mojang player with full UUID fidelity
	// and the reclaim/blacklist layer (keyed on "genuine Mojang has a different UUID")
	// could never catch it.
	t.Run("thirdparty UUID rewritten, never emitted as-is", func(t *testing.T) {
		evil := fakeYgg(t, notchMojangID, "Notch") // lies: returns real Notch's Mojang UUID
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{{Tag: "littleskin", URL: evil.URL, Identity: false}}

		w := getHasJoined(api.InternalHandler(), "Notch", "abc")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", w.Code)
		}
		got := profileOf(t, w).ID
		if got == notchMojangID {
			t.Fatalf("SECURITY: third-party Mojang-space UUID emitted as-is (%q) — impersonation open", got)
		}
		want := undashed(uuid.NewMD5(felisAuthNS, []byte("littleskin:"+notchMojangID)))
		if got != want {
			t.Fatalf("rewrite = %q, want deterministic UUIDv3 %q", got, want)
		}
	})

	// Mojang is priority-first: when both would validate the same name, Mojang wins.
	t.Run("mojang priority wins over thirdparty", func(t *testing.T) {
		mojang := fakeYgg(t, notchMojangID, "Notch")
		third := fakeYgg(t, "aaaaaaaaaaaa4aaaaaaaaaaaaaaaaaaa", "Notch")
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{
			{Tag: "mojang", URL: mojang.URL, Identity: true},
			{Tag: "littleskin", URL: third.URL, Identity: false},
		}
		w := getHasJoined(api.InternalHandler(), "Notch", "abc")
		if p := profileOf(t, w); p.ID != notchMojangID {
			t.Fatalf("id = %q, want mojang %q (priority)", p.ID, notchMojangID)
		}
	})

	// Mojang doesn't know the player (204) → fall through to the third-party source,
	// whose profile is returned rewritten.
	t.Run("fallthrough to thirdparty when mojang 204s", func(t *testing.T) {
		mojang := fakeYgg(t, "", "") // 204: not my player
		third := fakeYgg(t, notchMojangID, "Notch")
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{
			{Tag: "mojang", URL: mojang.URL, Identity: true},
			{Tag: "littleskin", URL: third.URL, Identity: false},
		}
		w := getHasJoined(api.InternalHandler(), "Notch", "abc")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", w.Code)
		}
		want := undashed(uuid.NewMD5(felisAuthNS, []byte("littleskin:"+notchMojangID)))
		if p := profileOf(t, w); p.ID != want {
			t.Fatalf("id = %q, want rewritten thirdparty %q", p.ID, want)
		}
	})

	// No source validates → 204 (authlib maps this to a verify failure).
	t.Run("no source validates -> 204", func(t *testing.T) {
		a := fakeYgg(t, "", "")
		b := fakeYgg(t, "", "")
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{
			{Tag: "mojang", URL: a.URL, Identity: true},
			{Tag: "littleskin", URL: b.URL, Identity: false},
		}
		if w := getHasJoined(api.InternalHandler(), "Ghost", "abc"); w.Code != http.StatusNoContent {
			t.Fatalf("code = %d, want 204", w.Code)
		}
	})

	// The reused bar gate: a barred CANONICAL UUID is rejected at the resolver, so a
	// reclaimed squatter stays out even on a consumer with no limbo plugin. Keyed on the
	// dashed canonical (post-rewrite), the same form Repo.ReclaimUsername stores.
	t.Run("barred canonical UUID -> 204", func(t *testing.T) {
		third := fakeYgg(t, notchMojangID, "Notch")
		repo := newFakeRepo()
		canonical := uuid.NewMD5(felisAuthNS, []byte("littleskin:"+notchMojangID))
		repo.blacklist[canonical.String()] = true // barred by a prior reclaim
		api := newTestAPI(repo, newFakeCluster())
		api.AuthSources = []AuthSource{{Tag: "littleskin", URL: third.URL, Identity: false}}

		if w := getHasJoined(api.InternalHandler(), "Notch", "abc"); w.Code != http.StatusNoContent {
			t.Fatalf("barred login: code = %d, want 204", w.Code)
		}
	})

	// Missing query fields → 204 without touching any source.
	t.Run("missing username -> 204", func(t *testing.T) {
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{{Tag: "mojang", URL: "http://127.0.0.1:0", Identity: true}}
		w := do(api.InternalHandler(), "GET", "/session/minecraft/hasJoined?serverId=abc", "", nil)
		if w.Code != http.StatusNoContent {
			t.Fatalf("code = %d, want 204", w.Code)
		}
	})
}
