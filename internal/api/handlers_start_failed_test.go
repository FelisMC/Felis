package api

import (
	"net/http"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
)

// A start that Failed holds desiredState Running already, so the wake that
// re-writes Running changes nothing. These pin the two ways out: a person's
// wake in the panel starts it over (RetryStart), and a player's join onto one
// whose retries are spent is told so (409 start_failed) and never resets them.

func failedServer(gaveUp bool) *ServerInfo {
	return &ServerInfo{Name: "survival", AutostartPolicy: "public",
		DesiredState: string(v1alpha1.DesiredRunning), Phase: string(v1alpha1.PhaseFailed),
		AutoRestarts: v1alpha1.MaxAutoRestarts, StartGaveUp: gaveUp}
}

func TestWakeOfFailedServerRetriesStart(t *testing.T) {
	mk := func(info *ServerInfo) (*API, *fakeCluster, *fakeRepo) {
		repo := newFakeRepo()
		cl := newFakeCluster()
		cl.byName["survival"] = info
		api := newTestAPI(repo, cl)
		api.External = staticExternal{p: &Principal{UserID: "u1", Role: "user"}}
		return api, cl, repo
	}
	wake := func(api *API) int {
		return do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil).Code
	}

	t.Run("retries spent: the wake starts it over", func(t *testing.T) {
		api, cl, repo := mk(failedServer(true))
		if code := wake(api); code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202", code)
		}
		if len(cl.retried) != 1 || cl.retried[0] != "survival" {
			t.Fatalf("retried = %v, want [survival]", cl.retried)
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "retry_start" {
			t.Fatalf("audits = %+v, want one retry_start", repo.audits)
		}
	})

	t.Run("inside the backoff: a person asking skips the wait", func(t *testing.T) {
		api, cl, _ := mk(failedServer(false))
		if code := wake(api); code != http.StatusAccepted || len(cl.retried) != 1 {
			t.Fatalf("code = %d retried = %v, want 202 and one retry", code, cl.retried)
		}
	})

	t.Run("stopped: a plain wake", func(t *testing.T) {
		api, cl, repo := mk(&ServerInfo{Name: "survival", AutostartPolicy: "public",
			DesiredState: string(v1alpha1.DesiredStopped), Phase: string(v1alpha1.PhaseStopped)})
		if code := wake(api); code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202", code)
		}
		if len(cl.retried) != 0 || cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("retried = %v desired = %q, want a plain start", cl.retried, cl.desired["survival"])
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "wake" {
			t.Fatalf("audits = %+v, want one wake", repo.audits)
		}
	})

	t.Run("maintenance holds the world: refused, nothing retried", func(t *testing.T) {
		api, cl, _ := mk(failedServer(true))
		cl.wakeErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindRestore}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "maintenance_in_progress" {
			t.Fatalf("code = %d body %s, want 409 maintenance_in_progress", w.Code, w.Body.String())
		}
		if len(cl.retried) != 0 {
			t.Fatalf("retried = %v, want none", cl.retried)
		}
	})
}

func TestInternalWakeOfFailedServer(t *testing.T) {
	body := `{"mc_uuid":"` + wakeUUID + `"}`

	t.Run("retries spent: 409 start_failed, budget and cooldown untouched", func(t *testing.T) {
		api, cl := newInternalWakeAPI("public")
		cl.byName["survival"] = failedServer(true)
		api.WakeCooldown = time.Minute
		repo := api.Repo.(*fakeRepo)

		w := internalWake(api, body)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "start_failed" {
			t.Fatalf("code = %d body %s, want 409 start_failed", w.Code, w.Body.String())
		}
		if len(cl.retried) != 0 {
			t.Fatalf("a join must never reset the restart budget: retried = %v", cl.retried)
		}
		if len(repo.audits) != 0 {
			t.Fatalf("nothing was woken, so nothing is audited: %+v", repo.audits)
		}
		// The owner fixes it and starts it from the panel; the next join is not
		// held back by a cooldown the refused one never spent.
		cl.byName["survival"] = &ServerInfo{Name: "survival", AutostartPolicy: "public",
			DesiredState: string(v1alpha1.DesiredRunning), Phase: string(v1alpha1.PhaseStarting)}
		if w := internalWake(api, body); w.Code != http.StatusAccepted {
			t.Fatalf("join after the fix: code = %d body %s, want 202", w.Code, w.Body.String())
		}
	})

	t.Run("inside the backoff: 202, the player waits for the next attempt", func(t *testing.T) {
		api, cl := newInternalWakeAPI("public")
		cl.byName["survival"] = failedServer(false)
		if w := internalWake(api, body); w.Code != http.StatusAccepted {
			t.Fatalf("code = %d body %s, want 202", w.Code, w.Body.String())
		}
		if len(cl.retried) != 0 {
			t.Fatalf("a join must never reset the restart budget: retried = %v", cl.retried)
		}
	})

	t.Run("forbidden player: 403 comes first", func(t *testing.T) {
		api, cl := newInternalWakeAPI("ownerOnly")
		info := failedServer(true)
		info.AutostartPolicy = "ownerOnly"
		cl.byName["survival"] = info
		if w := internalWake(api, body); w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
	})
}
