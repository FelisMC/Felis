package artifactcache

import (
	"context"
	"crypto/sha1" //nolint:gosec // the index serves sha1 requests
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"felis.lolicon.best/internal/software"
)

// origin stands in for an official download host: it serves files by path and
// counts how often each was downloaded.
type origin struct {
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]*int32
	// gate, when set, holds every response until it is closed.
	gate chan struct{}
}

func newOrigin(t *testing.T) *origin {
	o := &origin{files: map[string][]byte{}, hits: map[string]*int32{}}
	o.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		body, ok := o.files[r.URL.Path]
		n := o.hits[r.URL.Path]
		gate := o.gate
		o.mu.Unlock()
		if n != nil {
			atomic.AddInt32(n, 1)
		}
		if gate != nil {
			<-gate
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *origin) put(path string, body []byte) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.files[path] = body
	var n int32
	o.hits[path] = &n
	return o.srv.URL + path
}

func (o *origin) count(path string) int32 { return atomic.LoadInt32(o.hits[path]) }

func sha256Of(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func sha1Of(b []byte) string   { s := sha1.Sum(b); return hex.EncodeToString(s[:]) } //nolint:gosec // see import

// newCache is a cache that downloads from the origin's host only.
func newCache(t *testing.T, o *origin) (*Cache, http.Handler) {
	host := strings.TrimPrefix(o.srv.URL, "https://")
	c := &Cache{
		Dir:      t.TempDir(),
		Upstream: o.srv.Client(),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Allow:    func(h string) bool { return h == host },
	}
	return c, c
}

// fetchVia asks the cache for src by digest.
func fetchVia(h http.Handler, src, algo, digest string) *httptest.ResponseRecorder {
	q := url.Values{"url": {src}, "algo": {algo}, "digest": {digest}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/fetch?"+q.Encode(), nil))
	return w
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct{ Error string }
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return e.Error
}

// Ten requests for one file, five of them at once, download it from upstream
// once; every answer is that file.
func TestCacheDownloadsEachFileOnce(t *testing.T) {
	o := newOrigin(t)
	body := []byte("paper-1.21.4-232.jar contents")
	src := o.put("/v1/objects/paper.jar", body)
	_, h := newCache(t, o)

	gate := make(chan struct{})
	o.mu.Lock()
	o.gate = gate
	o.mu.Unlock()
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 5)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = fetchVia(h, src, software.SHA256, sha256Of(body))
		}(i)
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	o.mu.Lock()
	o.gate = nil
	o.mu.Unlock()
	for i := 0; i < 5; i++ {
		results = append(results, fetchVia(h, src, software.SHA256, sha256Of(body)))
	}
	for i, w := range results {
		if w.Code != http.StatusOK || w.Body.String() != string(body) {
			t.Fatalf("request %d = %d %q", i, w.Code, w.Body.String())
		}
	}
	if n := o.count("/v1/objects/paper.jar"); n != 1 {
		t.Errorf("upstream served the file %d times, want once", n)
	}
	if got := results[len(results)-1].Header().Get("X-Felis-Cache"); got != "hit" {
		t.Errorf("later request X-Felis-Cache = %q, want hit", got)
	}
}

// What upstream serves is kept only if it hashes to the digest asked for: a
// mismatch stores nothing, and the next request for that digest goes upstream
// again and gets the real file once upstream serves it.
func TestCacheKeepsOnlyContentMatchingTheDigest(t *testing.T) {
	o := newOrigin(t)
	real := []byte("the real jar")
	src := o.put("/jar", []byte("something else"))
	_, h := newCache(t, o)

	w := fetchVia(h, src, software.SHA256, sha256Of(real))
	if w.Code != http.StatusBadGateway || errCode(t, w) != "digest_mismatch" {
		t.Fatalf("mismatching content = %d %s, want 502 digest_mismatch", w.Code, w.Body.String())
	}
	o.put("/jar", real)
	w = fetchVia(h, src, software.SHA256, sha256Of(real))
	if w.Code != http.StatusOK || w.Body.String() != string(real) {
		t.Fatalf("after upstream recovered = %d %q", w.Code, w.Body.String())
	}
}

// A request names a URL; the cache only downloads from the official hosts, over
// https, and follows no redirect off them.
func TestCacheOnlyDownloadsFromAllowedHosts(t *testing.T) {
	o := newOrigin(t)
	_, h := newCache(t, o)
	body := []byte("x")
	for _, tc := range []struct {
		src    string
		status int
		code   string
	}{
		{"https://evil.example.net/x.jar", http.StatusForbidden, "host_not_allowed"},
		{"http://" + strings.TrimPrefix(o.srv.URL, "https://") + "/x.jar", http.StatusBadRequest, "bad_url"},
		{"https://user:pw@" + strings.TrimPrefix(o.srv.URL, "https://") + "/x.jar", http.StatusBadRequest, "bad_url"},
		{"file:///etc/passwd", http.StatusBadRequest, "bad_url"},
	} {
		w := fetchVia(h, tc.src, software.SHA256, sha256Of(body))
		if w.Code != tc.status || errCode(t, w) != tc.code {
			t.Errorf("%s = %d %s, want %d %s", tc.src, w.Code, w.Body.String(), tc.status, tc.code)
		}
	}

	// Both redirect targets answer with the right bytes, so only the redirect
	// rule stands between the request and a stored file.
	serve := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) })
	offList := httptest.NewTLSServer(serve)
	defer offList.Close()
	plain := httptest.NewServer(serve)
	defer plain.Close()
	plainHost := strings.TrimPrefix(plain.URL, "http://")
	for _, target := range []string{offList.URL + "/x.jar", plain.URL + "/x.jar"} {
		redirect := httptest.NewTLSServer(http.RedirectHandler(target, http.StatusFound))
		host := strings.TrimPrefix(redirect.URL, "https://")
		c := &Cache{Dir: t.TempDir(), Upstream: redirect.Client(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Allow: func(h string) bool { return h == host || h == plainHost }}
		if w := fetchVia(c, redirect.URL+"/x.jar", software.SHA256, sha256Of(body)); w.Code != http.StatusBadGateway {
			t.Errorf("redirect to %s = %d %s, want 502", target, w.Code, w.Body.String())
		}
		redirect.Close()
	}
	for _, h := range []string{"piston-data.mojang.com", "fill-data.papermc.io", "maven.neoforged.net", "cdn.modrinth.com"} {
		if !HostAllowed(h) {
			t.Errorf("%s is an official host and must be allowed", h)
		}
	}
	for _, h := range []string{"evil.example.net", "mojang.com.evil.example.net", "10.43.0.1", "localhost", "piston-data.mojang.com:8443"} {
		if HostAllowed(h) {
			t.Errorf("%s allowed", h)
		}
	}
}

// A file fetched by its sha256 serves a later request naming its sha1 (how
// Mojang and Maven publish) without going upstream.
func TestCacheServesWeakerDigestsOfStoredFiles(t *testing.T) {
	o := newOrigin(t)
	body := []byte("minecraft server jar")
	src := o.put("/server.jar", body)
	_, h := newCache(t, o)
	if w := fetchVia(h, src, software.SHA256, sha256Of(body)); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w := fetchVia(h, src, software.SHA1, sha1Of(body))
	if w.Code != http.StatusOK || w.Body.String() != string(body) || w.Header().Get("X-Felis-Cache") != "hit" {
		t.Errorf("by sha1 = %d %q %s", w.Code, w.Body.String(), w.Header().Get("X-Felis-Cache"))
	}
	if n := o.count("/server.jar"); n != 1 {
		t.Errorf("upstream hit %d times", n)
	}
}

// Over its cap the cache drops the files served longest ago.
func TestCacheEvictsTheLeastRecentlyServed(t *testing.T) {
	o := newOrigin(t)
	c, h := newCache(t, o)
	c.MaxBytes = 25
	a, b, d := []byte(strings.Repeat("a", 10)), []byte(strings.Repeat("b", 10)), []byte(strings.Repeat("d", 10))
	srcA, srcB, srcD := o.put("/a", a), o.put("/b", b), o.put("/d", d)
	fetchVia(h, srcA, software.SHA256, sha256Of(a))
	time.Sleep(20 * time.Millisecond)
	fetchVia(h, srcB, software.SHA256, sha256Of(b))
	time.Sleep(20 * time.Millisecond)
	fetchVia(h, srcA, software.SHA256, sha256Of(a)) // a is now the more recent
	time.Sleep(20 * time.Millisecond)
	fetchVia(h, srcD, software.SHA256, sha256Of(d))

	for name, sum := range map[string]string{"b": sha256Of(b)} {
		if _, err := os.Stat(c.blobPath(sum)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s, least recently served, was kept (stat err %v)", name, err)
		}
	}
	for name, sum := range map[string]string{"a": sha256Of(a), "d": sha256Of(d)} {
		if _, err := os.Stat(c.blobPath(sum)); err != nil {
			t.Errorf("%s was evicted: %v", name, err)
		}
	}
}

// The installer never trusts the cache: a cache answering with the wrong bytes
// costs a direct download, and a direct download with the wrong bytes fails.
func TestClientChecksEveryByteItself(t *testing.T) {
	o := newOrigin(t)
	body := []byte("fabric-loader-0.19.5.jar")
	src := o.put("/loader.jar", body)
	liar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tampered by the cache"))
	}))
	defer liar.Close()
	f := software.File{Path: "libraries/loader.jar", URL: src, Digest: software.Digest{Algo: software.SHA256, Hex: sha256Of(body)}}
	dest := filepath.Join(t.TempDir(), "loader.jar")
	c := &Client{CacheURL: liar.URL, Direct: o.srv.Client(), Log: io.Discard}
	if err := c.Fetch(context.Background(), f, dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != string(body) {
		t.Errorf("dest = %q, want the real file", got)
	}
	if n := o.count("/loader.jar"); n != 1 {
		t.Errorf("direct download count = %d, want 1 after the cache lied", n)
	}

	o.put("/loader.jar", []byte("tampered upstream"))
	dest2 := filepath.Join(t.TempDir(), "loader.jar")
	if err := c.Fetch(context.Background(), f, dest2); !errors.Is(err, software.ErrDigestMismatch) {
		t.Errorf("tampered upstream: err = %v, want ErrDigestMismatch", err)
	}
	if _, err := os.Stat(dest2); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a tampered file reached its destination (stat err %v)", err)
	}
}

// A connection that stops delivering bytes is dropped after the idle timeout,
// not after the overall one.
func TestClientDropsAStalledDownload(t *testing.T) {
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
		w.(http.Flusher).Flush()
		<-stall
	}))
	defer srv.Close()
	defer close(stall)
	c := &Client{IdleTimeout: 200 * time.Millisecond, Log: io.Discard}
	f := software.File{Path: "x.jar", URL: srv.URL + "/x.jar", Digest: software.Digest{Algo: software.SHA256, Hex: strings.Repeat("0", 64)}}
	start := time.Now()
	err := c.Fetch(context.Background(), f, filepath.Join(t.TempDir(), "x.jar"))
	if err == nil {
		t.Fatal("a stalled download succeeded")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("gave up after %s, want about two idle timeouts (one retry)", d)
	}
}

// A slow download that keeps delivering is not a stalled one: each byte that
// arrives restarts the idle timeout.
func TestClientKeepsASlowButSteadyDownload(t *testing.T) {
	body := []byte(strings.Repeat("y", 8))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		for i := range body {
			_, _ = w.Write(body[i : i+1])
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()
	c := &Client{IdleTimeout: 300 * time.Millisecond, Log: io.Discard}
	f := software.File{Path: "y.jar", URL: srv.URL + "/y.jar", Digest: software.Digest{Algo: software.SHA256, Hex: sha256Of(body)}}
	if err := c.Fetch(context.Background(), f, filepath.Join(t.TempDir(), "y.jar")); err != nil {
		t.Fatalf("a download delivering a byte every 100ms, with a 300ms idle timeout: %v", err)
	}
}
