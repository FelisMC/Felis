package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesPanelAndConfig(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me" {
			t.Fatalf("api saw unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusTeapot)
	})
	h := Handler(api, "example.test", "console.example.test", "op.console.example.test", "v1.2.3")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Felis") {
		t.Fatalf("index response = %d %q", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers/survival", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Felis") {
		t.Fatalf("spa fallback = %d %q", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/config.json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("config status = %d", w.Code)
	}
	var cfg runtimeConfig
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.APIBase != "/api/v1" || cfg.RootDomain != "example.test" {
		t.Fatalf("config = %+v", cfg)
	}
	// The tiering plumbing surfaces the two console hostnames so one bundle can
	// detect which face it is being served from, plus a resolved build stamp for
	// the version badge. A clean release tag resolves to a non-dev build.
	if cfg.PanelHostname != "console.example.test" || cfg.AdminHostname != "op.console.example.test" {
		t.Fatalf("config tiering hostnames = %+v", cfg)
	}
	if cfg.Build.Version != "v1.2.3" || cfg.Build.Release != "v1.2.3" || cfg.Build.Dev {
		t.Fatalf("config build stamp = %+v", cfg.Build)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if w.Code != http.StatusTeapot {
		t.Fatalf("api status = %d", w.Code)
	}
}

// TestParseBuildVersionSplitsBothStampForms pins the two shapes a felis binary can
// carry. The "+g<sha>" form is what deploy/bootstrap.sh's dev channel links in, and
// it is the one that regressed: before it was parsed, a dev build fell through to the
// default case and the badge rendered the whole stamp as the release with no commit.
func TestParseBuildVersionSplitsBothStampForms(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		release string
		commit  string
		dev     bool
	}{
		{"v1.2.3+g1a2b3c4", "v1.2.3", "1a2b3c4", true},
		{"v1.0.0-earlyAccess+g1a2b3c4", "v1.0.0-earlyAccess", "1a2b3c4", true},
		{"v1.0.0-earlyAccess-3-g1a2b3c4", "v1.0.0-earlyAccess", "1a2b3c4", true},
		{"v1.2.3", "v1.2.3", "", false},
		{"v1.0.0-earlyAccess", "v1.0.0-earlyAccess", "", false},
		{"v1.2.3+g1a2b3c4-dirty", "v1.2.3", "1a2b3c4", true},
		{"dev", "dev", "", true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got := parseBuildVersion(tc.raw)
			if got.Release != tc.release || got.Commit != tc.commit || got.Dev != tc.dev {
				t.Errorf("parseBuildVersion(%q) = {Release:%q Commit:%q Dev:%v}, want {Release:%q Commit:%q Dev:%v}",
					tc.raw, got.Release, got.Commit, got.Dev, tc.release, tc.commit, tc.dev)
			}
		})
	}
}
