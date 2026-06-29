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
	h := Handler(api, "example.test")

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

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if w.Code != http.StatusTeapot {
		t.Fatalf("api status = %d", w.Code)
	}
}
