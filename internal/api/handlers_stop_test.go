package api

import (
	"net/http"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

func TestStop(t *testing.T) {
	mk := func(p *Principal, inCluster bool) (*API, *fakeCluster, *fakeRepo) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "u1"}
		cl := newFakeCluster()
		if inCluster {
			cl.byName["survival"] = &ServerInfo{Name: "survival", DesiredState: string(v1alpha1.DesiredRunning)}
		}
		api := newTestAPI(repo, cl)
		api.External = staticExternal{p: p}
		return api, cl, repo
	}
	owner := &Principal{UserID: "u1", Role: "user"}
	stop := func(api *API) (int, string) {
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/stop", "", nil)
		if w.Code == http.StatusAccepted {
			return w.Code, ""
		}
		return w.Code, decodeErr(t, w)
	}

	t.Run("the owner stops it", func(t *testing.T) {
		api, cl, repo := mk(owner, true)
		if code, e := stop(api); code != http.StatusAccepted {
			t.Fatalf("stop = %d %s, want 202", code, e)
		}
		if cl.desired["survival"] != v1alpha1.DesiredStopped {
			t.Fatalf("desired = %q, want Stopped", cl.desired["survival"])
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "stop" || repo.audits[0].ServerName != "survival" {
			t.Fatalf("audits = %+v, want one stop of survival", repo.audits)
		}
	})

	t.Run("someone else is refused", func(t *testing.T) {
		api, cl, repo := mk(&Principal{UserID: "u2", Role: "user"}, true)
		if code, e := stop(api); code != http.StatusForbidden || e != "forbidden" {
			t.Fatalf("stop = %d %s, want 403 forbidden", code, e)
		}
		if _, set := cl.desired["survival"]; set || len(repo.audits) != 0 {
			t.Fatalf("a refused stop wrote desired=%v audits=%+v", cl.desired, repo.audits)
		}
	})

	t.Run("its MinecraftServer is gone", func(t *testing.T) {
		api, _, repo := mk(owner, false)
		if code, e := stop(api); code != http.StatusNotFound || e != "not_found" {
			t.Fatalf("stop = %d %s, want 404 not_found", code, e)
		}
		if len(repo.audits) != 0 {
			t.Fatalf("audits = %+v, want none", repo.audits)
		}
	})
}

func TestKillAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, server, role, user string
		staff                    bool
		status                   int
	}{
		{"server owner", "survival", "user", "owner1", false, 202},
		{"administrator", "survival", "admin", "admin1", true, 202},
		{"ordinary player", "survival", "user", "stranger", false, 403},
		{"admin without admin access", "survival", "admin", "stranger", false, 403},
		{"staff system service", "lobby", "admin", "admin1", true, 202},
		{"player system service", "lobby", "user", "owner1", false, 400},
		{"missing server", "missing", "user", "owner1", false, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, cl, _ := mkFiles(t)
			cl.byName["lobby"] = &ServerInfo{Name: "lobby", Phase: "Running", DesiredState: "Running"}
			a.External = staticExternal{p: &Principal{UserID: tc.user, Role: tc.role, ViaAdminAccess: tc.staff}}
			w := do(a.ExternalHandler(), "POST", "/api/v1/servers/"+tc.server+"/kill", "", nil)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
			if tc.status != http.StatusAccepted {
				if len(cl.killed) != 0 || len(repo.audits) != 0 {
					t.Fatal("refused kill changed the server or wrote a success audit")
				}
				return
			}
			if len(cl.killed) != 1 || cl.killed[0] != tc.server || cl.desired[tc.server] != v1alpha1.DesiredStopped {
				t.Fatalf("kill=%v desired=%v", cl.killed, cl.desired)
			}
			if len(repo.audits) != 1 || repo.audits[0].Action != "server.kill" || repo.audits[0].ServerName != tc.server {
				t.Fatalf("audits=%+v", repo.audits)
			}
		})
	}
}
