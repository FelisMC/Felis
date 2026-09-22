package api

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// blacklistDownRepo is a store whose bar list cannot be read.
type blacklistDownRepo struct{ *fakeRepo }

func (blacklistDownRepo) IsUsernameBlacklisted(context.Context, string) (bool, error) {
	return false, errors.New("bar list unreachable")
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

// undashed is the 32-hex form the resolver must emit (Velocity's GameProfile format).
func undashed(u uuid.UUID) string { return hex.EncodeToString(u[:]) }

const notchMojangID = "069a79f444e94726a5befca90e38aaf5" // a real Mojang-space UUID, undashed

func TestHasJoined(t *testing.T) {
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
		// Also pinned as literals: every third-party player's UUID, and every ban and account
		// link keyed on one, depends on these exact bytes, and a change to the namespace seed
		// moves both sides of the derived comparison above at once.
		if ns := felisAuthNS.String(); ns != "07228eae-77f6-500e-9dc0-436afbc87c27" || got != "b63bcc1c611432eeb7b3af3a15012e48" {
			t.Fatalf("namespace %s / rewrite %s changed; every existing third-party player would get a new UUID", ns, got)
		}
	})

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

	// A source that could not answer has not said no. With nobody validating, the login is
	// an outage whether that source errored, redirected or was unreachable.
	t.Run("failing source and no validator -> 503", func(t *testing.T) {
		down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(down.Close)
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.example/hasJoined", http.StatusMovedPermanently)
		}))
		t.Cleanup(redirector.Close)
		nobody := fakeYgg(t, "", "")
		for _, failing := range []string{down.URL, redirector.URL, "http://127.0.0.1:1"} {
			api := newTestAPI(newFakeRepo(), newFakeCluster())
			api.AuthSources = []AuthSource{
				{Tag: "mojang", URL: nobody.URL, Identity: true},
				{Tag: "littleskin", Prefix: "LS", URL: failing},
			}
			if w := getHasJoined(api.InternalHandler(), "Ghost", "abc"); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("source %s: code = %d, want 503", failing, w.Code)
			}
		}
	})

	// A skinless player's profile may come back with properties [], null or absent. The
	// relay must still send an array: Velocity's GameProfile parser throws on a missing or
	// null key and the login hangs, where the same answer sent straight to Velocity works.
	t.Run("properties always emitted as an array", func(t *testing.T) {
		for _, upstream := range []string{
			`{"id":"` + notchMojangID + `","name":"Notch","properties":[]}`,
			`{"id":"` + notchMojangID + `","name":"Notch","properties":null}`,
			`{"id":"` + notchMojangID + `","name":"Notch"}`,
		} {
			src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(upstream))
			}))
			api := newTestAPI(newFakeRepo(), newFakeCluster())
			api.AuthSources = []AuthSource{{Tag: "mojang", URL: src.URL, Identity: true}}

			w := getHasJoined(api.InternalHandler(), "Notch", "abc")
			src.Close()
			var body map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
				t.Fatalf("upstream %s: code = %d body = %q", upstream, w.Code, w.Body.String())
			}
			if got := string(body["properties"]); got != "[]" {
				t.Errorf("upstream %s: properties = %q, want []", upstream, got)
			}
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

	// A root whose headers blow past the cap is dropped like any failed source, even when the
	// body behind them is a well-formed profile.
	t.Run("oversized response headers skip the source", func(t *testing.T) {
		stubMojangNames(t)
		bloated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Padding", strings.Repeat("a", 64<<10))
			_ = json.NewEncoder(w).Encode(map[string]any{"id": notchMojangID, "name": "Steve0"})
		}))
		t.Cleanup(bloated.Close)
		honest := fakeYgg(t, "0123456789abcdef0123456789abcdef", "Steve0")
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{
			{Tag: "evil", Prefix: "EV", URL: bloated.URL},
			{Tag: "littleskin", Prefix: "LS", URL: honest.URL},
		}

		w := getHasJoined(api.InternalHandler(), "Steve0", "abc")
		want := undashed(uuid.NewMD5(felisAuthNS, []byte("littleskin:0123456789abcdef0123456789abcdef")))
		if w.Code != http.StatusOK || profileOf(t, w).ID != want {
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

	// With the bar list unreadable, nobody can say the player is not barred; the login must
	// not go through. (Velocity reports the 500 as the auth servers being down.)
	t.Run("bar list lookup error -> not admitted", func(t *testing.T) {
		mojang := fakeYgg(t, notchMojangID, "Notch")
		api := newTestAPI(blacklistDownRepo{newFakeRepo()}, newFakeCluster())
		api.AuthSources = []AuthSource{{Tag: "mojang", URL: mojang.URL, Identity: true}}
		if w := getHasJoined(api.InternalHandler(), "Notch", "abc"); w.Code == http.StatusOK {
			t.Fatalf("admitted with the bar list unreadable (%q)", w.Body.String())
		}
	})

	// Mojang is trusted for its UUIDs, which is exactly why one that does not parse must not
	// be emitted as some default: every such login would share the nil UUID.
	t.Run("identity source with an unparseable id -> 204", func(t *testing.T) {
		mojang := fakeYgg(t, "not-a-uuid", "Notch")
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{{Tag: "mojang", URL: mojang.URL, Identity: true}}
		if w := getHasJoined(api.InternalHandler(), "Notch", "abc"); w.Code != http.StatusNoContent {
			t.Fatalf("code = %d, want 204 (%q)", w.Code, w.Body.String())
		}
	})

	// The player's address is what lets a source refuse a session relayed from another IP
	// (prevent-proxy-connections); it has to reach the source unchanged.
	t.Run("ip is forwarded to the source", func(t *testing.T) {
		got := make(chan string, 1)
		src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got <- r.URL.Query().Get("ip")
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(src.Close)
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{{Tag: "mojang", URL: src.URL, Identity: true}}
		do(api.InternalHandler(), "GET", "/session/minecraft/hasJoined?username=Notch&serverId=abc&ip=203.0.113.9", "", nil)
		if ip := <-got; ip != "203.0.113.9" {
			t.Fatalf("source saw ip %q, want 203.0.113.9", ip)
		}
	})

	// A GET that declares a body it never sends must still be answered and lose its
	// connection; otherwise each such socket stays open for as long as the client likes.
	t.Run("request declaring a body is refused and closed", func(t *testing.T) {
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		srv := httptest.NewServer(api.InternalHandler())
		t.Cleanup(srv.Close)
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "GET /session/minecraft/hasJoined?username=a&serverId=b HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n")
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("no answer while the declared body never arrives: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !resp.Close {
			t.Fatalf("code = %d close = %v, want 400 with Connection: close", resp.StatusCode, resp.Close)
		}
	})

	// A missing or oversized field is answered 204 before any source sees it. The source
	// here validates anything it is asked, so only a request that never reaches it is a 204.
	t.Run("missing or oversized query field -> 204 without asking a source", func(t *testing.T) {
		var hits atomic.Int32
		src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": notchMojangID, "name": "Notch"})
		}))
		t.Cleanup(src.Close)
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.AuthSources = []AuthSource{{Tag: "mojang", URL: src.URL, Identity: true}}
		long := strings.Repeat("a", maxHasJoinedParam+1)
		for _, query := range []string{
			"serverId=abc",
			"username=Notch",
			"username=" + long + "&serverId=abc",
			"username=Notch&serverId=" + long,
			"username=Notch&serverId=abc&ip=" + long,
		} {
			w := do(api.InternalHandler(), "GET", "/session/minecraft/hasJoined?"+query, "", nil)
			if w.Code != http.StatusNoContent || hits.Load() != 0 {
				t.Fatalf("%.40s: code = %d, source asked %d times; want 204 and 0", query, w.Code, hits.Load())
			}
		}
		if w := getHasJoined(api.InternalHandler(), "Notch", "abc"); w.Code != http.StatusOK {
			t.Fatalf("a well-formed login: code = %d, want 200 (the source is live)", w.Code)
		}
	})
}

// profileAPIAnswering stands in for api.mojang.com answering every lookup with status and
// counts how often it is asked. 200 means taken, 404 free, anything else is an outage.
func profileAPIAnswering(t *testing.T, status int) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	setProfileAPI(t, srv.URL+"/")
	return &n
}

func seedPremium(name string, taken bool, age time.Duration) {
	premiumNames.Lock()
	premiumNames.m[strings.ToLower(name)] = premiumEntry{taken: taken, at: time.Now().Add(-age)}
	premiumNames.Unlock()
}

// The premium-name cache decides on each third-party login whether the player keeps their
// name. Each case pins one rule that an innocent-looking edit would break unnoticed.
func TestPremiumNameCache(t *testing.T) {
	ctx := context.Background()

	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("mojang %d is an outage, not a free name", status), func(t *testing.T) {
			profileAPIAnswering(t, status)
			if !isPremiumName(ctx, "Notch") {
				t.Fatal("name treated as free; a squatter would keep it through the outage")
			}
		})
	}

	t.Run("a free answer is asked again once it expires", func(t *testing.T) {
		n := profileAPIAnswering(t, http.StatusNotFound)
		seedPremium("Steve0", false, premiumFreeTTL+time.Minute)
		isPremiumName(ctx, "Steve0")
		if n.Load() != 1 {
			t.Fatalf("expired free answer: %d lookups, want 1", n.Load())
		}
	})

	t.Run("answers within their TTL come from the cache", func(t *testing.T) {
		n := profileAPIAnswering(t, http.StatusNotFound)
		seedPremium("Steve0", false, premiumFreeTTL-time.Minute)
		seedPremium("Notch", true, premiumTakenTTL-time.Hour)
		if isPremiumName(ctx, "Steve0") || !isPremiumName(ctx, "Notch") || n.Load() != 0 {
			t.Fatalf("cached answers not served as cached (%d lookups)", n.Load())
		}
	})

	t.Run("an expired taken answer outlives an outage", func(t *testing.T) {
		profileAPIAnswering(t, http.StatusServiceUnavailable)
		seedPremium("Notch", true, 2*premiumTakenTTL)
		if !isPremiumName(ctx, "Notch") {
			t.Fatal("known premium name treated as free during an outage")
		}
	})

	t.Run("the cache is cleared rather than grown past its bound", func(t *testing.T) {
		profileAPIAnswering(t, http.StatusNotFound)
		for i := range premiumCacheMax {
			seedPremium(fmt.Sprintf("n%d", i), false, 0)
		}
		isPremiumName(ctx, "Steve0")
		premiumNames.Lock()
		size := len(premiumNames.m)
		premiumNames.Unlock()
		if size != 1 {
			t.Fatalf("cache holds %d entries after passing its bound, want 1", size)
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
