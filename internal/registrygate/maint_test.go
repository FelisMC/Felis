package registrygate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newMaintGate(t *testing.T) (*Gate, *httptest.Server, *httptest.Server, *upstreamLog, *clock) {
	t.Helper()
	up := &upstreamLog{}
	upSrv := httptest.NewServer(up.handler())
	t.Cleanup(upSrv.Close)
	target, _ := url.Parse(upSrv.URL)
	g := New(target, map[string]string{
		PrincipalPlatform: "plat-secret", PrincipalBuild: "build-secret", PrincipalPrune: "prune-secret",
	}, nil)
	clk := &clock{t: time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)}
	g.maint.now = clk.now
	gs := httptest.NewServer(g)
	t.Cleanup(gs.Close)
	ms := httptest.NewServer(g.MaintHandler())
	t.Cleanup(ms.Close)
	return g, gs, ms, up, clk
}

func post(t *testing.T, srv *httptest.Server, path string) int {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestPrunePrincipalMayOnlyDeleteManifestsByDigest(t *testing.T) {
	up2 := &upstreamLog{}
	upSrv := httptest.NewServer(up2.handler())
	t.Cleanup(upSrv.Close)
	target, _ := url.Parse(upSrv.URL)
	g := New(target, map[string]string{PrincipalPrune: "prune-secret"}, nil)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)

	for _, p := range []string{
		"/v2/user-uploads/s1/manifests/" + testDigest,
		"/v2/felis/felis/manifests/" + testDigest,
	} {
		if resp := do(t, srv, http.MethodDelete, p, PrincipalPrune, "prune-secret"); resp.StatusCode != http.StatusOK {
			t.Errorf("prune DELETE %s = %d, want it proxied", p, resp.StatusCode)
		}
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodDelete, "/v2/user-uploads/s1/manifests/latest"},
		{http.MethodDelete, "/v2/user-uploads/s1/blobs/" + testDigest},
		{http.MethodDelete, "/v2/user-uploads/s1/manifests/sha256:short"},
		{http.MethodPut, "/v2/user-uploads/s1/manifests/" + testDigest},
		{http.MethodPost, "/v2/user-uploads/s1/blobs/uploads/"},
		{http.MethodPatch, "/v2/user-uploads/s1/blobs/uploads/abc"},
	} {
		if resp := do(t, srv, c.method, c.path, PrincipalPrune, "prune-secret"); resp.StatusCode != http.StatusForbidden {
			t.Errorf("prune %s %s = %d, want 403", c.method, c.path, resp.StatusCode)
		}
	}
	if n := up2.count(); n != 2 {
		t.Fatalf("upstream saw %d requests, want the 2 allowed deletes: %v", n, up2.seen)
	}
}

func TestReadOnlyWindowWaitsForQuietThenRefusesWrites(t *testing.T) {
	_, gs, ms, up, clk := newMaintGate(t)
	put := "/v2/user-uploads/s1/manifests/latest"

	if resp := do(t, gs, http.MethodPut, put, PrincipalBuild, "build-secret"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("write before any window = %d, want 201", resp.StatusCode)
	}
	// A write just finished: a push may be between two of its requests.
	if code := post(t, ms, "/readonly?lease=600"); code != http.StatusConflict {
		t.Fatalf("readonly right after a write = %d, want 409", code)
	}
	clk.advance(DefaultQuiet)
	if code := post(t, ms, "/readonly?lease=600"); code != http.StatusOK {
		t.Fatalf("readonly after %s of quiet = %d, want 200", DefaultQuiet, code)
	}

	before := up.count()
	resp := do(t, gs, http.MethodPut, put, PrincipalPlatform, "plat-secret")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("write during the window = %d Retry-After=%q, want 503 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if resp := do(t, gs, http.MethodGet, "/v2/felis/felis/manifests/b1", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("read during the window = %d, want 200", resp.StatusCode)
	}
	if up.count() != before+1 {
		t.Fatalf("the refused write reached the registry: %v", up.seen)
	}
	resp, err := http.Get(ms.URL + "/readonly")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readonly during the window = %d, want 200", resp.StatusCode)
	}

	if code := post(t, ms, "/readwrite"); code != http.StatusOK {
		t.Fatalf("readwrite = %d", code)
	}
	if resp := do(t, gs, http.MethodPut, put, PrincipalBuild, "build-secret"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("write after the window = %d, want 201", resp.StatusCode)
	}
}

func TestReadOnlyWindowIsALease(t *testing.T) {
	_, gs, ms, _, clk := newMaintGate(t)
	if code := post(t, ms, "/readonly?lease=60"); code != http.StatusOK {
		t.Fatalf("readonly on an idle gate = %d, want 200", code)
	}
	put := "/v2/user-uploads/s1/manifests/latest"
	if resp := do(t, gs, http.MethodPut, put, PrincipalBuild, "build-secret"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("write inside the lease = %d, want 503", resp.StatusCode)
	}
	// A GC sidecar that died mid-sweep never releases; the lease does.
	clk.advance(61 * time.Second)
	if resp := do(t, gs, http.MethodPut, put, PrincipalBuild, "build-secret"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("write after the lease expired = %d, want 201", resp.StatusCode)
	}
	for _, bad := range []string{"0", "-5", "soon"} {
		if code := post(t, ms, "/readonly?lease="+bad); code != http.StatusBadRequest {
			t.Errorf("lease=%s = %d, want 400", bad, code)
		}
	}
}

func TestReadOnlyWindowSurvivesAGateRestart(t *testing.T) {
	dir := t.TempDir()
	state := MaintStatePath(dir)
	g, _, ms, _, clk := newMaintGate(t)
	if err := g.SetMaintenanceState(state); err != nil {
		t.Fatal(err)
	}
	if code := post(t, ms, "/readonly?lease=600"); code != http.StatusOK {
		t.Fatalf("readonly = %d", code)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("window not persisted: %v", err)
	}

	// The restarted gate reads the window back and keeps refusing writes.
	g2, gs2, _, _, _ := newMaintGate(t)
	g2.maint.now = clk.now
	if err := g2.SetMaintenanceState(state); err != nil {
		t.Fatal(err)
	}
	if resp := do(t, gs2, http.MethodPut, "/v2/user-uploads/s1/manifests/latest", PrincipalBuild, "build-secret"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("write on the restarted gate = %d, want 503", resp.StatusCode)
	}

	if code := post(t, ms, "/readwrite"); code != http.StatusOK {
		t.Fatalf("readwrite = %d", code)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("state file left after release: %v", err)
	}

	// An expired window in the file is ignored.
	if err := os.WriteFile(state, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	g3, gs3, _, _, _ := newMaintGate(t)
	if err := g3.SetMaintenanceState(state); err != nil {
		t.Fatal(err)
	}
	if resp := do(t, gs3, http.MethodPut, "/v2/user-uploads/s1/manifests/latest", PrincipalBuild, "build-secret"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("write with an expired window on file = %d, want 201", resp.StatusCode)
	}
	if err := os.WriteFile(state, []byte("not-a-number"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g3.SetMaintenanceState(state); err == nil || !strings.Contains(err.Error(), "maintenance state") {
		t.Fatalf("a corrupt state file = %v, want an error naming it", err)
	}
}

func TestManifestIndexListsUntaggedRevisions(t *testing.T) {
	root := t.TempDir()
	base := root + "/docker/registry/v2/repositories/felis/paper/_manifests"
	hexA := strings.Repeat("a", 64)
	hexB := strings.Repeat("b", 64)
	hexGone := strings.Repeat("c", 64)
	for _, h := range []string{hexA, hexB} {
		if err := os.MkdirAll(base+"/revisions/sha256/"+h, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(base+"/revisions/sha256/"+h+"/link", []byte("sha256:"+h), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A deleted manifest leaves its directory without a link.
	if err := os.MkdirAll(base+"/revisions/sha256/"+hexGone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base+"/tags/demo/current", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+"/tags/demo/current/link", []byte("sha256:"+hexB), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(base+"/revisions/sha256/"+hexA+"/link", old, old); err != nil {
		t.Fatal(err)
	}

	up := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(up.Close)
	target, _ := url.Parse(up.URL)
	g := New(target, nil, nil)
	g.DataDir = root
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + IndexPathPrefix + "felis/paper")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var idx Index
	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Revisions) != 2 || idx.Revisions[0].Digest != "sha256:"+hexA || !idx.Revisions[0].Pushed.Equal(old) {
		t.Fatalf("revisions = %+v, want a (pushed %s) and b, without the deleted c", idx.Revisions, old)
	}
	if idx.Tags["demo"] != "sha256:"+hexB || len(idx.Tags) != 1 {
		t.Fatalf("tags = %v, want demo -> b", idx.Tags)
	}

	// A repository that does not exist is empty; a traversal is refused.
	if idx, err := ReadIndex(root, "user-uploads/none"); err != nil || len(idx.Revisions) != 0 || len(idx.Tags) != 0 {
		t.Fatalf("missing repo = %+v, %v, want empty", idx, err)
	}
	for _, bad := range []string{"felis/../felis/paper", "felis/./paper", "Felis/paper", "felis//paper", "felis/paper/"} {
		resp, err := http.Get(srv.URL + IndexPathPrefix + bad)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("index of %q = %d, want 400", bad, resp.StatusCode)
		}
	}
}
