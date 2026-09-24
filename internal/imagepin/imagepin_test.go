package imagepin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testDigest = "sha256:d2fcc09d2caa108678c540c99703db96d63038a5fc9e366402d7ef1712ec4d95"

// fakeRegistry serves /v2/felis/paper/manifests/demo (and the same manifest by
// its digest) and 404s everything else.
func fakeRegistry(t *testing.T, header bool) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			t.Errorf("request without an OCI index Accept: %q", r.Header.Get("Accept"))
		}
		if r.URL.Path != "/v2/felis/paper/manifests/demo" && r.URL.Path != "/v2/felis/paper/manifests/"+testDigest {
			http.NotFound(w, r)
			return
		}
		if header {
			w.Header().Set("Docker-Content-Digest", testDigest)
		}
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"schemaVersion":2}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func resolverFor(srv *httptest.Server) Resolver {
	return Resolver{
		Registry: "registry.felis.svc:5000",
		Endpoint: strings.TrimPrefix(srv.URL, "http://"),
		Client:   srv.Client(),
	}
}

func TestPinResolvesPlatformTag(t *testing.T) {
	srv, seen := fakeRegistry(t, true)
	got, err := resolverFor(srv).Pin(context.Background(), "registry.felis.svc:5000/felis/paper:demo")
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if want := "registry.felis.svc:5000/felis/paper:demo@" + testDigest; got != want {
		t.Errorf("Pin = %q, want %q", got, want)
	}
	if len(*seen) != 1 || (*seen)[0] != "HEAD /v2/felis/paper/manifests/demo" {
		t.Errorf("requests = %v, want a single HEAD", *seen)
	}
}

func TestPinHashesManifestWithoutDigestHeader(t *testing.T) {
	srv, seen := fakeRegistry(t, false)
	got, err := resolverFor(srv).Pin(context.Background(), "registry.felis.svc:5000/felis/paper:demo")
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	sum := sha256.Sum256([]byte(`{"schemaVersion":2}`))
	if want := "registry.felis.svc:5000/felis/paper:demo@sha256:" + hex.EncodeToString(sum[:]); got != want {
		t.Errorf("Pin = %q, want %q", got, want)
	}
	if len(*seen) != 2 {
		t.Errorf("requests = %v, want HEAD then GET", *seen)
	}
}

func TestPinLeavesOtherRefsAlone(t *testing.T) {
	srv, seen := fakeRegistry(t, true)
	r := resolverFor(srv)
	for _, ref := range []string{
		"docker.io/itzg/minecraft-server:java21",               // external
		"docker.io/itzg/minecraft-server:java21@" + testDigest, // external, pinned
		"registry.felis.svc:50000/felis/paper:demo",            // a different port is a different registry
	} {
		got, err := r.Pin(context.Background(), ref)
		if err != nil || got != ref {
			t.Errorf("Pin(%q) = %q, %v; want it unchanged", ref, got, err)
		}
	}
	if len(*seen) != 0 {
		t.Errorf("requests = %v, want none", *seen)
	}
}

// A pinned ref stays as it is, but only while the registry still holds that
// manifest: the pruner deletes builds nothing references, and setting a server
// back to one of them must fail here, not in ImagePullBackOff.
func TestPinChecksAPinnedRefStillExists(t *testing.T) {
	srv, seen := fakeRegistry(t, true)
	r := resolverFor(srv)
	ref := "registry.felis.svc:5000/felis/paper:demo@" + testDigest
	got, err := r.Pin(context.Background(), ref)
	if err != nil || got != ref {
		t.Fatalf("Pin(%q) = %q, %v; want it unchanged", ref, got, err)
	}
	if want := "HEAD /v2/felis/paper/manifests/" + testDigest; len(*seen) != 1 || (*seen)[0] != want {
		t.Errorf("requests = %v, want [%s]", *seen, want)
	}

	pruned := "registry.felis.svc:5000/felis/paper:demo@sha256:" + strings.Repeat("0", 64)
	if _, err := r.Pin(context.Background(), pruned); !errors.Is(err, ErrNotFound) {
		t.Errorf("pruned digest: err = %v, want ErrNotFound", err)
	}
	if _, err := r.Pin(context.Background(), "registry.felis.svc:5000/felis/paper:demo@sha256:nothex"); err == nil {
		t.Error("malformed pinned ref accepted")
	}
}

func TestPinImpliedLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/felis/paper/manifests/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", testDigest)
	}))
	defer srv.Close()
	got, err := resolverFor(srv).Pin(context.Background(), "registry.felis.svc:5000/felis/paper")
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if want := "registry.felis.svc:5000/felis/paper:latest@" + testDigest; got != want {
		t.Errorf("Pin = %q, want %q", got, want)
	}
}

func TestPinErrors(t *testing.T) {
	srv, _ := fakeRegistry(t, true)
	_, err := resolverFor(srv).Pin(context.Background(), "registry.felis.svc:5000/felis/paper:gone")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tag: err = %v, want ErrNotFound", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:nothex")
	}))
	defer bad.Close()
	if _, err := resolverFor(bad).Pin(context.Background(), "registry.felis.svc:5000/felis/paper:demo"); err == nil {
		t.Error("malformed digest accepted")
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	_, err = resolverFor(down).Pin(context.Background(), "registry.felis.svc:5000/felis/paper:demo")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("503: err = %v, want a non-NotFound error", err)
	}
}
