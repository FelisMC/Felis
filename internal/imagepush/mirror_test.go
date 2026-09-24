package imagepush

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/registrygate"
)

// fakeSource is a public registry that answers anonymous requests with a bearer
// challenge, like ghcr.io: a token from /token is required for every read.
type fakeSource struct {
	manifests map[string][]byte // reference (tag or digest) → body
	types     map[string]string
	blobs     map[string][]byte
	tokens    int
	srv       *httptest.Server
}

func newFakeSource(t *testing.T) *fakeSource {
	f := &fakeSource{manifests: map[string][]byte{}, types: map[string]string{}, blobs: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if r.URL.Query().Get("scope") != "repository:tools/thing:pull" || r.URL.Query().Get("service") != "fake" {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			f.tokens++
			fmt.Fprint(w, `{"token":"t0k"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer t0k" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake",scope="repository:tools/thing:pull"`, f.srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			b, ok := f.manifests[ref]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", f.types[ref])
			w.Write(b)
		case strings.Contains(r.URL.Path, "/blobs/"):
			b, ok := f.blobs[ref]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSource) host() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeSource) addBlob(b []byte) descriptor {
	d := digestOf(b)
	f.blobs[d] = b
	return descriptor{MediaType: mediaOCILayerGz, Size: int64(len(b)), Digest: d}
}

// addImage stores a single-platform manifest and returns its digest.
func (f *fakeSource) addImage(arch string) string {
	cfg := f.addBlob([]byte(`{"architecture":"` + arch + `","os":"linux"}`))
	cfg.MediaType = mediaOCIConfig
	layer := f.addBlob([]byte("layer for " + arch))
	body, _ := json.Marshal(manifest{SchemaVersion: 2, MediaType: mediaOCIManifest, Config: cfg, Layers: []descriptor{layer}})
	d := digestOf(body)
	f.manifests[d] = body
	f.types[d] = mediaOCIManifest
	return d
}

// addIndex stores an index over amd64 and arm64 under tag and returns the
// index digest and the arm64 manifest digest.
func (f *fakeSource) addIndex(tag string) (index, arm64 string) {
	amd := f.addImage("amd64")
	arm := f.addImage("arm64")
	body := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[`+
		`{"mediaType":%q,"digest":%q,"size":1,"platform":{"os":"linux","architecture":"amd64"}},`+
		`{"mediaType":%q,"digest":%q,"size":1,"platform":{"os":"linux","architecture":"arm64","variant":"v8"}}]}`,
		mediaOCIIndex, mediaOCIManifest, amd, mediaOCIManifest, arm)
	d := digestOf([]byte(body))
	f.manifests[tag], f.manifests[d] = []byte(body), []byte(body)
	f.types[tag], f.types[d] = mediaOCIIndex, mediaOCIIndex
	return d, arm
}

func TestMirrorCopiesThePlatformManifest(t *testing.T) {
	src := newFakeSource(t)
	idx, arm := src.addIndex("v1")
	reg, host := startStack(t)
	p := &Pusher{Scheme: "http", Username: registrygate.PrincipalPlatform, Password: "plat-secret", Attempts: 1}
	s := &Source{Scheme: "http", Platform: "linux/arm64"}

	got, err := p.Mirror(context.Background(), s, src.host()+"/tools/thing:v1@"+idx, host+"/mirror/thing:v1")
	if err != nil {
		t.Fatal(err)
	}
	if got != arm {
		t.Errorf("mirrored digest = %s, want the arm64 manifest %s", got, arm)
	}
	if string(reg.manifests["mirror/thing:v1"]) != string(src.manifests[arm]) {
		t.Error("the destination manifest differs from the source's bytes")
	}
	if reg.puts != 2 {
		t.Errorf("uploaded %d blobs, want the config and one layer", reg.puts)
	}
	if src.tokens != 1 {
		t.Errorf("fetched %d tokens, want one reused for every read", src.tokens)
	}

	// A second run finds every blob in place and only re-puts the manifest.
	if _, err := p.Mirror(context.Background(), s, src.host()+"/tools/thing:v1@"+idx, host+"/mirror/thing:v1"); err != nil {
		t.Fatal(err)
	}
	if reg.puts != 2 {
		t.Errorf("second run uploaded blobs again: %d", reg.puts)
	}
}

func TestMirrorRefusesAMovedPin(t *testing.T) {
	src := newFakeSource(t)
	src.addIndex("v1")
	other, _ := src.addIndex("v2")
	// Different bytes under the pinned digest: a tampered or re-pushed source.
	src.manifests[other] = []byte(`{"schemaVersion":2,"mediaType":"` + mediaOCIIndex + `","manifests":[]}`)
	reg, host := startStack(t)
	p := &Pusher{Scheme: "http", Username: registrygate.PrincipalPlatform, Password: "plat-secret", Attempts: 1}
	_, err := p.Mirror(context.Background(), &Source{Scheme: "http"}, src.host()+"/tools/thing:v1@"+other, host+"/mirror/thing:v1")
	if err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("Mirror = %v, want a digest mismatch", err)
	}
	if len(reg.manifests) != 0 || reg.puts != 0 {
		t.Error("a refused source still wrote to the registry")
	}
}

func TestMirrorFollowsATagForAnArtifact(t *testing.T) {
	src := newFakeSource(t)
	d := src.addImage("amd64")
	src.manifests["2"], src.types["2"] = src.manifests[d], mediaOCIManifest
	reg, host := startStack(t)
	p := &Pusher{Scheme: "http", Username: registrygate.PrincipalPlatform, Password: "plat-secret", Attempts: 1}
	got, err := p.Mirror(context.Background(), &Source{Scheme: "http"}, src.host()+"/tools/thing:2", host+"/mirror/thing-db:2")
	if err != nil {
		t.Fatal(err)
	}
	if got != d || reg.manifests["mirror/thing-db:2"] == nil {
		t.Errorf("Mirror = %s, manifests %v", got, reg.manifests)
	}
}

func TestParseSourceRef(t *testing.T) {
	for in, want := range map[string]SourceRef{
		"ghcr.io/aquasecurity/trivy:0.74.0":                    {Host: "ghcr.io", Repo: "aquasecurity/trivy", Tag: "0.74.0"},
		"mirror.gcr.io/aquasec/trivy-db:2":                     {Host: "mirror.gcr.io", Repo: "aquasec/trivy-db", Tag: "2"},
		"docker.io/registry:2":                                 {Host: "registry-1.docker.io", Repo: "library/registry", Tag: "2"},
		"gcr.io/x/y":                                           {Host: "gcr.io", Repo: "x/y", Tag: "latest"},
		"localhost:5000/a/b@sha256:" + strings.Repeat("a", 64): {Host: "localhost:5000", Repo: "a/b", Digest: "sha256:" + strings.Repeat("a", 64)},
		"gcr.io/k/e:v1@sha256:" + strings.Repeat("b", 64):      {Host: "gcr.io", Repo: "k/e", Tag: "v1", Digest: "sha256:" + strings.Repeat("b", 64)},
	} {
		got, err := ParseSourceRef(in)
		if err != nil || got != want {
			t.Errorf("ParseSourceRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"trivy:latest", "ghcr.io/", "gcr.io/x@sha256:zz", "gcr.io/x:"} {
		if _, err := ParseSourceRef(bad); err == nil {
			t.Errorf("ParseSourceRef(%q) accepted", bad)
		}
	}
}

func TestMirrorStatusRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "status.json")
	if st, err := ReadMirrorStatus(path); st != nil || err != nil {
		t.Fatalf("missing file = %v, %v", st, err)
	}
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	if err := WriteMirrorStatus(path, MirrorStatus{LastAttempt: now, LastSuccess: now}); err != nil {
		t.Fatal(err)
	}
	st, err := ReadMirrorStatus(path)
	if err != nil || !st.LastSuccess.Equal(now) || st.LastError != "" {
		t.Errorf("round trip = %+v, %v", st, err)
	}
}
