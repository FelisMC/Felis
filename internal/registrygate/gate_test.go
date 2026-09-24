package registrygate

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type upstreamLog struct {
	mu   sync.Mutex
	seen []string
	auth []string
}

func (u *upstreamLog) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.seen = append(u.seen, r.Method+" "+r.URL.RequestURI())
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Location", "/v2/x/blobs/uploads/abc")
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
}

func (u *upstreamLog) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.seen)
}

func newGate(t *testing.T) (*httptest.Server, *upstreamLog) {
	t.Helper()
	up := &upstreamLog{}
	upSrv := httptest.NewServer(up.handler())
	t.Cleanup(upSrv.Close)
	target, _ := url.Parse(upSrv.URL)
	g := New(target, map[string]string{PrincipalPlatform: "plat-secret", PrincipalBuild: "build-secret"}, nil)
	gs := httptest.NewServer(g)
	t.Cleanup(gs.Close)
	return gs, up
}

func do(t *testing.T, srv *httptest.Server, method, path, user, pass string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestAnonymousReadsPassButTheAPIRootChallenges(t *testing.T) {
	gs, up := newGate(t)
	for _, p := range []string{
		"/v2/felis/felis/manifests/v0.1.0",
		"/v2/felis/felis/blobs/sha256:abc",
		"/v2/mirror/trivy-db/manifests/2",
		"/v2/_catalog",
		"/v2/user-uploads/s1/tags/list",
	} {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			if resp := do(t, gs, m, p, "", ""); resp.StatusCode != http.StatusOK {
				t.Errorf("%s %s anonymous = %d, want 200", m, p, resp.StatusCode)
			}
		}
	}
	// Docker's daemon only sends credentials on a push if the API root challenged
	// it, so the root must say 401 + Basic to anonymous callers.
	resp := do(t, gs, http.MethodGet, "/v2/", "", "")
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
		t.Fatalf("anonymous GET /v2/ = %d %q, want 401 Basic challenge", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if resp := do(t, gs, http.MethodGet, "/v2/", PrincipalBuild, "build-secret"); resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET /v2/ = %d, want 200", resp.StatusCode)
	}
	if resp := do(t, gs, http.MethodGet, "/v2/", PrincipalBuild, "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /v2/ with a bad secret = %d, want 401", resp.StatusCode)
	}
	_ = up
}

func TestAnonymousWritesNeverReachTheRegistry(t *testing.T) {
	gs, up := newGate(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v2/felis/felis/blobs/uploads/"},
		{http.MethodPut, "/v2/felis/felis/manifests/v0.1.0"},
		{http.MethodPatch, "/v2/user-uploads/s1/blobs/uploads/abc"},
		{http.MethodDelete, "/v2/user-uploads/s1/manifests/sha256:abc"},
	} {
		resp := do(t, gs, c.method, c.path, "", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", c.method, c.path, resp.StatusCode)
		}
		if resp := do(t, gs, c.method, c.path, PrincipalPlatform, "not-it"); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("bad-secret %s %s = %d, want 401", c.method, c.path, resp.StatusCode)
		}
		if resp := do(t, gs, c.method, c.path, "intruder", "plat-secret"); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unknown-user %s %s = %d, want 401", c.method, c.path, resp.StatusCode)
		}
	}
	if n := up.count(); n != 0 {
		t.Fatalf("%d refused writes reached the registry: %v", n, up.seen)
	}
}

func TestBuildPrincipalIsFencedOffPlatformRepos(t *testing.T) {
	gs, up := newGate(t)
	for _, p := range []string{
		"/v2/felis/felis/manifests/v0.1.0",
		"/v2/felis/limbo/blobs/uploads/",
		"/v2/felis/manifests/latest",
		"/v2/mirror/trivy-db/manifests/2",
		"/v2/mirror/trivy-java-db/blobs/uploads/abc",
	} {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
			if resp := do(t, gs, m, p, PrincipalBuild, "build-secret"); resp.StatusCode != http.StatusForbidden {
				t.Errorf("build %s %s = %d, want 403", m, p, resp.StatusCode)
			}
		}
	}
	if resp := do(t, gs, http.MethodDelete, "/v2/user-uploads/s1/manifests/sha256:abc", PrincipalBuild, "build-secret"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("build DELETE = %d, want 403", resp.StatusCode)
	}
	if n := up.count(); n != 0 {
		t.Fatalf("%d refused writes reached the registry: %v", n, up.seen)
	}

	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/v2/user-uploads/s1/blobs/uploads/", http.StatusAccepted},
		{http.MethodPatch, "/v2/user-uploads/s1/blobs/uploads/abc", http.StatusOK},
		{http.MethodPut, "/v2/user-uploads/s1/blobs/uploads/abc?digest=sha256:0", http.StatusCreated},
		{http.MethodPut, "/v2/user-uploads/s1/manifests/latest", http.StatusCreated},
		{http.MethodPut, "/v2/modpacks/pack/manifests/1.0", http.StatusCreated},
		// A repository merely containing "felis" deeper down is not reserved.
		{http.MethodPut, "/v2/builds/felis/manifests/1", http.StatusCreated},
	} {
		if resp := do(t, gs, c.method, c.path, PrincipalBuild, "build-secret"); resp.StatusCode != c.want {
			t.Errorf("build %s %s = %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
	for i, a := range up.auth {
		if a != "" {
			t.Errorf("request %d (%s) reached the registry with an Authorization header", i, up.seen[i])
		}
	}
}

func TestPlatformPrincipalMayWriteAndDeleteAnything(t *testing.T) {
	gs, _ := newGate(t)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPut, "/v2/felis/felis/manifests/v0.2.0", http.StatusCreated},
		{http.MethodPost, "/v2/mirror/trivy-db/blobs/uploads/", http.StatusAccepted},
		{http.MethodDelete, "/v2/user-uploads/s1/manifests/sha256:abc", http.StatusOK},
	} {
		if resp := do(t, gs, c.method, c.path, PrincipalPlatform, "plat-secret"); resp.StatusCode != c.want {
			t.Errorf("platform %s %s = %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
}

func TestPathTricksAreRefusedBeforeAuthorization(t *testing.T) {
	gs, up := newGate(t)
	for _, p := range []string{
		"/v2/user-uploads/../felis/felis/manifests/v1",
		"/v2/user-uploads/./x/manifests/v1",
		"/v2/user-uploads%2F..%2Ffelis/felis/manifests/v1",
		"/v2/user-uploads//felis/manifests/v1",
	} {
		req, _ := http.NewRequest(http.MethodPut, gs.URL, nil)
		req.URL.Opaque = p // send the path exactly as written, no client-side cleaning
		req.SetBasicAuth(PrincipalBuild, "build-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", p, resp.StatusCode)
		}
	}
	if n := up.count(); n != 0 {
		t.Fatalf("%d crafted paths reached the registry: %v", n, up.seen)
	}
}

func TestMissingTokenFailsClosed(t *testing.T) {
	up := &upstreamLog{}
	upSrv := httptest.NewServer(up.handler())
	defer upSrv.Close()
	target, _ := url.Parse(upSrv.URL)
	gs := httptest.NewServer(New(target, map[string]string{PrincipalBuild: ""}, nil))
	defer gs.Close()
	if resp := do(t, gs, http.MethodPut, "/v2/user-uploads/s1/manifests/latest", PrincipalBuild, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty configured token accepted an empty password: %d", resp.StatusCode)
	}
	if resp := do(t, gs, http.MethodGet, "/v2/user-uploads/s1/manifests/latest", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("reads must keep working without tokens: %d", resp.StatusCode)
	}
}

func TestHealth(t *testing.T) {
	gs, _ := newGate(t)
	if resp := do(t, gs, http.MethodGet, "/healthz", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
	dead, _ := url.Parse("http://127.0.0.1:1")
	ds := httptest.NewServer(New(dead, nil, nil))
	defer ds.Close()
	if resp := do(t, ds, http.MethodGet, "/healthz", "", ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("healthz with a dead upstream = %d, want 503", resp.StatusCode)
	}
	if resp := do(t, ds, http.MethodGet, "/livez", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("livez = %d", resp.StatusCode)
	}
}

func TestRepoFromPath(t *testing.T) {
	for path, want := range map[string]string{
		"/v2/":                                 "",
		"/v2/_catalog":                         "",
		"/v2/a/manifests/latest":               "a",
		"/v2/a/b/c/manifests/sha256:0":         "a/b/c",
		"/v2/a/blobs/sha256:0":                 "a",
		"/v2/a/b/blobs/uploads/":               "a/b",
		"/v2/a/b/blobs/uploads/uuid-1":         "a/b",
		"/v2/a/tags/list":                      "a",
		"/v2/user-uploads/blobs/manifests/one": "user-uploads/blobs",
	} {
		if got := RepoFromPath(path); got != want {
			t.Errorf("RepoFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}
