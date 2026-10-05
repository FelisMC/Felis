package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

const wakePolicyPath = "/api/v1/settings/wake-policy"

func TestWakePolicyPersistenceAndAdmission(t *testing.T) {
	repo := newFakeRepo()
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", AutostartPolicy: "public", Phase: "Stopped", DesiredState: "Stopped"}
	cl.list = []ServerInfo{{Name: "other", DesiredState: "Running"}}
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner"}
	a := newTestAPI(repo, cl)
	a.External = staticExternal{p: &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true}}
	view, _, err := a.readWakePolicy(context.Background())
	if err != nil || view.Managed {
		t.Fatal(view, err)
	}
	save := func(view wakePolicyView) int {
		body, _ := json.Marshal(map[string]any{"maxRunningServers": view.MaxRunningServers, "wakeCooldownSeconds": view.WakeCooldownSeconds, "revision": view.Revision})
		return do(a.ExternalHandler(), "PUT", wakePolicyPath, string(body), jsonHeader).Code
	}
	view.MaxRunningServers = 1
	view.WakeCooldownSeconds = 60
	if code := save(view); code != 200 {
		t.Fatal("save", code)
	}
	if code := save(view); code != 409 {
		t.Fatal("stale write", code)
	}
	replica := newTestAPI(repo, cl)
	replica.External = a.External
	persisted, _, err := replica.readWakePolicy(context.Background())
	if err != nil || !persisted.Managed || persisted.MaxRunningServers != 1 || persisted.cooldown(0) != time.Minute {
		t.Fatal(persisted, err)
	}
	if w := do(replica.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != 503 || decodeErr(t, w) != "at_capacity" {
		t.Fatal("panel cap", w.Code, w.Body.String())
	}
	if w := internalWake(replica, `{"mc_uuid":"`+wakeUUID+`"}`); w.Code != 503 || decodeErr(t, w) != "at_capacity" {
		t.Fatal("game cap", w.Code, w.Body.String())
	}
	if why, retry, err := replica.startScheduled(context.Background(), "survival"); err != nil || !retry || why == "" {
		t.Fatal("scheduled cap", why, retry, err)
	}
	persisted.MaxRunningServers = 2
	if code := save(persisted); code != 200 {
		t.Fatal("increase cap", code)
	}
	if w := do(replica.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != 202 || cl.desired["survival"] != v1alpha1.DesiredRunning {
		t.Fatal("wake", w.Code, w.Body.String())
	}
	if w := internalWake(replica, `{"mc_uuid":"`+wakeUUID+`"}`); w.Code != 429 {
		t.Fatal("shared saved cooldown", w.Code, w.Body.String())
	}
	repo.failGetSetting = errors.New("database down")
	if w := do(replica.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != 500 {
		t.Fatal("outage failed open", w.Code)
	}
	repo.failGetSetting = nil
	repo.settings[wakePolicyKey] = []byte(`{"maxRunningServers":-1}`)
	if _, _, err := replica.readWakePolicy(context.Background()); err == nil {
		t.Fatal("invalid persisted policy failed open")
	}
}

func TestWakePolicyAuthorizationAndValidation(t *testing.T) {
	a := newTestAPI(newFakeRepo(), newFakeCluster())
	for _, p := range []*Principal{nil, {UserID: "admin", Role: "admin", ViaAdminAccess: true}, {UserID: "owner", Role: "owner"}} {
		a.External = staticExternal{p: p}
		for _, method := range []string{"GET", "PUT"} {
			if w := do(a.ExternalHandler(), method, wakePolicyPath, `{}`, jsonHeader); w.Code != 401 && w.Code != 403 {
				t.Fatalf("unauthorized %+v: %d", p, w.Code)
			}
		}
	}
	p := &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true, ViaSession: true, EmailVerified: true}
	a.External = staticExternal{p: p}
	a.Repo.(*fakeRepo).passkeyCreds["owner-key"] = PasskeyCredential{ID: "owner-key", UserID: p.UserID, UserVerified: true}
	if w := do(a.ExternalHandler(), "PUT", wakePolicyPath, `{}`, jsonHeader); w.Code != 403 || decodeErr(t, w) != "reauth_required" {
		t.Fatal("reauth not enforced", w.Code)
	}
	p.ReauthAt = a.now()
	for _, body := range []string{`{"maxRunningServers":-1}`, `{"wakeCooldownSeconds":3601}`, `{"maxRunningServers":1.5}`} {
		if w := do(a.ExternalHandler(), "PUT", wakePolicyPath, body, jsonHeader); w.Code != 400 {
			t.Fatal("invalid policy", w.Code, w.Body.String())
		}
	}
}
