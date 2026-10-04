package updater

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const installerTestSHA = "0123456789abcdef0123456789abcdef01234567"

func TestInstallerResolvesMovingRefsOnceAndPinsTheScript(t *testing.T) {
	for _, tc := range []struct{ name, release, ref, releasePath, commitRef string }{
		{"latest release", "", "", "releases/latest", "v0.2.0"},
		{"named release", "v0.2.0", "", "releases/tags/v0.2.0", "v0.2.0"},
		{"main", "", "main", "", "main"},
		{"branch with slash", "", "feature/example", "", "feature/example"},
		{"exact commit", "", installerTestSHA, "", installerTestSHA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			const script = "#!/bin/bash\n# FELIS_RELEASE\necho example\n"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer private-token" || r.Header.Get("User-Agent") == "" {
					t.Errorf("unexpected request headers/method: %v", r)
				}
				calls = append(calls, r.URL.Path)
				switch r.URL.Path {
				case "/repos/example/Felis/" + tc.releasePath:
					fmt.Fprint(w, `{"tag_name":"v0.2.0"}`)
				case "/repos/example/Felis/commits/" + tc.commitRef:
					fmt.Fprintf(w, `{"sha":%q}`, installerTestSHA)
				case "/repos/example/Felis/contents/deploy/bootstrap.sh":
					if r.URL.Query().Get("ref") != installerTestSHA || r.Header.Get("Accept") != "application/vnd.github.raw+json" {
						t.Errorf("script not pinned/raw: %v", r)
					}
					fmt.Fprint(w, script)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			gh := newTestGitHub(srv)
			gh.token = "private-token"
			target, err := (InstallerSource{github: gh, repo: "example/Felis"}).Prepare(context.Background(), tc.release, tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			wantRelease := ""
			wantCalls := []string{}
			if tc.releasePath != "" {
				wantRelease = "v0.2.0"
				wantCalls = append(wantCalls, "/repos/example/Felis/"+tc.releasePath)
			}
			wantCalls = append(wantCalls, "/repos/example/Felis/commits/"+tc.commitRef, "/repos/example/Felis/contents/deploy/bootstrap.sh")
			if target.Release != wantRelease || target.Revision != installerTestSHA || target.Script != script || !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("target %+v, calls %v; want %v", target, calls, wantCalls)
			}
		})
	}
}

func TestInstallerFailsClosedBeforeExecutingUnusableTargets(t *testing.T) {
	for _, tc := range []struct {
		name, releaseBody, commitBody, script string
		status                                int
	}{
		{"prerelease", `{"tag_name":"v0.2.0","prerelease":true}`, "", "", 200},
		{"invalid tag", `{"tag_name":"latest"}`, "", "", 200},
		{"short commit", `{"tag_name":"v0.2.0"}`, `{"sha":"abcdef0"}`, "", 200},
		{"invalid commit", `{"tag_name":"v0.2.0"}`, `{"sha":"zz23456789abcdef0123456789abcdef01234567"}`, "", 200},
		{"non-script response", `{"tag_name":"v0.2.0"}`, fmt.Sprintf(`{"sha":%q}`, installerTestSHA), "<html>unavailable</html>", 200},
		{"oversized script", `{"tag_name":"v0.2.0"}`, fmt.Sprintf(`{"sha":%q}`, installerTestSHA), "#!/bin/bash\n" + strings.Repeat("#", 2<<20), 200},
		{"unavailable", "", "", "", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				switch {
				case strings.Contains(r.URL.Path, "/releases/"):
					fmt.Fprint(w, tc.releaseBody)
				case strings.Contains(r.URL.Path, "/commits/"):
					fmt.Fprint(w, tc.commitBody)
				default:
					fmt.Fprint(w, tc.script)
				}
			}))
			defer srv.Close()
			got, err := (InstallerSource{github: newTestGitHub(srv), repo: officialRepo}).Prepare(context.Background(), "", "")
			if err == nil || got != (InstallTarget{}) {
				t.Fatalf("unsafe target %+v, err %v", got, err)
			}
		})
	}
}

func TestInstallerAcceptsGitHubReposWithoutEmbeddingCredentials(t *testing.T) {
	for _, repoURL := range []string{"", "https://github.com/FelisMC/Felis.git", "git@github.com:FelisMC/Felis.git", "ssh://git@github.com/FelisMC/Felis.git"} {
		s, err := NewInstallerSource(repoURL)
		if err != nil || s.RepoURL() != "https://github.com/FelisMC/Felis.git" {
			t.Errorf("%s: %v, %+v", repoURL, err, s)
		}
	}
	for _, repoURL := range []string{"https://example.com/FelisMC/Felis", "http://github.com/FelisMC/Felis", "https://github.com/FelisMC/Felis?token=secret", "git@github.com:../../bad"} {
		if _, err := NewInstallerSource(repoURL); err == nil {
			t.Errorf("accepted %s", repoURL)
		}
	}
}

func TestInstallerRefusesACommitDifferentFromTheRequestedSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/contents/") {
			t.Error("mismatched commit must not reach installer download")
		}
		fmt.Fprintf(w, `{"sha":%q}`, strings.Repeat("a", 40))
	}))
	defer srv.Close()
	target, err := (InstallerSource{github: newTestGitHub(srv), repo: officialRepo}).Prepare(context.Background(), "", installerTestSHA)
	if err == nil || target != (InstallTarget{}) {
		t.Fatalf("accepted a different commit: %+v / %v", target, err)
	}
}
