package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"felis.lolicon.best/internal/mailsettings"
)

func TestNanoRoutesAreFelisRoutes(t *testing.T) {
	a := newTestAPI(newFakeRepo(), nil)
	full := map[string]bool{}
	for _, rt := range append(a.internalAPIRoutes(), a.externalAPIRoutes()...) {
		full[rt.Method+" "+rt.Pattern] = true
	}
	for k := range nanoRoutes {
		if !full[k] {
			t.Errorf("nano mounts %s, which no Felis face serves", k)
		}
	}
}

// nanoAPI wires what felis nano wires and leaves every server subsystem nil, as a nano host has none.
func nanoAPI() *API {
	repo := newFakeRepo()
	a := newTestAPI(repo, nil)
	a.Nano = true
	a.External = staticExternal{p: &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true, ViaSession: true, EmailVerified: true}}
	a.AuthSourceSettings = &AuthSourceSettings{Repo: repo}
	a.MailSettings = &MailSettings{Repo: repo, NewMailer: func(s mailsettings.Settings) OTPMailer { return s.SMTP() }}
	return a
}

func TestNanoRoutesNeedNoServerSubsystem(t *testing.T) {
	a := nanoAPI()
	internal := map[string]bool{}
	for _, rt := range a.internalAPIRoutes() {
		internal[rt.Method+" "+rt.Pattern] = true
	}
	faces := map[bool]http.Handler{true: a.InternalHandler(), false: a.ExternalHandler()}
	path := strings.NewReplacer("{mc_uuid}", "069a79f4-44e9-4726-a5be-fca90e38aaf5", "{id}", "x", "{hash}", "h", "{name}", "survival")
	for route := range nanoRoutes {
		method, pattern, _ := strings.Cut(route, " ")
		// Served directly, outside the OpenAPI recorder: these probes look for panics, not for the wire contract.
		r := httptest.NewRequest(method, path.Replace(pattern), strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		faces[internal[route]].ServeHTTP(w, r)
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if body.Error.Code == "panic" {
			t.Errorf("%s reached a subsystem a nano host lacks (500 panic)", route)
		}
	}
}

func TestNanoServesNothingThatTouchesServers(t *testing.T) {
	a := nanoAPI()
	for _, rt := range []struct {
		h            http.Handler
		method, path string
	}{
		{a.ExternalHandler(), "GET", "/api/v1/servers"},
		{a.ExternalHandler(), "GET", "/api/v1/settings/node-control"},
		{a.InternalHandler(), "POST", "/api/v1/internal/servers/survival/wake"},
		{a.InternalHandler(), "GET", "/api/v1/servers"},
	} {
		if w := do(rt.h, rt.method, rt.path, "", nil); w.Code != http.StatusNotFound {
			t.Errorf("nano %s %s = %d, want 404", rt.method, rt.path, w.Code)
		}
	}
}

func TestPanelURLCarriesANonStandardPort(t *testing.T) {
	for _, tc := range []struct {
		port int
		want string
	}{
		{0, "https://console.example.com"},
		{443, "https://console.example.com"},
		{8443, "https://console.example.com:8443"},
	} {
		a := &API{RootDomain: "example.com", PanelPort: tc.port}
		if got := a.panelURL(); got != tc.want {
			t.Errorf("PanelPort %d: panelURL() = %q, want %q", tc.port, got, tc.want)
		}
	}
}
