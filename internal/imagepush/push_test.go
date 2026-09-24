package imagepush

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"felis.lolicon.best/internal/registrygate"
)

// fakeRegistry is the distribution v2 subset the pusher speaks, backed by maps. It
// checks what a real registry checks: an upload's bytes hash to the digest the
// client claims, and a manifest references only blobs the repository holds.
type fakeRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte // repo@digest -> bytes
	manifests map[string][]byte // repo:tag -> manifest
	types     map[string]string // repo:tag -> content type
	uploads   int
	puts      int
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{blobs: map[string][]byte{}, manifests: map[string][]byte{}, types: map[string]string{}}
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	repo := registrygate.RepoFromPath(r.URL.Path)
	p := r.URL.Path
	switch {
	case r.Method == http.MethodHead && strings.Contains(p, "/blobs/sha256:"):
		d := p[strings.LastIndex(p, "/")+1:]
		if _, ok := f.blobs[repo+"@"+d]; ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/blobs/uploads/"):
		f.uploads++
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/u%d?_state=x", repo, f.uploads))
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut && strings.Contains(p, "/blobs/uploads/"):
		if r.URL.Query().Get("_state") != "x" {
			http.Error(w, "upload state lost", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		got := "sha256:" + hex.EncodeToString(sum[:])
		if want := r.URL.Query().Get("digest"); want != got {
			http.Error(w, `{"errors":[{"code":"DIGEST_INVALID"}]}`, http.StatusBadRequest)
			return
		}
		f.blobs[repo+"@"+got] = body
		f.puts++
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut && strings.Contains(p, "/manifests/"):
		body, _ := io.ReadAll(r.Body)
		var m manifest
		if err := json.Unmarshal(body, &m); err != nil {
			http.Error(w, "bad manifest", http.StatusBadRequest)
			return
		}
		for _, d := range append([]descriptor{m.Config}, m.Layers...) {
			b, ok := f.blobs[repo+"@"+d.Digest]
			if !ok || int64(len(b)) != d.Size {
				http.Error(w, `{"errors":[{"code":"MANIFEST_BLOB_UNKNOWN"}]}`, http.StatusBadRequest)
				return
			}
		}
		tag := p[strings.LastIndex(p, "/")+1:]
		f.manifests[repo+":"+tag] = body
		f.types[repo+":"+tag] = r.Header.Get("Content-Type")
		sum := sha256.Sum256(body)
		w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
		w.WriteHeader(http.StatusCreated)
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, http.StatusNotImplemented)
	}
}

// writeTarball writes the layout Kaniko's --tar-path produces (go-containerregistry
// tarball.Write): the config under its digest, gzip layers as <hex>.tar.gz, and a
// manifest.json tying them to the destination tag.
func writeTarball(t *testing.T, ref string, layers ...[]byte) string {
	t.Helper()
	cfg := []byte(`{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	cfgSum := sha256.Sum256(cfg)
	cfgName := "sha256:" + hex.EncodeToString(cfgSum[:])

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name string, b []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	add(cfgName, cfg)
	var names []string
	for _, l := range layers {
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		zw.Write(l)
		zw.Close()
		sum := sha256.Sum256(gz.Bytes())
		name := hex.EncodeToString(sum[:]) + ".tar.gz"
		add(name, gz.Bytes())
		names = append(names, name)
	}
	mj, _ := json.Marshal([]tarManifest{{Config: cfgName, RepoTags: []string{ref}, Layers: names}})
	add("manifest.json", mj)
	tw.Close()

	path := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func startStack(t *testing.T) (*fakeRegistry, string) {
	t.Helper()
	reg := newFakeRegistry()
	upstream := httptest.NewServer(reg)
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	gate := httptest.NewServer(registrygate.New(u, map[string]string{
		registrygate.PrincipalBuild:    "build-secret",
		registrygate.PrincipalPlatform: "plat-secret",
	}, nil))
	t.Cleanup(gate.Close)
	return reg, strings.TrimPrefix(gate.URL, "http://")
}

func TestPushThroughTheGate(t *testing.T) {
	reg, host := startStack(t)
	ref := host + "/user-uploads/sub-1:latest"
	tarPath := writeTarball(t, ref, []byte("layer one"), []byte("layer two"))
	p := &Pusher{Scheme: "http", Username: registrygate.PrincipalBuild, Password: "build-secret", Attempts: 1}

	digest, err := p.Push(context.Background(), tarPath, ref)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	body := reg.manifests["user-uploads/sub-1:latest"]
	if body == nil {
		t.Fatal("no manifest recorded")
	}
	sum := sha256.Sum256(body)
	if digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("returned digest %s does not name the stored manifest", digest)
	}
	if ct := reg.types["user-uploads/sub-1:latest"]; ct != mediaDockerManifest {
		t.Fatalf("manifest content type %q, want %q", ct, mediaDockerManifest)
	}
	var m manifest
	json.Unmarshal(body, &m)
	if len(m.Layers) != 2 || m.Layers[0].MediaType != mediaDockerLayerGz || m.Config.MediaType != mediaDockerConfig {
		t.Fatalf("manifest shape: %+v", m)
	}
	if reg.puts != 3 {
		t.Fatalf("uploaded %d blobs, want 3 (config + 2 layers)", reg.puts)
	}

	// A second push of the same image re-sends only the manifest.
	if _, err := p.Push(context.Background(), tarPath, ref); err != nil {
		t.Fatalf("re-push: %v", err)
	}
	if reg.puts != 3 {
		t.Fatalf("re-push uploaded %d blobs in total, want the 3 from the first push", reg.puts)
	}
}

func TestPushIntoAReservedRepoIsRefusedWithoutRetry(t *testing.T) {
	reg, host := startStack(t)
	ref := host + "/felis/felis:v0.1.0"
	tarPath := writeTarball(t, ref, []byte("evil"))
	p := &Pusher{Scheme: "http", Username: registrygate.PrincipalBuild, Password: "build-secret"}

	_, err := p.Push(context.Background(), tarPath, ref)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("push into felis/ = %v, want a 403 StatusError", err)
	}
	if reg.uploads != 0 || len(reg.manifests) != 0 {
		t.Fatalf("a refused push reached the registry: uploads=%d manifests=%d", reg.uploads, len(reg.manifests))
	}
}

func TestPushWithoutCredentialsIsRefused(t *testing.T) {
	reg, host := startStack(t)
	ref := host + "/user-uploads/sub-2:latest"
	tarPath := writeTarball(t, ref, []byte("x"))
	p := &Pusher{Scheme: "http"}
	if _, err := p.Push(context.Background(), tarPath, ref); err == nil {
		t.Fatal("anonymous push succeeded")
	}
	if len(reg.manifests) != 0 {
		t.Fatal("anonymous push stored a manifest")
	}
}

func TestTamperedTarballIsRefused(t *testing.T) {
	_, host := startStack(t)
	ref := host + "/user-uploads/sub-3:latest"
	dir := t.TempDir()
	// A config whose name is not its digest.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	cfg := []byte(`{}`)
	tw.WriteHeader(&tar.Header{Name: "sha256:" + strings.Repeat("0", 64), Mode: 0o644, Size: int64(len(cfg))})
	tw.Write(cfg)
	mj, _ := json.Marshal([]tarManifest{{Config: "sha256:" + strings.Repeat("0", 64), Layers: []string{"l.tar.gz"}}})
	tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(mj))})
	tw.Write(mj)
	tw.Close()
	path := filepath.Join(dir, "bad.tar")
	os.WriteFile(path, buf.Bytes(), 0o644)
	p := &Pusher{Scheme: "http", Username: registrygate.PrincipalBuild, Password: "build-secret"}
	if _, err := p.Push(context.Background(), path, ref); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("tampered config = %v, want a digest mismatch", err)
	}
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]Ref{
		"registry.felis.svc:5000/user-uploads/s:latest": {"registry.felis.svc:5000", "user-uploads/s", "latest"},
		"127.0.0.1:5000/a/b/c:1.2":                      {"127.0.0.1:5000", "a/b/c", "1.2"},
		"registry.felis.svc:5000/untagged":              {"registry.felis.svc:5000", "untagged", "latest"},
	} {
		got, err := ParseRef(in)
		if err != nil || got != want {
			t.Errorf("ParseRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "busybox", "library/busybox:1", "r.io/x@sha256:00", "r.io/", "r.io/x:"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted", bad)
		}
	}
}
