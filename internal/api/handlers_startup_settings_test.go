package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

func newStartupSettingsAPI() (*API, *fakeRepo, *fakeCluster) {
	a, repo, cl, _ := newPatchAPI()
	repo.byName["survival"].OwnerID = "owner1"
	cl.startupSpecs["survival"] = v1alpha1.MinecraftServerSpec{
		JavaFlags: []string{"-Dprivate=startup-private-flags"},
		Args:      []string{"startup-private-argument", "", "  unchanged  "},
		Env: []v1alpha1.EnvVar{
			{Name: "FELIS_SERVICE_TOKEN", Value: "fake-managed-secret"},
			{Name: "RCON_PASSWORD", Value: "fake-rcon-secret"},
			{Name: "PLUGIN_EMPTY", Value: ""},
			{Name: "PLUGIN_MODE", Value: "startup-private-environment"},
		},
		Lifecycle: v1alpha1.LifecycleSpec{StopCommands: []string{"say startup-private-command"}},
	}
	return a, repo, cl
}

func TestStartupSettingsPrivateRead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal *Principal
		server    string
		unowned   bool
		status    int
	}{
		{"owner", &Principal{UserID: "owner1", Role: "user"}, "survival", false, http.StatusOK},
		{"administrator", &Principal{UserID: "staff", Role: "admin", ViaAdminAccess: true}, "survival", false, http.StatusOK},
		{"system administrator", &Principal{UserID: "staff", Role: "admin", ViaAdminAccess: true}, "login", false, http.StatusOK},
		{"other player", &Principal{UserID: "other", Role: "user"}, "survival", false, http.StatusForbidden},
		{"unowned server", &Principal{UserID: "owner1", Role: "user"}, "survival", true, http.StatusForbidden},
		{"anonymous", nil, "survival", false, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, cl := newStartupSettingsAPI()
			a.External = staticExternal{p: tc.principal}
			if tc.unowned {
				repo.byName["survival"].OwnerID = ""
			}
			if tc.server == "login" {
				cl.byName["login"] = &ServerInfo{Name: "login"}
				cl.startupSpecs["login"] = cl.startupSpecs["survival"]
			}
			cl.noWorld[tc.server] = true
			w := do(a.ExternalHandler(), "GET", "/api/v1/servers/"+tc.server+"/startup-settings", "", nil)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.status != http.StatusOK {
				if len(cl.startupReads) != 0 || strings.Contains(w.Body.String(), "startup-private") {
					t.Fatal("unauthorized request read or disclosed startup settings")
				}
				return
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("private settings may be stored by HTTP caches")
			}
			var got ServerStartupSettings
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := ServerStartupSettings{
				JavaFlags: []string{"-Dprivate=startup-private-flags"},
				Args:      []string{"startup-private-argument", "", "  unchanged  "},
				Env: []StartupEnvVar{
					{Name: "PLUGIN_EMPTY", Value: ""},
					{Name: "PLUGIN_MODE", Value: "startup-private-environment"},
				},
				StopCommands: []string{"say startup-private-command"},
			}
			if !reflect.DeepEqual(got, want) || strings.Contains(w.Body.String(), "fake-managed-secret") || strings.Contains(w.Body.String(), "fake-rcon-secret") {
				t.Fatal("private read lost literal values or exposed managed environment")
			}
			var wire struct {
				Env []map[string]json.RawMessage `json:"env"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			if len(wire.Env) != 2 || string(wire.Env[0]["value"]) != `""` || len(wire.Env[0]) != 2 {
				t.Fatal("an empty environment value was omitted from the wire")
			}
			if !reflect.DeepEqual(cl.startupReads, []string{tc.server}) || len(cl.ensured) != 0 || len(cl.acquired) != 0 || len(repo.audits) != 0 {
				t.Fatal("private read performed a write or unexpected audit")
			}
		})
	}
}

func TestStartupSettingsEmptyArraysAndLookupErrors(t *testing.T) {
	a, _, cl := newStartupSettingsAPI()
	cl.startupSpecs["survival"] = v1alpha1.MinecraftServerSpec{}
	w := do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/startup-settings", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"java_flags", "args", "env", "stop_commands"} {
		if string(wire[field]) != "[]" {
			t.Errorf("%s=%s, want []", field, wire[field])
		}
	}
	if len(wire) != 4 {
		t.Fatal("private response included unexpected fields")
	}
	for _, tc := range []struct {
		server string
		status int
	}{
		{"ghost", http.StatusNotFound},
		{"Bad_Name", http.StatusBadRequest},
	} {
		w := do(a.ExternalHandler(), "GET", "/api/v1/servers/"+tc.server+"/startup-settings", "", nil)
		if w.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.server, w.Code, w.Body.String())
		}
	}
}

func TestStartupSettingsPatchRoundTrip(t *testing.T) {
	for _, mode := range []string{"owner", "administrator", "system administrator"} {
		t.Run(mode, func(t *testing.T) {
			a, repo, cl := newStartupSettingsAPI()
			name, path := "survival", "/api/v1/servers/survival"
			if mode == "owner" {
				a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
				path = "/api/v1/me/servers/survival"
			}
			if mode == "system administrator" {
				name, path = "login", "/api/v1/servers/login"
				cl.byName[name] = &ServerInfo{Name: name}
				cl.startupSpecs[name] = cl.startupSpecs["survival"]
			}
			body := `{"java_flags":["-XX:+UseG1GC","-Dprivate=startup-private-new"],"args":["--motd","hello world","","line one\nline two",null],"env":[{"name":"PLUGIN_MODE","value":"  startup-private-new  "},{"name":"PLUGIN_EMPTY","value":""}],"stop_commands":[" /say startup-private-new ","stop"]}`
			w := do(a.ExternalHandler(), "PATCH", path, body, jsonHeader)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var response struct {
				Patched []string `json:"patched"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(response.Patched, []string{"java_flags", "args", "env", "stop_commands"}) || strings.Contains(w.Body.String(), "startup-private") {
				t.Fatal("patch result omitted a changed field or returned its private values")
			}
			got := cl.startupSpecs[name]
			if !reflect.DeepEqual(got.JavaFlags, []string{"-XX:+UseG1GC", "-Dprivate=startup-private-new"}) ||
				!reflect.DeepEqual(got.Args, []string{"--motd", "hello world", "", "line one\nline two", ""}) ||
				!reflect.DeepEqual(got.Lifecycle.StopCommands, []string{"say startup-private-new", "stop"}) {
				t.Fatal("patch changed argument boundaries or stop-command order")
			}
			wantEnv := []v1alpha1.EnvVar{
				{Name: "FELIS_SERVICE_TOKEN", Value: "fake-managed-secret"},
				{Name: "RCON_PASSWORD", Value: "fake-rcon-secret"},
				{Name: "PLUGIN_MODE", Value: "  startup-private-new  "},
				{Name: "PLUGIN_EMPTY", Value: ""},
			}
			if !reflect.DeepEqual(got.Env, wantEnv) {
				t.Fatal("patch discarded managed variables or trimmed user values")
			}
			w = do(a.ExternalHandler(), "GET", "/api/v1/servers/"+name+"/startup-settings", "", nil)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "startup-private-new") || strings.Contains(w.Body.String(), "fake-managed-secret") {
				t.Fatal("saved settings did not round-trip through the private endpoint")
			}
			audits, err := json.Marshal(repo.audits)
			if err != nil {
				t.Fatal(err)
			}
			if len(repo.audits) != 1 || repo.audits[0].Action != "server.patch" || strings.Contains(string(audits), "startup-private") || strings.Contains(string(audits), "fake-managed-secret") {
				t.Fatal("audit included startup values or failed to record the patch")
			}
		})
	}
}

func TestStartupSettingsPatchOmittedNullAndEmpty(t *testing.T) {
	for _, owner := range []bool{false, true} {
		for _, mode := range []string{"omitted", "null", "clear args", "clear all", "only null"} {
			t.Run(map[bool]string{false: "administrator/", true: "owner/"}[owner]+mode, func(t *testing.T) {
				a, repo, cl := newStartupSettingsAPI()
				path := "/api/v1/servers/survival"
				if owner {
					a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
					path = "/api/v1/me/servers/survival"
				}
				before := cl.startupSpecs["survival"]
				body := `{"displayName":"Renamed"}`
				switch mode {
				case "null":
					body = `{"displayName":"Renamed","java_flags":null,"args":null,"env":null,"stop_commands":null}`
				case "clear args":
					body = `{"args":[]}`
				case "clear all":
					body = `{"java_flags":[],"args":[],"env":[],"stop_commands":[]}`
				case "only null":
					body = `{"java_flags":null,"args":null,"env":null,"stop_commands":null}`
				}
				w := do(a.ExternalHandler(), "PATCH", path, body, jsonHeader)
				if mode == "only null" {
					if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" || len(cl.patched) != 0 || len(repo.audits) != 0 {
						t.Fatal("an all-null patch was accepted as a change")
					}
					return
				}
				if w.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				want := before.DeepCopy()
				if mode == "clear args" || mode == "clear all" {
					want.Args = nil
				}
				if mode == "clear all" {
					want.JavaFlags, want.Lifecycle.StopCommands = nil, nil
					want.Env = before.Env[:2]
				}
				if !reflect.DeepEqual(cl.startupSpecs["survival"], *want) {
					t.Fatal("omitted settings changed or an explicit empty array was ignored")
				}
			})
		}
	}
}

func TestStartupSettingsPatchRejectsAtomically(t *testing.T) {
	for _, owner := range []bool{false, true} {
		for _, tc := range []struct {
			name, fields, code string
		}{
			{"heap flag", `"java_flags":["-Xmx2G"]`, "bad_startup_settings"},
			{"reserved env", `"env":[{"name":"FELIS_SERVICE_TOKEN","value":"startup-private-rejected"}]`, "bad_startup_settings"},
			{"duplicate env", `"env":[{"name":"PLUGIN_MODE","value":"one"},{"name":"PLUGIN_MODE","value":"two"}]`, "bad_startup_settings"},
			{"long argument", `"args":["` + strings.Repeat("x", 2049) + `"]`, "bad_startup_settings"},
			{"multiple commands", `"stop_commands":["say one\nop two"]`, "bad_startup_settings"},
			{"flags item type", `"java_flags":[7]`, "bad_request"},
			{"args type", `"args":"one"`, "bad_request"},
			{"env value type", `"env":[{"name":"PLUGIN_MODE","value":true}]`, "bad_request"},
			{"stop command type", `"stop_commands":[false]`, "bad_request"},
			{"secret reference", `"env":[{"name":"PLUGIN_MODE","valueFrom":{"secretKeyRef":{"name":"fake"}}}]`, "bad_request"},
			{"unknown field", `"javaFlags":["-ea"]`, "bad_request"},
		} {
			t.Run(map[bool]string{false: "administrator/", true: "owner/"}[owner]+tc.name, func(t *testing.T) {
				a, repo, cl := newStartupSettingsAPI()
				path := "/api/v1/servers/survival"
				if owner {
					a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
					path = "/api/v1/me/servers/survival"
				}
				spec := cl.startupSpecs["survival"]
				before := spec.DeepCopy()
				w := do(a.ExternalHandler(), "PATCH", path, `{"displayName":"must not persist",`+tc.fields+`}`, jsonHeader)
				if w.Code != http.StatusBadRequest || decodeErr(t, w) != tc.code {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if !reflect.DeepEqual(cl.startupSpecs["survival"], *before) || cl.byName["survival"].DisplayName != "" || len(cl.patched) != 0 || len(repo.audits) != 0 || strings.Contains(w.Body.String(), "startup-private") {
					t.Fatal("a rejected mixed patch persisted fields, wrote an audit or exposed values")
				}
			})
		}
	}
}

func TestStartupSettingsPatchAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal *Principal
		path      string
		status    int
	}{
		{"another owner", &Principal{UserID: "other", Role: "user"}, "/api/v1/me/servers/survival", http.StatusForbidden},
		{"owner on admin route", &Principal{UserID: "owner1", Role: "user"}, "/api/v1/servers/survival", http.StatusForbidden},
		{"anonymous", nil, "/api/v1/me/servers/survival", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, cl := newStartupSettingsAPI()
			a.External = staticExternal{p: tc.principal}
			spec := cl.startupSpecs["survival"]
			before := spec.DeepCopy()
			w := do(a.ExternalHandler(), "PATCH", tc.path, `{"args":[]}`, jsonHeader)
			if w.Code != tc.status || len(cl.patched) != 0 || len(repo.audits) != 0 || !reflect.DeepEqual(cl.startupSpecs["survival"], *before) {
				t.Fatalf("unauthorized patch: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestStartupSettingsPatchEmptyEnvironmentValues(t *testing.T) {
	a, _, cl := newStartupSettingsAPI()
	w := do(a.ExternalHandler(), "PATCH", "/api/v1/servers/survival", `{"env":[{"name":"PLUGIN_EMPTY"},{"name":"PLUGIN_NULL","value":null}]}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got := userStartupEnv(cl.startupSpecs["survival"].Env)
	if !reflect.DeepEqual(got, []v1alpha1.EnvVar{{Name: "PLUGIN_EMPTY"}, {Name: "PLUGIN_NULL"}}) {
		t.Fatal("omitted or null environment values were not preserved as empty strings")
	}
}

func TestStartupSettingsStayOutOfStatusAndLists(t *testing.T) {
	a, repo, cl := newStartupSettingsAPI()
	cl.list = []ServerInfo{*cl.byName["survival"]}
	repo.mine["owner1"] = []MyServerView{{Name: "survival", Owned: true}}
	for _, tc := range []struct {
		path      string
		principal *Principal
		internal  bool
	}{
		{"/api/v1/servers/survival/status", &Principal{UserID: "other", Role: "user"}, false},
		{"/api/v1/servers/survival/status", &Principal{UserID: "owner1", Role: "user"}, false},
		{"/api/v1/me/servers", &Principal{UserID: "owner1", Role: "user"}, false},
		{"/api/v1/fleet", &Principal{UserID: "staff", Role: "admin", ViaAdminAccess: true}, false},
		{"/api/v1/servers", nil, true},
	} {
		a.External = staticExternal{p: tc.principal}
		h := a.ExternalHandler()
		if tc.internal {
			h = a.InternalHandler()
		}
		w := do(h, "GET", tc.path, "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", tc.path, w.Code, w.Body.String())
		}
		for _, private := range []string{"startup-private", "fake-managed-secret", "fake-rcon-secret", `"java_flags"`, `"args"`, `"env"`, `"stop_commands"`} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatalf("%s exposed a private startup field", tc.path)
			}
		}
	}
	if len(cl.startupReads) != 0 {
		t.Fatal("an existing public view fetched private startup settings")
	}
}
