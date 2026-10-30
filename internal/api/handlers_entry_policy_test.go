package api

import (
	"context"
	"encoding/json"
	"testing"
)

func TestEntryPolicyPersistenceAndAuthorization(t *testing.T) {
	repo, cluster := newFakeRepo(), newFakeCluster()
	cluster.byName["main"] = &ServerInfo{Name: "main"}
	a := newTestAPI(repo, cluster)
	path := "/api/v1/settings/entry-policy"
	for _, p := range []*Principal{nil, {UserID: "admin", Role: "admin", ViaAdminAccess: true}, {UserID: "owner", Role: "owner"}} {
		a.External = staticExternal{p: p}
		for _, method := range []string{"GET", "PUT"} {
			if w := do(a.ExternalHandler(), method, path, `{}`, jsonHeader); w.Code != 401 && w.Code != 403 {
				t.Fatalf("unauthorized: %d", w.Code)
			}
		}
	}
	p := &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true}
	a.External = staticExternal{p: p}
	view, _, err := a.readEntryPolicy(context.Background())
	if err != nil || !view.RequireAccountLink || view.Mode != "domain" {
		t.Fatal(view, err)
	}
	save := func(v entryPolicyView) int {
		body, _ := json.Marshal(v)
		return do(a.ExternalHandler(), "PUT", path, string(body), jsonHeader).Code
	}
	view.Mode = "direct"
	view.DefaultServer = "main"
	view.RequireAccountLink = false
	view.WaitingSpace = "login"
	if code := save(view); code != 200 {
		t.Fatal("save", code)
	}
	if code := save(view); code != 409 {
		t.Fatal("stale save", code)
	}
	replica := newTestAPI(repo, cluster)
	persisted, _, err := replica.readEntryPolicy(context.Background())
	if err != nil || persisted.Mode != "direct" || persisted.DefaultServer != "main" || persisted.RequireAccountLink {
		t.Fatal(persisted, err)
	}
	persisted.DefaultServer = "missing"
	if code := save(persisted); code != 404 {
		t.Fatal("missing target", code)
	}
	p.ViaSession = true
	p.EmailVerified = true
	repo.passkeyCreds["key"] = PasskeyCredential{ID: "key", UserID: "owner", UserVerified: true}
	persisted.DefaultServer = "main"
	if code := save(persisted); code != 403 {
		t.Fatal("reauth", code)
	}
	repo.settings[entryPolicyKey] = []byte(`{"mode":"invalid"}`)
	if _, _, err := a.readEntryPolicy(context.Background()); err == nil {
		t.Fatal("invalid persisted policy accepted")
	}
}

func TestEntryPolicyValidation(t *testing.T) {
	good := entryPolicy{Mode: "direct", DefaultServer: "main", OfflineAction: "wake", WaitingSpace: "login"}
	if !good.valid(false) {
		t.Fatal("valid policy rejected")
	}
	for _, mutate := range []func(*entryPolicy){
		func(p *entryPolicy) { p.Mode = "invalid" }, func(p *entryPolicy) { p.DefaultServer = "" }, func(p *entryPolicy) { p.DefaultServer = "login" }, func(p *entryPolicy) { p.OfflineAction = "fallback" }, func(p *entryPolicy) { p.OfflineAction = "fallback"; p.FallbackServer = "main" }, func(p *entryPolicy) { p.WaitingSpace = "invalid" }, func(p *entryPolicy) { p.RequireAccountLink = true },
	} {
		p := good
		mutate(&p)
		if p.valid(false) {
			t.Fatal("invalid policy accepted", p)
		}
	}
}

func TestEntryPolicyWithoutALobbyKeepsLinkedPlayersInTheLoginSpace(t *testing.T) {
	repo := newFakeRepo()
	a := newTestAPI(repo, newFakeCluster())
	a.NoLobby = true
	view, _, err := a.readEntryPolicy(context.Background())
	if err != nil || !view.RequireAccountLink || view.WaitingSpace != "login" {
		t.Fatalf("default without a lobby = %+v, %v; want linking kept with login waiting", view, err)
	}
	repo.settings[entryPolicyKey] = []byte(`{"mode":"lobby","requireAccountLink":true,"offlineAction":"wake","waitingSpace":"lobby"}`)
	view, _, err = a.readEntryPolicy(context.Background())
	if err != nil || view.Mode != "domain" || view.WaitingSpace != "login" {
		t.Fatalf("a policy saved with a lobby = %+v, %v; want it moved off the lobby", view, err)
	}
	if (entryPolicy{Mode: "lobby", OfflineAction: "wake", WaitingSpace: "login"}).valid(true) {
		t.Fatal("lobby entry accepted without a lobby")
	}
	if !(entryPolicy{Mode: "domain", RequireAccountLink: true, OfflineAction: "wake", WaitingSpace: "login"}).valid(true) {
		t.Fatal("linked login-space waiting refused without a lobby")
	}
}

func TestEntryPolicyInternalCallerBoundary(t *testing.T) {
	a := newTestAPI(newFakeRepo(), newFakeCluster())
	a.Internal = CallerTokens{CallerVelocity: "proxy", CallerLimbo: "login", CallerBuild: "build"}
	for token, want := range map[string]int{"proxy": 200, "login": 200, "build": 403, "invalid": 401} {
		if w := do(a.InternalHandler(), "GET", "/api/v1/internal/settings/entry-policy", "", map[string]string{"Authorization": "Bearer " + token}); w.Code != want {
			t.Fatal(token, w.Code, w.Body.String())
		}
	}
}
