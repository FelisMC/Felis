package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/fileedit"
)

// The passkey relying party follows the panel host the SPA is served on: an install
// that names only its root domain still gets passkeys, on console.<root>, with the
// operator host as the second origin; only an install with no panel host goes without.
func TestPasskeyRelyingParty(t *testing.T) {
	t.Setenv("FELIS_PANEL_NODEPORT", "")
	for _, tc := range []struct {
		name               string
		root, panel, admin string
		wantRP             string
		wantOrigins        []string
	}{
		{"root domain only", "example.net", "", "", "console.example.net",
			[]string{"https://console.example.net", "https://op.console.example.net"}},
		{"configured hosts", "example.net", " play.example.net ", "ops.example.net", "play.example.net",
			[]string{"https://play.example.net", "https://ops.example.net"}},
		{"operator host equal to the panel host", "example.net", "console.example.net", "console.example.net",
			"console.example.net", []string{"https://console.example.net"}},
		{"panel host without a root domain", "", "console.example.org", "", "console.example.org",
			[]string{"https://console.example.org"}},
		{"no host at all", "", "", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Server.RootDomain, cfg.Auth.PanelHostname, cfg.Auth.AdminHostname = tc.root, tc.panel, tc.admin
			var wantOrigins []string
			for _, origin := range tc.wantOrigins {
				wantOrigins = append(wantOrigins, origin, fmt.Sprintf("%s:%d", origin, defaultPanelNodePort))
			}
			rp, origins := passkeyRelyingParty(cfg)
			if rp != tc.wantRP || !slices.Equal(origins, wantOrigins) {
				t.Fatalf("relying party = %q %q, want %q %q", rp, origins, tc.wantRP, wantOrigins)
			}
		})
	}
}

func TestPasskeyRelyingPartyIncludesConfiguredNodePort(t *testing.T) {
	t.Setenv("FELIS_PANEL_NODEPORT", "30445")
	cfg := &config.Config{}
	cfg.Server.RootDomain = "example.com"
	_, origins := passkeyRelyingParty(cfg)
	if !slices.Contains(origins, "https://op.console.example.com:30445") {
		t.Fatalf("configured NodePort origin missing: %v", origins)
	}
}

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
			{Tag: "guild", Prefix: "GD", URL: "https://guild.example/hasJoined", APIURL: "https://guild.example/api"},
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
				if s.Tag != c.Tag || s.Prefix != c.Prefix || s.URL != c.URL || s.APIURL != c.APIURL {
					t.Errorf("source %d = %+v, want %+v in config order", i+1, s, c)
				}
			}
		})
	}
}

// TestBuildConfig_ProjectsOverrides pins the [registry] overrides reaching the
// build subsystem: unset fields must stay EMPTY (the build package's compiled-in
// defaults apply there, not here), and set fields must pass through verbatim —
// an air-gapped install points these at its imported mirrors.
func TestBuildConfig_ProjectsOverrides(t *testing.T) {
	empty := buildConfig(&config.Config{})
	if empty.KanikoImage != "" || empty.TrivyImage != "" || empty.CPULimit != "" || empty.MemLimit != "" {
		t.Errorf("empty registry config must project empty overrides (defaults live in internal/build), got %+v", empty)
	}
	full := buildConfig(&config.Config{Registry: config.RegistryConfig{
		URL:            "registry.felis.svc:5000",
		BuildNamespace: "felis-build",
		KanikoImage:    "reg/kaniko:v1",
		TrivyImage:     "reg/trivy:v1",
		BuildCPULimit:  "1",
		BuildMemLimit:  "2Gi",
	}})
	if full.KanikoImage != "reg/kaniko:v1" || full.TrivyImage != "reg/trivy:v1" ||
		full.CPULimit != "1" || full.MemLimit != "2Gi" {
		t.Errorf("registry overrides did not reach build.Config: %+v", full)
	}
	if full.Namespace != "felis-build" || full.RegistryURL != "registry.felis.svc:5000" {
		t.Errorf("namespace/registry url must keep projecting: %+v", full)
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

// Every listener drains at once, and the shutdown hook (API.CloseStreams in
// cmdAPI) runs on each: two listeners each holding a request that ends when
// the hook fires take one hook's worth of time, well inside the deadline.
func TestShutdownServersDrainsListenersTogether(t *testing.T) {
	release := make(chan struct{})
	var hooks int32
	started := make(chan struct{}, 2)
	var servers []*http.Server
	for range 2 {
		srv := newAPIServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started <- struct{}{}
			<-release
		}))
		srv.RegisterOnShutdown(func() {
			if atomic.AddInt32(&hooks, 1) == 1 {
				time.AfterFunc(100*time.Millisecond, func() { close(release) })
			}
		})
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv.Addr = l.Addr().String()
		go func() { _ = srv.Serve(l) }()
		go func() {
			if resp, err := http.Get("http://" + srv.Addr); err == nil {
				resp.Body.Close()
			}
		}()
		servers = append(servers, srv)
	}
	<-started
	<-started

	begun := time.Now()
	shutdownServers(servers, 5*time.Second, io.Discard)
	if took := time.Since(begun); took > 2*time.Second {
		t.Fatalf("shutdown took %v", took)
	}
	if got := atomic.LoadInt32(&hooks); got != 2 {
		t.Fatalf("shutdown hook ran %d times, want once per listener", got)
	}
}

type fakeRefStore struct {
	images []build.Image
	builds []build.Build
	err    error
}

func (f fakeRefStore) ListImages(context.Context) ([]build.Image, error) { return f.images, f.err }
func (f fakeRefStore) ListUnfinishedBuilds(context.Context) ([]build.Build, error) {
	return f.builds, nil
}

type fakeServers struct {
	list    []api.ServerInfo
	pods    []string
	podsErr error
}

func (f fakeServers) ListServers(context.Context) ([]api.ServerInfo, error) { return f.list, nil }
func (f fakeServers) PodImages(context.Context) ([]string, error)           { return f.pods, f.podsErr }

// The registry pruner deletes whatever this list does not name, so every source of
// a reference has to be in it, and a failing source must fail the list.
func TestInUseImageRefsCoversEverySource(t *testing.T) {
	const reg = "registry.felis.svc:5000/"
	store := fakeRefStore{
		images: []build.Image{{ImageRef: reg + "modpacks/pack:*"}, {ImageRef: reg + "felis/paper:demo"}},
		builds: []build.Build{{ImageRef: reg + "user-uploads/sub-9:latest"}},
	}
	servers := fakeServers{
		list: []api.ServerInfo{{Name: "s1", Image: reg + "felis/paper:demo@sha256:" + fmt.Sprintf("%064d", 1)}},
		pods: []string{reg + "felis/felis:v1.0.0"},
	}
	got, err := inUseImageRefs(context.Background(), store, servers, []string{reg + "felis/felis:b60"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		reg + "felis/felis:b60",
		reg + "modpacks/pack:*", reg + "felis/paper:demo",
		reg + "felis/paper:demo@sha256:" + fmt.Sprintf("%064d", 1),
		reg + "felis/felis:v1.0.0",
		reg + "user-uploads/sub-9:latest",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("refs = %v\nwant %v", got, want)
	}

	servers.podsErr = errors.New("apiserver down")
	if _, err := inUseImageRefs(context.Background(), store, servers, nil); err == nil {
		t.Fatal("a failing pod list produced a reference list")
	}
	servers.podsErr = nil

	store.err = errors.New("db down")
	if _, err := inUseImageRefs(context.Background(), store, servers, nil); err == nil {
		t.Fatal("a failing whitelist read produced a reference list")
	}
}

// TestExpireFileSessions runs the loop against a stage whose clock the test
// holds: the session idle past fileedit.SessionIdle goes, the one touched since
// stays, and the drop is said once.
func TestExpireFileSessions(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	st := &fileedit.Stage{Dir: t.TempDir(), MinFree: 1e-9, Now: func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}}
	idle, err := st.Begin("u1", "survival", "a.jar", 3)
	if err != nil {
		t.Fatal(err)
	}
	advance(fileedit.SessionIdle - time.Minute)
	fresh, err := st.Begin("u1", "survival", "b.jar", 3)
	if err != nil {
		t.Fatal(err)
	}
	advance(2 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan struct{})
	go func() { expireFileSessions(ctx, st, time.Millisecond, &out); close(done) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := st.Status("u1", "survival", idle.ID); errors.Is(err, fileedit.ErrNotStaged) {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the idle session was never dropped")
		}
	}
	// A few more ticks with nothing idle, which must stay quiet.
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if _, err := st.Status("u1", "survival", fresh.ID); err != nil {
		t.Fatalf("the session touched since went too: %v", err)
	}
	if got := out.String(); got != "felis api: dropped 1 upload session(s) left idle for 6h0m0s\n" {
		t.Fatalf("said %q", got)
	}
}

type sweepCount struct{ n atomic.Int32 }

func (s *sweepCount) ExpireExports() { s.n.Add(1) }

// TestExpireExports: the loop sweeps on each tick, and returns once felis-api
// shuts down.
func TestExpireExports(t *testing.T) {
	var s sweepCount
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { expireExports(ctx, &s, time.Millisecond); close(done) }()
	for deadline := time.Now().Add(5 * time.Second); s.n.Load() < 3; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("swept %d times in 5s at a 1ms tick", s.n.Load())
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop outlived its context")
	}
}
