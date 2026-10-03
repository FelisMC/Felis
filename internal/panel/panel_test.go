package panel

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandlerServesPanelAndConfig(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me" {
			t.Fatalf("api saw unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusTeapot)
	})
	h := Handler(api, "example.test", "console.example.test", "op.console.example.test", 0, "26.3", "v1.2.3")

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
	if cfg.APIBase != "/api/v1" || cfg.RootDomain != "example.test" || cfg.GameVersion != "26.3" {
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
// The SPA appends the public game port to the addresses players copy, so
// /config.json carries it, except when a bare hostname already reaches the proxy.
func TestHandlerPublishesNonDefaultGamePort(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 0}, {25565, 0}, {25570, 25570}} {
		h := Handler(http.NotFoundHandler(), "example.test", "", "", tc.in, "", "v1.2.3")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/config.json", nil))
		var cfg map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
			t.Fatalf("decode config: %v", err)
		}
		got, present := cfg["gamePort"]
		switch {
		case tc.want == 0 && present:
			t.Errorf("game port %d: gamePort = %v, want absent", tc.in, got)
		case tc.want != 0 && got != float64(tc.want):
			t.Errorf("game port %d: gamePort = %v, want %d", tc.in, got, tc.want)
		}
	}
}

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

// The console's pages go out under a CSP that admits the inline theme script by
// its hash (and nothing else inline), cannot be framed, and cache by name:
// hashed assets forever, the page itself never without revalidation.
func TestHandlerSetsPageSecurityAndCacheHeaders(t *testing.T) {
	h := Handler(http.NotFoundHandler(), "example.test", "", "", 0, "26.3", "v1.2.3")

	w := httptest.NewRecorder()
	// Through the tunnel: TLS to the origin as well, the edge's scheme in XFP.
	r := httptest.NewRequest(http.MethodGet, "https://console.example.test/servers/survival", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	h.ServeHTTP(w, r)
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "'unsafe-inline'") && !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP %q allows inline outside styles", csp)
	}
	for k, want := range map[string]string{
		"X-Frame-Options":           "DENY",
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "no-referrer",
		"Strict-Transport-Security": "max-age=31536000",
		"Cache-Control":             "no-cache",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// Straight to the self-signed listener: no HSTS.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "https://10.0.0.7:30443/", nil)
	h.ServeHTTP(w, r)
	if got := w.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS on the self-signed listener: %q", got)
	}
}

func TestContentSecurityPolicyHashesInlineScripts(t *testing.T) {
	script := "\n      document.documentElement.classList.add(\"dark\");\n    "
	files := fstest.MapFS{"index.html": {Data: []byte(
		"<html><head><script>" + script + "</script>" +
			`<script type="module" src="/assets/index.js"></script></head></html>`)}}
	sum := sha256.Sum256([]byte(script))
	want := "script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "';"
	if csp := contentSecurityPolicy(files); !strings.Contains(csp, want) {
		t.Fatalf("CSP %q, want %q", csp, want)
	}
	if csp := contentSecurityPolicy(fstest.MapFS{}); !strings.Contains(csp, "script-src 'self';") {
		t.Fatalf("CSP without an index = %q", csp)
	}
}
