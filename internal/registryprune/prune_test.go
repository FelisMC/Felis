package registryprune

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"felis.lolicon.best/internal/registrygate"
)

const host = "registry.felis.svc:5000"

var now = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func dg(c string) string { return "sha256:" + strings.Repeat(c, 64) }

func rev(c string, age time.Duration) registrygate.Revision {
	return registrygate.Revision{Digest: dg(c), Pushed: now.Add(-age)}
}

const day = 24 * time.Hour

func targets(ts []Target) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Repo+"@"+t.Digest[7:8])
	}
	sort.Strings(out)
	return out
}

func TestPlanKeepsWhatIsReferencedOrRecent(t *testing.T) {
	indexes := map[string]*registrygate.Index{
		// Rebuilt twice: a is untagged and pinned by a server, b untagged and
		// unused, c is :latest and whitelisted.
		"user-uploads/sub-1": {
			Revisions: []registrygate.Revision{rev("a", 30*day), rev("b", 20*day), rev("c", 10*day)},
			Tags:      map[string]string{"latest": dg("c")},
		},
		// Whitelist entry removed, no server: goes.
		"user-uploads/sub-2": {
			Revisions: []registrygate.Revision{rev("d", 5*day)},
			Tags:      map[string]string{"latest": dg("d")},
		},
		// Pushed an hour ago, not admitted yet: the grace keeps it.
		"user-uploads/sub-3": {
			Revisions: []registrygate.Revision{rev("e", time.Hour)},
			Tags:      map[string]string{"latest": dg("e")},
		},
		// A wildcard whitelist entry keeps every tag, not an untagged leftover.
		"modpacks/pack": {
			Revisions: []registrygate.Revision{rev("f", 9*day), rev("1", 8*day), rev("2", 7*day)},
			Tags:      map[string]string{"1.0": dg("f"), "1.1": dg("1")},
		},
	}
	refs := []string{
		host + "/user-uploads/sub-1:latest",
		host + "/user-uploads/sub-1:latest@" + dg("a"),
		host + "/modpacks/pack:*",
		"docker.io/library/nginx:latest",
		"",
	}
	got := targets(Plan(indexes, refs, host, now, day, 5))
	want := []string{"modpacks/pack@2", "user-uploads/sub-1@b", "user-uploads/sub-2@d"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
}

func TestPlanKeepsTheNewestTaggedPlatformImages(t *testing.T) {
	indexes := map[string]*registrygate.Index{
		// Seven installer runs: b1..b7 tagged, plus untagged leftovers x (pinned by
		// a server) and y.
		"felis/felis": {
			Revisions: []registrygate.Revision{
				rev("1", 70*day), rev("2", 60*day), rev("3", 50*day), rev("4", 40*day),
				rev("5", 30*day), rev("6", 20*day), rev("7", 10*day),
				rev("x", 80*day), rev("y", 90*day),
			},
			Tags: map[string]string{
				"b1": dg("1"), "b2": dg("2"), "b3": dg("3"), "b4": dg("4"),
				"b5": dg("5"), "b6": dg("6"), "b7": dg("7"),
			},
		},
		// The scanner DB mirror: one tag, kept however old.
		"mirror/trivy-db": {
			Revisions: []registrygate.Revision{rev("8", 200*day), rev("9", 300*day)},
			Tags:      map[string]string{"2": dg("8")},
		},
	}
	refs := []string{
		// The running control plane is older than the newest three.
		host + "/felis/felis:b2",
		host + "/felis/felis:b5@" + dg("x"),
	}
	got := targets(Plan(indexes, refs, host, now, day, 3))
	want := []string{"felis/felis@1", "felis/felis@3", "felis/felis@4", "felis/felis@y", "mirror/trivy-db@9"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
}

func TestParseRef(t *testing.T) {
	for _, c := range []struct {
		ref, repo, tag, digest string
		ok                     bool
	}{
		{host + "/felis/paper:demo", "felis/paper", "demo", "", true},
		{host + "/felis/paper:demo@" + dg("a"), "felis/paper", "demo", dg("a"), true},
		{host + "/felis/paper@" + dg("a"), "felis/paper", "", dg("a"), true},
		{host + "/felis/paper", "felis/paper", "latest", "", true},
		{host + "/pack:*", "pack", "*", "", true},
		{"other:5000/felis/paper:demo", "", "", "", false},
		{host + "/", "", "", "", false},
	} {
		repo, tag, digest, ok := parseRef(c.ref, host)
		if repo != c.repo || tag != c.tag || digest != c.digest || ok != c.ok {
			t.Errorf("parseRef(%q) = %q %q %q %v, want %q %q %q %v", c.ref, repo, tag, digest, ok, c.repo, c.tag, c.digest, c.ok)
		}
	}
}

// fakeRegistry serves _catalog (paged by one), the manifest index and DELETE.
type fakeRegistry struct {
	mu      sync.Mutex
	indexes map[string]*registrygate.Index
	deletes []string
	auth    []string
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/v2/_catalog":
		var repos []string
		for repo := range f.indexes {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		last := r.URL.Query().Get("last")
		var page []string
		for _, repo := range repos {
			if repo > last {
				page = append(page, repo)
				break
			}
		}
		if len(page) == 1 && page[0] != repos[len(repos)-1] {
			w.Header().Set("Link", `</v2/_catalog?last=`+url.QueryEscape(page[0])+`&n=1>; rel="next"`)
		}
		_ = json.NewEncoder(w).Encode(map[string][]string{"repositories": page})
	case strings.HasPrefix(r.URL.Path, registrygate.IndexPathPrefix):
		idx := f.indexes[strings.TrimPrefix(r.URL.Path, registrygate.IndexPathPrefix)]
		_ = json.NewEncoder(w).Encode(idx)
	case r.Method == http.MethodDelete:
		user, pass, _ := r.BasicAuth()
		f.auth = append(f.auth, user+":"+pass)
		f.deletes = append(f.deletes, r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
	default:
		http.NotFound(w, r)
	}
}

func TestRunDeletesThroughTheGateAsThePrunePrincipal(t *testing.T) {
	reg := &fakeRegistry{indexes: map[string]*registrygate.Index{
		"e2e/probe":          {Revisions: []registrygate.Revision{rev("a", 3*day)}, Tags: map[string]string{"latest": dg("a")}},
		"user-uploads/sub-1": {Revisions: []registrygate.Revision{rev("b", 3*day)}, Tags: map[string]string{"latest": dg("b")}},
		"felis/paper":        {Revisions: []registrygate.Revision{rev("c", 3*day)}, Tags: map[string]string{"demo": dg("c")}},
	}}
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	p := &Pruner{
		Registry: &Client{Endpoint: srv.URL, Token: "prune-secret"},
		Host:     host,
		Refs: func(context.Context) ([]string, error) {
			return []string{host + "/user-uploads/sub-1:latest"}, nil
		},
		Now: func() time.Time { return now },
	}
	rep, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Repositories != 3 || rep.Manifests != 3 || len(rep.Deleted) != 1 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want 3 repos, 3 manifests, 1 deleted", rep)
	}
	if want := "/v2/e2e/probe/manifests/" + dg("a"); len(reg.deletes) != 1 || reg.deletes[0] != want {
		t.Fatalf("deletes = %v, want [%s]", reg.deletes, want)
	}
	if reg.auth[0] != registrygate.PrincipalPrune+":prune-secret" {
		t.Fatalf("delete sent as %q, want the prune principal", reg.auth[0])
	}
}

func TestRunAbortsOnAPartialView(t *testing.T) {
	reg := &fakeRegistry{indexes: map[string]*registrygate.Index{
		"e2e/probe": {Revisions: []registrygate.Revision{rev("a", 3*day)}, Tags: map[string]string{"latest": dg("a")}},
	}}
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	p := &Pruner{
		Registry: &Client{Endpoint: srv.URL, Token: "t"},
		Host:     host,
		Refs: func(context.Context) ([]string, error) {
			return nil, errors.New("cluster unreachable")
		},
		Now: func() time.Time { return now },
	}
	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("run with no view of the servers succeeded")
	}
	if len(reg.deletes) != 0 {
		t.Fatalf("deleted %v without knowing what is in use", reg.deletes)
	}
}

// Through the real gate: the index comes off the data directory, the delete is
// authorized as the prune principal and reaches the registry without the
// credential.
func TestRunThroughTheGate(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "docker/registry/v2/repositories/e2e/probe/_manifests")
	hex := strings.Repeat("a", 64)
	for _, dir := range []string{"revisions/sha256/" + hex, "tags/latest/current"} {
		if err := os.MkdirAll(filepath.Join(repoDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"revisions/sha256/" + hex + "/link", "tags/latest/current/link"} {
		if err := os.WriteFile(filepath.Join(repoDir, f), []byte("sha256:"+hex), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var upstream []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstream = append(upstream, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		switch {
		case r.URL.Path == "/v2/_catalog":
			_, _ = w.Write([]byte(`{"repositories":["e2e/probe"]}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	target, _ := url.Parse(up.URL)
	g := registrygate.New(target, map[string]string{
		registrygate.PrincipalPrune: "prune-secret",
		registrygate.PrincipalBuild: "build-secret",
	}, nil)
	g.DataDir = root
	gate := httptest.NewServer(g)
	t.Cleanup(gate.Close)

	refs := func(context.Context) ([]string, error) { return nil, nil }
	later := func() time.Time { return time.Now().Add(48 * time.Hour) }

	// The build principal's token is not good for a delete.
	wrong := &Pruner{Registry: &Client{Endpoint: gate.URL, Token: "build-secret"}, Host: host, Refs: refs, Now: later}
	if rep, err := wrong.Run(context.Background()); err != nil || rep.Failed != 1 || len(rep.Deleted) != 0 {
		t.Fatalf("run with the wrong token = %+v, %v; want one refused delete", rep, err)
	}

	p := &Pruner{Registry: &Client{Endpoint: gate.URL, Token: "prune-secret"}, Host: host, Refs: refs, Now: later}
	rep, err := p.Run(context.Background())
	if err != nil || len(rep.Deleted) != 1 || rep.Failed != 0 {
		t.Fatalf("run = %+v, %v; want one deletion", rep, err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := "DELETE /v2/e2e/probe/manifests/sha256:" + hex + " auth="
	var deletes []string
	for _, u := range upstream {
		if strings.HasPrefix(u, "DELETE") {
			deletes = append(deletes, u)
		}
	}
	if len(deletes) != 1 || deletes[0] != want {
		t.Fatalf("upstream deletes = %q, want [%q]", deletes, want)
	}
}
