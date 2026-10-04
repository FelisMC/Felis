package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"felis.lolicon.best/internal/config"
)

const authSourcesPath = "/api/v1/settings/auth-sources"

func sourceEntry(tag, prefix, endpoint string) authSourceEntry {
	return authSourceEntry{AuthSourceConfig: config.AuthSourceConfig{Tag: tag, Prefix: prefix, URL: endpoint}, Enabled: true}
}

func seedAuthSourcesAPI() (*API, *fakeRepo) {
	repo := newFakeRepo()
	a := newTestAPI(repo, newFakeCluster())
	a.External = staticExternal{p: &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true}}
	a.AuthSources = []AuthSource{{Tag: "mojang", URL: "https://sessionserver.mojang.com/session/minecraft/hasJoined", Identity: true}, {Tag: "littleskin", Prefix: "LS", URL: "https://littleskin.cn/api/yggdrasil/sessionserver/session/minecraft/hasJoined"}}
	a.AuthSourceSettings = &AuthSourceSettings{Repo: repo, Defaults: a.AuthSources}
	return a, repo
}

func readAuthSources(t *testing.T, a *API) authSourcesView {
	t.Helper()
	w := do(a.ExternalHandler(), "GET", authSourcesPath, "", nil)
	if w.Code != 200 {
		t.Fatalf("read = %d %s", w.Code, w.Body.String())
	}
	var view authSourcesView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func saveAuthSources(a *API, view authSourcesView) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"sources": view.Sources, "revision": view.Revision})
	return do(a.ExternalHandler(), "PUT", authSourcesPath, string(body), jsonHeader)
}

func TestAuthSourcesSaveAndRuntime(t *testing.T) {
	const native = "123456781234423482341234567890ab"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/official" {
			w.WriteHeader(204)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/profile/"+native) {
			_, _ = w.Write([]byte(`{"id":"` + native + `","name":"LemonMiaow"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + native + `","name":"LemonMiaow"}`))
	}))
	defer upstream.Close()
	a, repo := seedAuthSourcesAPI()
	a.AuthSources[0].URL = upstream.URL + "/official"
	a.AuthSources[1].URL = upstream.URL + "/sessionserver/session/minecraft/hasJoined"
	before := readAuthSources(t, a)
	if before.Managed || len(before.Sources) != 1 || before.Sources[0].Tag != "littleskin" {
		t.Fatalf("defaults = %+v", before)
	}
	before.Sources = append(before.Sources, sourceEntry("other", "OT", upstream.URL+"/other-check"))
	before.Sources[1].APIURL = upstream.URL
	before.Sources[0], before.Sources[1] = before.Sources[1], before.Sources[0]
	w := saveAuthSources(a, before)
	if w.Code != 200 {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	saved := readAuthSources(t, a)
	if !saved.Managed || saved.Revision == before.Revision || saved.Sources[0].Tag != "other" {
		t.Fatalf("saved = %+v", saved)
	}
	// Another API replica sees the same order and identity policy without restart.
	replica := newTestAPI(repo, newFakeCluster())
	replica.AuthSourceSettings = &AuthSourceSettings{Repo: repo, Defaults: a.AuthSources}
	sources, err := replica.currentAuthSources(context.Background())
	if err != nil || len(sources) != 3 || !sources[0].Identity || sources[1].Identity || sources[1].Tag != "other" {
		t.Fatalf("active = %+v err = %v", sources, err)
	}
	profile, src, failed := replica.resolveHasJoined(context.Background(), "LemonMiaow", "joined-session", "")
	if profile == nil || failed || src.Tag != "other" {
		t.Fatalf("login = %+v src=%+v failed=%v", profile, src, failed)
	}
	preview, err := a.lookupProfile(context.Background(), "other", native)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := canonicalProfileUUID(src, native)
	if preview.MCUUID != canonical.String() {
		t.Fatal("role lookup and game identity disagree")
	}
	// Disabled sources disappear from login priority and role lookup immediately.
	saved.Sources[0].Enabled = false
	if w = saveAuthSources(a, saved); w.Code != 200 {
		t.Fatalf("disable = %d %s", w.Code, w.Body.String())
	}
	_, src, _ = replica.resolveHasJoined(context.Background(), "LemonMiaow", "joined-session", "")
	if src.Tag != "littleskin" {
		t.Fatalf("disabled source still used: %+v", src)
	}
	if _, err = a.lookupProfile(context.Background(), "other", native); err == nil {
		t.Fatal("disabled source remains selectable")
	}
	list := do(a.ExternalHandler(), "GET", "/api/v1/account/link/sources", "", nil)
	if list.Code != 200 || strings.Contains(list.Body.String(), `"other"`) || strings.Contains(list.Body.String(), upstream.URL) {
		t.Fatalf("public source list = %d %s", list.Code, list.Body.String())
	}
	// Durable failure must never resurrect the installation defaults.
	repo.failGetSetting = errors.New("database unavailable")
	profile, _, failed = replica.resolveHasJoined(context.Background(), "LemonMiaow", "joined-session", "")
	if profile != nil || !failed {
		t.Fatal("DB outage fell back to obsolete configuration")
	}
	if _, err = replica.currentAuthSources(context.Background()); err == nil {
		t.Fatal("settings outage ignored")
	}
}

func TestAuthSourcesProtectNamespacesAndRevision(t *testing.T) {
	a, repo := seedAuthSourcesAPI()
	initial := readAuthSources(t, a)
	for _, rename := range []bool{false, true} {
		v := readAuthSources(t, a)
		if rename {
			v.Sources[0].Tag = "renamed"
		} else {
			v.Sources = []authSourceEntry{}
		}
		w := saveAuthSources(a, v)
		if w.Code != 409 || decodeErr(t, w) != "auth_source_tag_locked" {
			t.Fatalf("namespace change = %d %s", w.Code, w.Body.String())
		}
	}
	initial.Sources[0].Enabled = false
	if w := saveAuthSources(a, initial); w.Code != 200 {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	original := string(repo.settings[authSourcesKey])
	initial.Sources[0].Prefix = "XX"
	if w := saveAuthSources(a, initial); w.Code != 409 || decodeErr(t, w) != "auth_sources_changed" {
		t.Fatalf("stale save = %d %s", w.Code, w.Body.String())
	}
	if string(repo.settings[authSourcesKey]) != original {
		t.Fatal("stale save overwrote durable config")
	}
	// Invalid stored data also fails closed.
	repo.settings[authSourcesKey] = []byte(`[{"tag":"mojang","prefix":"M","url":"https://example.test/check","enabled":true}]`)
	if _, err := a.currentAuthSources(context.Background()); err == nil {
		t.Fatal("corrupt DB config trusted as Mojang")
	}
}

type conflictSettingsRepo struct{ *fakeRepo }

func (r conflictSettingsRepo) CompareAndSetSetting(context.Context, string, []byte, []byte) error {
	return ErrConflict
}

func TestAuthSourcesConcurrentWrite(t *testing.T) {
	a, repo := seedAuthSourcesAPI()
	v := readAuthSources(t, a)
	a.AuthSourceSettings.Repo = conflictSettingsRepo{repo}
	v.Sources[0].Enabled = false
	w := saveAuthSources(a, v)
	if w.Code != 409 || decodeErr(t, w) != "auth_sources_changed" || len(repo.settings) != 0 {
		t.Fatalf("concurrent write = %d %s", w.Code, w.Body.String())
	}
}

func TestAuthSourcesValidation(t *testing.T) {
	valid := sourceEntry("custom", "CS", "https://example.test/check")
	for _, tc := range []struct {
		name    string
		entries []authSourceEntry
	}{
		{"null", nil},
		{"Mojang", []authSourceEntry{sourceEntry("MOJANG", "M", valid.URL)}},
		{"empty tag", []authSourceEntry{sourceEntry("", "M", valid.URL)}},
		{"colon", []authSourceEntry{sourceEntry("x:y", "M", valid.URL)}},
		{"duplicate tag", []authSourceEntry{valid, valid}},
		{"duplicate prefix", []authSourceEntry{valid, sourceEntry("other", "cs", valid.URL)}},
		{"long prefix", []authSourceEntry{sourceEntry("x", "ABCDE", valid.URL)}},
		{"public HTTP", []authSourceEntry{sourceEntry("x", "X", "http://example.test/check")}},
		{"query", []authSourceEntry{sourceEntry("x", "X", valid.URL+"?secret=1")}},
		{"fragment", []authSourceEntry{sourceEntry("x", "X", valid.URL+"#fragment")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo := seedAuthSourcesAPI()
			v := readAuthSources(t, a)
			v.Sources = tc.entries
			w := saveAuthSources(a, v)
			if w.Code != 400 || len(repo.settings) != 0 {
				t.Fatalf("validation = %d %s", w.Code, w.Body.String())
			}
		})
	}
	a, _ := seedAuthSourcesAPI()
	w := do(a.ExternalHandler(), "PUT", authSourcesPath, `{"sources":[],"revision":"x","identity":true}`, jsonHeader)
	if w.Code != 400 {
		t.Fatal("caller could set trust policy")
	}
	w = do(a.ExternalHandler(), "PUT", authSourcesPath, `{}`, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if w.Code != 415 {
		t.Fatal("cross-site form accepted")
	}
}

func TestAuthSourcesOwnerGateAndReauth(t *testing.T) {
	for _, p := range []*Principal{nil, {UserID: "admin", Role: "admin", ViaAdminAccess: true}, {UserID: "user", Role: "user", ViaAdminAccess: true}, {UserID: "owner", Role: "owner"}} {
		a, repo := seedAuthSourcesAPI()
		a.External = staticExternal{p: p}
		for _, route := range []struct{ method, path string }{{"GET", authSourcesPath}, {"PUT", authSourcesPath}, {"POST", authSourcesPath + "/test"}} {
			w := do(a.ExternalHandler(), route.method, route.path, `{}`, jsonHeader)
			if w.Code != 401 && w.Code != 403 {
				t.Fatalf("%+v reached %s: %d", p, route.path, w.Code)
			}
		}
		if len(repo.settings) != 0 {
			t.Fatal("unauthorized write")
		}
	}
	a, repo := seedAuthSourcesAPI()
	p := &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true, ViaSession: true, EmailVerified: true}
	repo.passkeyCreds["owner-key"] = PasskeyCredential{ID: "owner-key", UserID: p.UserID, UserVerified: true}
	a.External = staticExternal{p: p}
	v := readAuthSources(t, a)
	v.Sources[0].Enabled = false
	w := saveAuthSources(a, v)
	if w.Code != 403 || decodeErr(t, w) != "reauth_required" || len(repo.settings) != 0 {
		t.Fatalf("reauth = %d %s", w.Code, w.Body.String())
	}
	p.ReauthAt = a.now()
	if w = saveAuthSources(a, v); w.Code != 200 {
		t.Fatalf("fresh reauth = %d %s", w.Code, w.Body.String())
	}
}

func TestAuthSourceProbe(t *testing.T) {
	for _, status := range []int{204, 200, 302, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ids := []string{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("username") != "FelisProbe" {
					t.Error("missing probe user")
				}
				ids = append(ids, r.URL.Query().Get("serverId"))
				w.Header().Set("Location", "http://127.0.0.1:1/no-redirect")
				w.WriteHeader(status)
			}))
			defer upstream.Close()
			a, repo := seedAuthSourcesAPI()
			body, _ := json.Marshal(sourceEntry("probe", "P", upstream.URL))
			for i := 0; i < 2; i++ {
				w := do(a.ExternalHandler(), "POST", authSourcesPath+"/test", string(body), jsonHeader)
				var result struct {
					OK      bool  `json:"ok"`
					Status  int   `json:"status"`
					Elapsed int64 `json:"elapsed_ms"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if w.Code != 200 || result.OK != (status == 204) || result.Status != status || result.Elapsed < 0 {
					t.Fatalf("probe = %d %s", w.Code, w.Body.String())
				}
			}
			if len(ids) != 2 || ids[0] == "" || ids[0] == ids[1] || !reflect.DeepEqual(repo.settings, map[string][]byte{}) {
				t.Fatal("probe reused identity or saved config")
			}
		})
	}
	a, _ := seedAuthSourcesAPI()
	body, _ := json.Marshal(sourceEntry("probe", "P", "http://127.0.0.1:1/check"))
	w := do(a.ExternalHandler(), "POST", authSourcesPath+"/test", string(body), jsonHeader)
	if w.Code != 503 || decodeErr(t, w) != "auth_source_unavailable" {
		t.Fatalf("unreachable = %d %s", w.Code, w.Body.String())
	}
}
