package api

import (
	"net/http"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

// A stop flips desiredState to Stopped for the server's owner or an admin. A
// server whose database row outlived its MinecraftServer answers 404 not_found,
// the same as a wake of it, where it used to be an opaque 500.
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
