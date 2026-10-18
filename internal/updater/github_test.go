package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Use captured CalVer and semver releases; reject RC tags and prerelease flags.
const (
	cloudflaredLatestFixture = `{"tag_name":"2026.6.1","prerelease":false,"draft":false,"name":"2026.6.1"}`
	k3sLatestFixture         = `{"tag_name":"v1.36.2+k3s1","prerelease":false,"draft":false,"name":"v1.36.2+k3s1"}`
	// adoptium/temurin25-binaries on 2026-09-25: an emergency respin, four components.
	temurinLatestFixture = `{"tag_name":"jdk-25.0.4.1+1","prerelease":false,"draft":false,"name":"jdk-25.0.4.1+1"}`
)

func TestGitHubLatestTemurin(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(temurinLatestFixture))
	}))
	defer srv.Close()
	v, err := newTestGitHub(srv).latestTemurin(context.Background(), 25)
	if err != nil {
		t.Fatalf("latestTemurin: %v", err)
	}
	if path != "/repos/adoptium/temurin25-binaries/releases/latest" {
		t.Errorf("requested %s, want the temurin25-binaries repository", path)
	}
	if v.String() != "25.0.4.1+1" || v.Revision != 1 {
		t.Errorf("version = %s (%+v), want 25.0.4.1+1 with revision 1", v, v)
	}
}

func newTestGitHub(srv *httptest.Server) github {
	return github{
		baseURL:   srv.URL,
		userAgent: "felis-updater/0.1",
		hc:        srv.Client(),
	}
}

// ghFixtureServer serves a fixed body to any /repos/.../releases/latest path, and — like
// the real API — 403s a request that arrives without a User-Agent.
func ghFixtureServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "Request forbidden by administrative rules", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestGitHubLatestStableParsesRealTags(t *testing.T) {
	cases := []struct {
		name            string
		repo            string
		body            string
		wantMajMinPat   [3]int
		wantRawInReport string
	}{
		{"cloudflared CalVer", "cloudflare/cloudflared", cloudflaredLatestFixture, [3]int{2026, 6, 1}, "2026.6.1"},
		{"k3s v-prefix +build", "k3s-io/k3s", k3sLatestFixture, [3]int{1, 36, 2}, "v1.36.2+k3s1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := ghFixtureServer(tc.body)
			defer srv.Close()

			v, err := newTestGitHub(srv).latestStable(context.Background(), tc.repo)
			if err != nil {
				t.Fatalf("latestStable: %v", err)
			}
			if got := [3]int{v.Major, v.Minor, v.Patch}; got != tc.wantMajMinPat {
				t.Errorf("version core = %v, want %v", got, tc.wantMajMinPat)
			}
			if v.IsPrerelease() {
				t.Errorf("stable release parsed as prerelease: %q", v.String())
			}
			if v.String() != tc.wantRawInReport {
				t.Errorf("String() = %q, want raw upstream tag %q preserved for the report", v.String(), tc.wantRawInReport)
			}
		})
	}
}

func TestGitHubClearsTheUserAgentGate(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		if gotUA == "" {
			http.Error(w, "Request forbidden by administrative rules", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(cloudflaredLatestFixture))
	}))
	defer srv.Close()

	v, err := newTestGitHub(srv).latestStable(context.Background(), "cloudflare/cloudflared")
	if err != nil {
		t.Fatalf("latestStable through the UA gate: %v", err)
	}
	if v.String() != "2026.6.1" {
		t.Errorf("latestStable = %q, want 2026.6.1", v.String())
	}
	if gotUA == "" {
		t.Fatal("no User-Agent sent — the real GitHub API would have refused this request")
	}
}

func TestGitHubFailsClosedOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	if v, err := newTestGitHub(srv).latestStable(context.Background(), "acme/no-releases"); err == nil {
		t.Fatalf("want error on 404, got version %q", v.String())
	}
}

func TestGitHubFailsClosedOnPrereleaseFlag(t *testing.T) {
	const rc = `{"tag_name":"v1.37.0-rc1+k3s1","prerelease":true,"draft":false}`
	srv := ghFixtureServer(rc)
	defer srv.Close()

	if v, err := newTestGitHub(srv).latestStable(context.Background(), "k3s-io/k3s"); err == nil {
		t.Fatalf("want error on a prerelease-flagged release, got %q", v.String())
	}
}

func TestGitHubFailsClosedOnUnparseableTag(t *testing.T) {
	const junk = `{"tag_name":"nightly","prerelease":false,"draft":false}`
	srv := ghFixtureServer(junk)
	defer srv.Close()

	if v, err := newTestGitHub(srv).latestStable(context.Background(), "acme/rolling"); err == nil {
		t.Fatalf("want error on an unparseable tag, got %q", v.String())
	}
}

func TestGitHubSendsTokenOnlyWhenSet(t *testing.T) {
	for _, tc := range []struct {
		name, token, wantAuth string
	}{
		{"a token is sent as a Bearer credential", "ghp_secret", "Bearer ghp_secret"},
		{"no token sends no Authorization header", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				_, _ = w.Write([]byte(`{"tag_name":"v1.2.3","prerelease":false,"draft":false}`))
			}))
			defer srv.Close()

			g := newTestGitHub(srv)
			g.token = tc.token
			if _, err := g.latestStable(context.Background(), "acme/private"); err != nil {
				t.Fatalf("latestStable: %v", err)
			}
			if gotAuth != tc.wantAuth {
				t.Errorf("Authorization = %q, want %q", gotAuth, tc.wantAuth)
			}
		})
	}
}

func TestGitHubNamesTheTokenOnAnUnauthenticated404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	g := newTestGitHub(srv)
	_, err := g.latestStable(context.Background(), "acme/private")
	if err == nil {
		t.Fatal("want an error on 404")
	}
	if !strings.Contains(err.Error(), tokenEnv) {
		t.Errorf("unauthenticated 404 must name %s so an operator knows the fix; got: %v", tokenEnv, err)
	}

	g.token = "ghp_secret"
	_, err = g.latestStable(context.Background(), "acme/private")
	if err == nil {
		t.Fatal("want an error on 404")
	}
	if strings.Contains(err.Error(), tokenEnv) {
		t.Errorf("a 404 WITH a token set must not blame the missing token; got: %v", err)
	}

	// The official repository is public: a 404 there means no stable release, whatever
	// the token.
	g.token = ""
	_, err = g.latestStable(context.Background(), "felismc/felis")
	if err == nil {
		t.Fatal("want an error on 404")
	}
	if strings.Contains(err.Error(), tokenEnv) || strings.Contains(err.Error(), "private") {
		t.Errorf("a 404 from the public official repository must not suggest it is private; got: %v", err)
	}
	if !strings.Contains(err.Error(), "no published stable release") {
		t.Errorf("a 404 from the official repository must say it has no stable release; got: %v", err)
	}
}
