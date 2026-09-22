package api

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// stubMojangNames points the premium-name lookup at a fake api.mojang.com that reports the
// given names as registered and every other name as free, and clears the process-wide cache
// so no subtest can see another's answers. Without it these tests would query the real
// Mojang over the network — and get a different answer on the day someone buys the name.
func stubMojangNames(t *testing.T, taken ...string) {
	t.Helper()
	registered := make(map[string]bool, len(taken))
	for _, n := range taken {
		registered[strings.ToLower(n)] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.ToLower(path.Base(r.URL.Path))
		if !registered[name] {
			w.WriteHeader(http.StatusNotFound) // Mojang's "nobody owns this name"
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": notchMojangID, "name": name})
	}))
	t.Cleanup(srv.Close)
	setProfileAPI(t, srv.URL+"/")
}

func setProfileAPI(t *testing.T, base string) {
	t.Helper()
	prev := mojangProfileAPI
	mojangProfileAPI = base
	resetPremiumCache()
	t.Cleanup(func() { mojangProfileAPI = prev; resetPremiumCache() })
}

func resetPremiumCache() {
	premiumNames.Lock()
	clear(premiumNames.m)
	premiumNames.Unlock()
}

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
		stubMojangNames(t)
		evil := fakeYgg(t, notchMojangID, "Notch") // lies: returns real Notch's Mojang UUID
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{{Tag: "littleskin", Prefix: "LS", URL: evil.URL, Identity: false}}

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
		stubMojangNames(t, "Notch")
		mojang := fakeYgg(t, notchMojangID, "Notch")
		third := fakeYgg(t, "aaaaaaaaaaaa4aaaaaaaaaaaaaaaaaaa", "Notch")
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{
			{Tag: "mojang", URL: mojang.URL, Identity: true},
			{Tag: "littleskin", Prefix: "LS", URL: third.URL, Identity: false},
		}
		w := getHasJoined(api.InternalHandler(), "Notch", "abc")
		p := profileOf(t, w)
		if p.ID != notchMojangID {
			t.Fatalf("id = %q, want mojang %q (priority)", p.ID, notchMojangID)
		}
		// The player who OWNS the name is never the one who gets renamed, even though the
		// name is (of course) a registered Mojang name.
		if p.Name != "Notch" {
			t.Fatalf("mojang name = %q, want unchanged %q", p.Name, "Notch")
		}
	})

	// Mojang doesn't know the player (204) → fall through to the third-party source,
	// whose profile is returned rewritten.
	t.Run("fallthrough to thirdparty when mojang 204s", func(t *testing.T) {
		stubMojangNames(t)
		mojang := fakeYgg(t, "", "") // 204: not my player
		third := fakeYgg(t, notchMojangID, "Notch")
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{
			{Tag: "mojang", URL: mojang.URL, Identity: true},
			{Tag: "littleskin", Prefix: "LS", URL: third.URL, Identity: false},
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

	// A root that answers with a redirect is skipped, not followed: following it lets that
	// root aim this host at arbitrary URLs, including its own hasJoined route, which re-enters
	// the scan and multiplies the upstream traffic one login causes.
	t.Run("redirecting source is skipped, not followed", func(t *testing.T) {
		stubMojangNames(t)
		var followed atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			followed.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": notchMojangID, "name": "Notch"})
		}))
		t.Cleanup(target.Close)
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/hasJoined?"+r.URL.RawQuery, http.StatusFound)
		}))
		t.Cleanup(redirector.Close)
		honest := fakeYgg(t, "0123456789abcdef0123456789abcdef", "Steve0")
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{
			{Tag: "evil", Prefix: "EV", URL: redirector.URL},
			{Tag: "littleskin", Prefix: "LS", URL: honest.URL},
		}

		w := getHasJoined(api.InternalHandler(), "Steve0", "abc")
		if followed.Load() {
			t.Fatal("the redirect was followed")
		}
		if w.Code != http.StatusOK || profileOf(t, w).Name != "Steve0" {
			t.Fatalf("code = %d body = %q, want the next source's player", w.Code, w.Body.String())
		}
	})

	// The reused bar gate: a barred CANONICAL UUID is rejected at the resolver, so a
	// reclaimed squatter stays out even on a consumer with no limbo plugin. Keyed on the
	// dashed canonical (post-rewrite), the same form Repo.ReclaimUsername stores.
	t.Run("barred canonical UUID -> 204", func(t *testing.T) {
		stubMojangNames(t)
		third := fakeYgg(t, notchMojangID, "Notch")
		repo := newFakeRepo()
		canonical := uuid.NewMD5(felisAuthNS, []byte("littleskin:"+notchMojangID))
		repo.blacklist[canonical.String()] = true // barred by a prior reclaim
		api := newTestAPI(repo, newFakeCluster())
		api.AuthSources = []AuthSource{{Tag: "littleskin", Prefix: "LS", URL: third.URL, Identity: false}}

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

// The username namespace is the proxy's, not ours: Velocity keys its player registry on the
// NAME, so a third-party player holding a Mojang player's name locks the owner out of their
// own proxy ("You are already connected to this proxy!") even though the UUID rewrite makes
// them different players. These cover both branches of the rename — the rename itself, and
// the far more common case of leaving an ordinary player's name alone.
func TestHasJoinedPremiumNameRename(t *testing.T) {
	thirdPartyLogin := func(t *testing.T, name string) sessionProfile {
		t.Helper()
		third := fakeYgg(t, notchMojangID, name)
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{{Tag: "littleskin", Prefix: "LS", URL: third.URL, Identity: false}}
		w := getHasJoined(api.InternalHandler(), name, "abc")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%q)", w.Code, w.Body.String())
		}
		return profileOf(t, w)
	}

	// The branch the whole "only on collision" policy exists for: nobody at Mojang owns
	// this name, so the third-party player keeps it.
	t.Run("free name is left alone", func(t *testing.T) {
		stubMojangNames(t, "Notch") // Notch is taken; Steve0 is not
		if got := thirdPartyLogin(t, "Steve0").Name; got != "Steve0" {
			t.Fatalf("name = %q, want unchanged %q — a player nobody collides with must keep their name", got, "Steve0")
		}
	})

	t.Run("premium name is prefixed", func(t *testing.T) {
		stubMojangNames(t, "Notch")
		if got := thirdPartyLogin(t, "Notch").Name; got != "LS_Notch" {
			t.Fatalf("name = %q, want %q", got, "LS_Notch")
		}
	})

	// A name too long to prefix must be TRUNCATED, not left alone — skipping the rename
	// here is what would silently hand a long premium name back to the squatter.
	t.Run("long premium name is truncated to fit", func(t *testing.T) {
		stubMojangNames(t, "Antidisestablish") // 16 chars: the protocol maximum
		got := thirdPartyLogin(t, "Antidisestablish").Name
		if got != "LS_Antidisestabl" {
			t.Fatalf("name = %q, want %q", got, "LS_Antidisestabl")
		}
		if len(got) > mcUsernameMax {
			t.Fatalf("name %q is %d chars, over the %d-char protocol limit", got, len(got), mcUsernameMax)
		}
	})

	// Mojang unreachable, nothing cached → assume the name is premium and rename. A Mojang
	// outage must not become an opportunity to hold someone else's name.
	t.Run("mojang unreachable -> fails closed and renames", func(t *testing.T) {
		setProfileAPI(t, "http://127.0.0.1:1/") // nothing listening: instant connection refused
		if got := thirdPartyLogin(t, "Notch").Name; got != "LS_Notch" {
			t.Fatalf("name = %q, want %q — a failed lookup must not leave a squatter holding the name", got, "LS_Notch")
		}
	})

	// A third-party root is untrusted input, its name field included.
	t.Run("hostile upstream name -> 204", func(t *testing.T) {
		stubMojangNames(t)
		for _, bad := range []string{"§4admin", "not a name", "ab", strings.Repeat("x", 17), ""} {
			third := fakeYgg(t, notchMojangID, bad)
			api := newTestAPI(newFakeRepo(), newFakeCluster())
			api.AuthSources = []AuthSource{{Tag: "littleskin", Prefix: "LS", URL: third.URL, Identity: false}}
			if w := getHasJoined(api.InternalHandler(), "Notch", "abc"); w.Code != http.StatusNoContent {
				t.Fatalf("upstream name %q: code = %d, want 204 (%q)", bad, w.Code, w.Body.String())
			}
		}
	})
}
