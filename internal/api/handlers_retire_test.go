package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

var (
	retireOwner    = &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}
	retireAdmin    = &Principal{UserID: "admin1", Role: "admin", ViaAdminAccess: true}
	retireStranger = &Principal{UserID: "other", Email: "other@example.net", Role: "user"}
)

// retireAPI serves survival, owned by owner1 and running, to p.
func retireAPI(p *Principal) (*API, *fakeRepo, *fakeCluster) {
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Running", Ready: true,
		DesiredState: string(v1alpha1.DesiredRunning), AutostartPolicy: "public"}
	a := newTestAPI(repo, cl)
	a.External = staticExternal{p: p}
	return a, repo, cl
}

func putRetire(a *API, body string) *httpResult {
	return result(do(a.ExternalHandler(), "PUT", "/api/v1/servers/survival/retirement", body, jsonHeader))
}

func cancelRetire(a *API) *httpResult {
	return result(do(a.ExternalHandler(), "DELETE", "/api/v1/servers/survival/retirement", "", nil))
}

// TestRetireRequest covers PUT /servers/{name}/retirement: the owner may give the
// server up and an admin may delete it, both only with the name typed back; the
// server is stopped and the request recorded for the reaper, and audited. Every
// refusal leaves the server running and nothing recorded.
func TestRetireRequest(t *testing.T) {
	t.Run("owner gives the server up", func(t *testing.T) {
		a, repo, cl := retireAPI(retireOwner)
		res := putRetire(a, `{"confirm":"survival"}`)
		if res.code != http.StatusAccepted {
			t.Fatalf("code = %d (%s)", res.code, res.body)
		}
		var got struct {
			Name     string      `json:"name"`
			Retiring RetireState `json:"retiring"`
		}
		if err := json.Unmarshal([]byte(res.body), &got); err != nil || got.Name != "survival" ||
			got.Retiring.Delete || got.Retiring.RequestedAt.IsZero() {
			t.Fatalf("body = %s (%v)", res.body, err)
		}
		if cl.desired["survival"] != v1alpha1.DesiredStopped {
			t.Fatalf("desiredState = %q, want Stopped", cl.desired["survival"])
		}
		if r := repo.byName["survival"].Retire; r == nil || r.Delete {
			t.Fatalf("recorded = %+v, want a release", r)
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "server.release" || repo.audits[0].ActorUserID != "owner1" ||
			!strings.Contains(string(repo.audits[0].Payload), `"owner_id":"owner1"`) {
			t.Fatalf("audits = %+v", repo.audits)
		}
	})

	t.Run("admin deletes the server", func(t *testing.T) {
		a, repo, cl := retireAPI(retireAdmin)
		res := putRetire(a, `{"confirm":"survival","delete":true}`)
		if res.code != http.StatusAccepted || !strings.Contains(res.body, `"delete":true`) {
			t.Fatalf("code = %d (%s)", res.code, res.body)
		}
		if cl.desired["survival"] != v1alpha1.DesiredStopped {
			t.Fatalf("desiredState = %q, want Stopped", cl.desired["survival"])
		}
		if r := repo.byName["survival"].Retire; r == nil || !r.Delete {
			t.Fatalf("recorded = %+v, want a deletion", r)
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "server.delete" || repo.audits[0].ActorUserID != "admin1" {
			t.Fatalf("audits = %+v", repo.audits)
		}
	})

	t.Run("a server whose MinecraftServer is gone can still be deleted", func(t *testing.T) {
		a, repo, cl := retireAPI(retireAdmin)
		delete(cl.byName, "survival")
		if res := putRetire(a, `{"confirm":"survival","delete":true}`); res.code != http.StatusAccepted {
			t.Fatalf("code = %d (%s)", res.code, res.body)
		}
		if _, set := cl.desired["survival"]; set || repo.byName["survival"].Retire == nil {
			t.Fatalf("desired %v, recorded %+v", cl.desired, repo.byName["survival"].Retire)
		}
	})

	for _, tc := range []struct {
		name  string
		p     *Principal
		body  string
		setup func(*fakeRepo, *fakeCluster)
		code  int
		err   string
	}{
		{"owner may not delete", retireOwner, `{"confirm":"survival","delete":true}`, nil, http.StatusForbidden, "forbidden"},
		{"stranger", retireStranger, `{"confirm":"survival"}`, nil, http.StatusForbidden, "forbidden"},
		{"name not typed back", retireOwner, `{"confirm":"Survival"}`, nil, http.StatusBadRequest, "confirm_mismatch"},
		{"no confirmation", retireAdmin, `{"delete":true}`, nil, http.StatusBadRequest, "confirm_mismatch"},
		{"system server", retireAdmin, `{"confirm":"survival","delete":true}`,
			func(_ *fakeRepo, cl *fakeCluster) { cl.byName["survival"].ReaperExempt = true },
			http.StatusConflict, "system_server"},
		{"unknown server", retireAdmin, `{"confirm":"survival","delete":true}`,
			func(repo *fakeRepo, _ *fakeCluster) { delete(repo.byName, "survival") },
			http.StatusNotFound, "not_found"},
		{"release of a server with no MinecraftServer", retireAdmin, `{"confirm":"survival"}`,
			func(_ *fakeRepo, cl *fakeCluster) { delete(cl.byName, "survival") },
			http.StatusNotFound, "not_found"},
		{"world volume left without its server", retireAdmin, `{"confirm":"survival","delete":true}`,
			func(_ *fakeRepo, cl *fakeCluster) {
				delete(cl.byName, "survival")
				cl.orphanWorld = map[string]bool{"survival": true}
			},
			http.StatusConflict, "world_volume_orphaned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, cl := retireAPI(tc.p)
			if tc.setup != nil {
				tc.setup(repo, cl)
			}
			res := putRetire(a, tc.body)
			if res.code != tc.code || res.errCode() != tc.err {
				t.Fatalf("code = %d %q, want %d %q (%s)", res.code, res.errCode(), tc.code, tc.err, res.body)
			}
			if _, set := cl.desired["survival"]; set {
				t.Fatal("a refused request stopped the server")
			}
			if rec := repo.byName["survival"]; rec != nil && rec.Retire != nil {
				t.Fatalf("a refused request was recorded: %+v", rec.Retire)
			}
			if len(repo.audits) != 0 {
				t.Fatalf("a refused request was audited: %+v", repo.audits)
			}
		})
	}
}

// TestRetireCancel covers DELETE /servers/{name}/retirement: the owner takes back
// giving the server up, but only an admin cancels a deletion.
func TestRetireCancel(t *testing.T) {
	pending := func(repo *fakeRepo, del bool) {
		repo.byName["survival"].Retire = &RetireState{RequestedAt: time.Now(), Delete: del}
	}
	for _, tc := range []struct {
		name    string
		p       *Principal
		del     bool
		code    int
		cleared bool
	}{
		{"owner cancels giving it up", retireOwner, false, http.StatusNoContent, true},
		{"owner may not cancel a deletion", retireOwner, true, http.StatusForbidden, false},
		{"admin cancels a deletion", retireAdmin, true, http.StatusNoContent, true},
		{"stranger", retireStranger, false, http.StatusForbidden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, _ := retireAPI(tc.p)
			pending(repo, tc.del)
			res := cancelRetire(a)
			if res.code != tc.code {
				t.Fatalf("code = %d, want %d (%s)", res.code, tc.code, res.body)
			}
			if cleared := repo.byName["survival"].Retire == nil; cleared != tc.cleared {
				t.Fatalf("cleared = %v, want %v", cleared, tc.cleared)
			}
			audited := len(repo.audits) == 1 && repo.audits[0].Action == "server.retire_cancel"
			if audited != tc.cleared || (!tc.cleared && len(repo.audits) != 0) {
				t.Fatalf("audits = %+v", repo.audits)
			}
		})
	}

	t.Run("nothing pending is a quiet no-op", func(t *testing.T) {
		a, repo, _ := retireAPI(retireOwner)
		if res := cancelRetire(a); res.code != http.StatusNoContent || len(repo.audits) != 0 {
			t.Fatalf("code = %d, audits %+v", res.code, repo.audits)
		}
	})
}

// A server with a pending retirement stays as its owner left it until the reaper
// archives it: no face wakes or claims it, the lobby does not offer it, and the
// owner's and admin's views say it is going.
func TestRetiringServerIsFrozen(t *testing.T) {
	t.Run("external wake", func(t *testing.T) {
		a, repo, cl := retireAPI(retireOwner)
		cl.byName["survival"].DesiredState = string(v1alpha1.DesiredStopped)
		cl.byName["survival"].Phase, cl.byName["survival"].Ready = "Stopped", false
		repo.byName["survival"].Retire = &RetireState{RequestedAt: time.Now()}
		res := result(do(a.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil))
		if res.code != http.StatusConflict || res.errCode() != "server_retiring" {
			t.Fatalf("code = %d %q (%s)", res.code, res.errCode(), res.body)
		}
		if _, set := cl.desired["survival"]; set {
			t.Fatal("the wake went through")
		}
	})

	t.Run("internal wake", func(t *testing.T) {
		a, repo, cl := retireAPI(nil)
		cl.byName["survival"].DesiredState = string(v1alpha1.DesiredStopped)
		cl.byName["survival"].Phase, cl.byName["survival"].Ready = "Stopped", false
		repo.byName["survival"].Retire = &RetireState{RequestedAt: time.Now()}
		res := result(internalWake(a, `{"mc_uuid":"`+wakeUUID+`"}`))
		if res.code != http.StatusConflict || res.errCode() != "server_retiring" {
			t.Fatalf("code = %d %q (%s)", res.code, res.errCode(), res.body)
		}
		if _, set := cl.desired["survival"]; set {
			t.Fatal("the wake went through")
		}
	})

	t.Run("claim of an unowned server being deleted", func(t *testing.T) {
		a, repo, _ := retireAPI(retireStranger)
		repo.byName["survival"] = &ServerRecord{Name: "survival", Retire: &RetireState{RequestedAt: time.Now(), Delete: true}}
		repo.linked["other"], repo.quota["other"], repo.claimOK["survival"] = true, true, true
		res := result(do(a.ExternalHandler(), "POST", "/api/v1/servers/survival/claim", "", nil))
		if res.code != http.StatusConflict || res.errCode() != "server_retiring" {
			t.Fatalf("code = %d %q (%s)", res.code, res.errCode(), res.body)
		}
		if len(repo.audits) != 0 {
			t.Fatalf("audits = %+v", repo.audits)
		}
	})

	t.Run("lobby menu", func(t *testing.T) {
		a, repo, _ := retireAPI(nil)
		repo.byName["survival"] = &ServerRecord{Name: "survival", Retire: &RetireState{RequestedAt: time.Now(), Delete: true}}
		res := result(internalMenu(a))
		if res.code != http.StatusOK || !strings.Contains(res.body, `"claimable":false`) {
			t.Fatalf("code = %d (%s)", res.code, res.body)
		}
	})

	t.Run("status shows the request to the owner only", func(t *testing.T) {
		for _, tc := range []struct {
			p    *Principal
			sees bool
		}{{retireOwner, true}, {retireAdmin, true}, {retireStranger, false}} {
			a, repo, _ := retireAPI(tc.p)
			repo.byName["survival"].Retire = &RetireState{RequestedAt: time.Now()}
			res := result(do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/status", "", nil))
			if res.code != http.StatusOK || strings.Contains(res.body, `"retiring"`) != tc.sees {
				t.Fatalf("%s: code = %d (%s)", tc.p.UserID, res.code, res.body)
			}
		}
	})

	t.Run("fleet", func(t *testing.T) {
		a, repo, cl := retireAPI(retireAdmin)
		cl.list = []ServerInfo{{Name: "survival", Phase: "Stopped"}, {Name: "creative", Phase: "Stopped"}}
		repo.owners["survival"] = ServerOwnership{Retire: &RetireState{RequestedAt: time.Now(), Delete: true}}
		repo.owners["creative"] = ServerOwnership{}
		res := result(do(a.ExternalHandler(), "GET", "/api/v1/fleet", "", nil))
		var got struct {
			Servers []fleetServerView `json:"servers"`
		}
		if res.code != http.StatusOK || json.Unmarshal([]byte(res.body), &got) != nil || len(got.Servers) != 2 {
			t.Fatalf("code = %d (%s)", res.code, res.body)
		}
		if s := got.Servers[0]; s.Claimable || s.Retiring == nil || !s.Retiring.Delete {
			t.Fatalf("survival = %+v, want retiring and not claimable", s)
		}
		if s := got.Servers[1]; !s.Claimable || s.Retiring != nil {
			t.Fatalf("creative = %+v, want claimable", s)
		}
	})
}

type httpResult struct {
	code int
	body string
}

func result(w *httptest.ResponseRecorder) *httpResult {
	return &httpResult{code: w.Code, body: w.Body.String()}
}

// errCode is the error envelope's code, "" for a body that is not one.
func (r *httpResult) errCode() string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(r.body), &env)
	return env.Error.Code
}
