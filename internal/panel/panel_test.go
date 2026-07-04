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
