package api

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
)

const (
	defaultLanguagePath = "/api/v1/settings/default-language"
	panelLanguagePath   = "/api/v1/panel/language"
)

func languageBody(t *testing.T, w *httptest.ResponseRecorder) (language, revision string) {
	t.Helper()
	var v struct {
		Language string `json:"language"`
		Revision string `json:"revision"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	return v.Language, v.Revision
}

func TestDefaultLanguageSaveAndPublicRead(t *testing.T) {
	repo := newFakeRepo()
	cl := newFakeCluster()
	owner := newTestAPI(repo, cl)
	owner.External = staticExternal{p: &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true}}
	visitor := newTestAPI(repo, cl)
	visitor.External = staticExternal{}
	put := func(language, revision string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"language": language, "revision": revision})
		return do(owner.ExternalHandler(), "PUT", defaultLanguagePath, string(body), jsonHeader)
	}
	publicLanguage := func() string {
		w := do(visitor.ExternalHandler(), "GET", panelLanguagePath, "", nil)
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
		language, _ := languageBody(t, w)
		return language
	}

	language, revision := languageBody(t, do(owner.ExternalHandler(), "GET", defaultLanguagePath, "", nil))
	if language != "" || publicLanguage() != "" {
		t.Fatalf("unset default = %q, want follow browser", language)
	}
	if w := put("fr", revision); w.Code != 400 || decodeErr(t, w) != "bad_request" {
		t.Fatalf("unsupported language: %d %s", w.Code, w.Body.String())
	}
	if got := publicLanguage(); got != "" {
		t.Fatalf("rejected value was stored: %q", got)
	}
	if w := put("en-US", revision); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if w := put("zh-CN", revision); w.Code != 409 || decodeErr(t, w) != "conflict" {
		t.Fatalf("stale revision: %d %s", w.Code, w.Body.String())
	}
	if got := publicLanguage(); got != "en-US" {
		t.Fatalf("public language = %q, want en-US", got)
	}
	last := repo.audits[len(repo.audits)-1]
	if last.Action != "platform.default_language" || last.ActorUserID != "owner" || string(last.Payload) != `{"from":"","to":"en-US"}` {
		t.Fatalf("audit = %+v", last)
	}
}

func TestDefaultLanguageRequiresOwner(t *testing.T) {
	repo := newFakeRepo()
	a := newTestAPI(repo, newFakeCluster())
	a.External = staticExternal{p: &Principal{UserID: "admin", Role: "admin", ViaAdminAccess: true}}
	if w := do(a.ExternalHandler(), "PUT", defaultLanguagePath, `{"language":"en-US","revision":"x"}`, jsonHeader); w.Code != 403 {
		t.Fatalf("admin PUT = %d", w.Code)
	}
	if _, err := repo.GetSetting(t.Context(), defaultLanguageKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("setting written by a non-owner: %v", err)
	}
}

func TestPanelLanguageFallsBackToEmptyOnStoreFailure(t *testing.T) {
	repo := newFakeRepo()
	a := newTestAPI(repo, newFakeCluster())
	a.External = staticExternal{}
	read := func() string {
		language, _ := languageBody(t, do(a.ExternalHandler(), "GET", panelLanguagePath, "", nil))
		return language
	}
	repo.failGetSetting = errors.New("database down")
	if got := read(); got != "" {
		t.Fatalf("store outage returned %q", got)
	}
	repo.failGetSetting = nil
	repo.settings[defaultLanguageKey] = []byte(`{"language":"fr"}`)
	if got := read(); got != "" {
		t.Fatalf("invalid stored value returned %q", got)
	}
}
