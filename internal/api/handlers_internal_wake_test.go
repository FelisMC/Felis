package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

// The internal-face wake (spec §9.1, §14) is what velocity drives for
// domain-autostart: service-token auth, the joining player identified by their
// verified online-mode UUID rather than a web Principal. These exercise the same
// autostartPolicy gate as the external wake but keyed by UUID, plus the shared
// cooldown and the phase reported back so velocity can decide whether to wait.

const wakeUUID = "11111111-2222-3333-4444-555555555555"

func newInternalWakeAPI(policy string) (*API, *fakeCluster) {
	repo := newFakeRepo()
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped", AutostartPolicy: policy}
	return newTestAPI(repo, cl), cl
}

func internalWake(api *API, body string) *httptest.ResponseRecorder {
	return do(api.InternalHandler(), "POST", "/api/v1/internal/servers/survival/wake", body, nil)
}

func TestInternalWakeAutostartGate(t *testing.T) {
	body := `{"mc_uuid":"` + wakeUUID + `"}`

	t.Run("public: any UUID wakes and gets phase back", func(t *testing.T) {
		api, cl := newInternalWakeAPI("public")
		w := internalWake(api, body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("desiredState = %q, want Running", cl.desired["survival"])
		}
		// velocity reads phase/ready to decide whether to hold the player.
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		if got["phase"] != "Stopped" {
			t.Fatalf("phase = %v, want Stopped", got["phase"])
		}
		if got["ready"] != false {
			t.Fatalf("ready = %v, want false", got["ready"])
		}
	})

	t.Run("missing mc_uuid is rejected", func(t *testing.T) {
		api, cl := newInternalWakeAPI("public")
		w := internalWake(api, `{}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
		if _, set := cl.desired["survival"]; set {
			t.Fatal("desiredState must not change when the UUID is missing")
		}
	})

	t.Run("ownerOnly: unlinked UUID forbidden", func(t *testing.T) {
		api, cl := newInternalWakeAPI("ownerOnly")
		api.Repo.(*fakeRepo).byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		w := internalWake(api, body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if _, set := cl.desired["survival"]; set {
			t.Fatal("desiredState must not change on a forbidden wake")
		}
	})

	t.Run("ownerOnly: owner's linked UUID wakes", func(t *testing.T) {
		api, cl := newInternalWakeAPI("ownerOnly")
		repo := api.Repo.(*fakeRepo)
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		repo.links[wakeUUID] = "owner1" // account_links: this UUID belongs to owner1
		if w := internalWake(api, body); w.Code != http.StatusAccepted {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("desiredState = %q, want Running", cl.desired["survival"])
		}
	})

	t.Run("ownerOnly: a different linked user is still forbidden", func(t *testing.T) {
		api, _ := newInternalWakeAPI("ownerOnly")
		repo := api.Repo.(*fakeRepo)
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		repo.links[wakeUUID] = "someone-else"
		if w := internalWake(api, body); w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
	})

	t.Run("ownerOnly: a linked admin wakes someone else's server without claiming", func(t *testing.T) {
		api, cl := newInternalWakeAPI("ownerOnly")
		repo := api.Repo.(*fakeRepo)
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		repo.links[wakeUUID] = "a1"
		repo.staff["op"] = &StaffUser{ID: "a1", Username: "op", Role: "admin"}
		if w := internalWake(api, body); w.Code != http.StatusAccepted {
			t.Fatalf("admin: code = %d body %s", w.Code, w.Body.String())
		}
		if cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("desiredState = %q, want Running", cl.desired["survival"])
		}
	})

	t.Run("allowlist: a linked admin bypasses the list", func(t *testing.T) {
		api, _ := newInternalWakeAPI("allowlist")
		repo := api.Repo.(*fakeRepo)
		repo.links[wakeUUID] = "a1"
		repo.staff["op"] = &StaffUser{ID: "a1", Username: "op", Role: "owner"} // owner ⊇ admin
		if w := internalWake(api, body); w.Code != http.StatusAccepted {
			t.Fatalf("staff off-list: code = %d body %s", w.Code, w.Body.String())
		}
	})

	t.Run("allowlist: only a listed UUID wakes", func(t *testing.T) {
		api, _ := newInternalWakeAPI("allowlist")
		repo := api.Repo.(*fakeRepo)
		repo.allowUUID["survival"] = map[string]bool{wakeUUID: true}
		if w := internalWake(api, body); w.Code != http.StatusAccepted {
			t.Fatalf("listed UUID: code = %d body %s", w.Code, w.Body.String())
		}

		api2, _ := newInternalWakeAPI("allowlist") // a different, unlisted UUID
		other := `{"mc_uuid":"99999999-0000-0000-0000-000000000000"}`
		if w := internalWake(api2, other); w.Code != http.StatusForbidden {
			t.Fatalf("unlisted UUID: code = %d, want 403", w.Code)
		}
	})

	t.Run("allowlist: owner bypasses the list even when not on it", func(t *testing.T) {
		api, cl := newInternalWakeAPI("allowlist")
		repo := api.Repo.(*fakeRepo)
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		repo.links[wakeUUID] = "owner1" // owner, but allowUUID is empty
		if w := internalWake(api, body); w.Code != http.StatusAccepted {
			t.Fatalf("owner: code = %d body %s", w.Code, w.Body.String())
		}
		if cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("desiredState = %q, want Running", cl.desired["survival"])
		}
	})
}

// A running server is joined, not woken: the menu and /felis go wake before they
// move anyone, and a friend who is not the owner of a running ownerOnly server was
// told "you may not start it". Only an up-and-staying-up server skips the gate; one
// that is on its way down is a real start and stays policy-gated.
func TestInternalWakeOfRunningServerIsNotGated(t *testing.T) {
	body := `{"mc_uuid":"` + wakeUUID + `"}`
	running := func(desired v1alpha1.DesiredState) (*API, *fakeCluster, *fakeRepo) {
		api, cl := newInternalWakeAPI("ownerOnly")
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Running", Ready: true,
			AutostartPolicy: "ownerOnly", DesiredState: string(desired)}
		repo := api.Repo.(*fakeRepo)
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		repo.links[wakeUUID] = "someone-else"
		api.WakeCooldown = time.Minute
		return api, cl, repo
	}

	t.Run("up: a non-owner gets 202 ready and nothing changes", func(t *testing.T) {
		api, cl, repo := running(v1alpha1.DesiredRunning)
		w := internalWake(api, body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (body %s)", w.Code, w.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		if got["ready"] != true || got["phase"] != "Running" {
			t.Fatalf("reply = %v, want ready true and phase Running", got)
		}
		if _, set := cl.desired["survival"]; set {
			t.Fatal("a no-op wake must not write desiredState")
		}
		if len(repo.audits) != 0 {
			t.Fatalf("a no-op wake must not be audited as a wake: %+v", repo.audits)
		}
		// Nothing was woken, so the cooldown is untouched: a second join is not a 429.
		if w := internalWake(api, body); w.Code != http.StatusAccepted {
			t.Fatalf("second join code = %d, want 202", w.Code)
		}
	})

	t.Run("stopping: a non-owner's wake is still a start and still forbidden", func(t *testing.T) {
		api, cl, _ := running(v1alpha1.DesiredStopped)
		if w := internalWake(api, body); w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if _, set := cl.desired["survival"]; set {
			t.Fatal("desiredState must not change on a forbidden wake")
		}
	})
}

func TestInternalWakeCooldownIsShared(t *testing.T) {
	api, _ := newInternalWakeAPI("public")
	api.WakeCooldown = time.Minute
	body := `{"mc_uuid":"` + wakeUUID + `"}`

	if w := internalWake(api, body); w.Code != http.StatusAccepted {
		t.Fatalf("first wake code = %d", w.Code)
	}
	// clock is frozen, so the second wake is inside the cooldown window; velocity
	// reads this 429 as "already waking, keep waiting", not a failure.
	if w := internalWake(api, body); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second wake code = %d, want 429", w.Code)
	}
}

// TestInternalWakeRunningCap proves the §9.1 cap is shared by the velocity-driven
// internal wake, not only the web wake: with one slot and another server already
// desired-Running, a join-triggered wake of a stopped server is held with 503
// at_capacity and leaves desiredState untouched. velocity reads this as "cluster
// full, hold the player", distinct from the 429 cooldown's "already waking".
func TestInternalWakeRunningCap(t *testing.T) {
	api, cl := newInternalWakeAPI("public")
	api.MaxRunningServers = 1
	cl.list = []ServerInfo{{Name: "other", DesiredState: string(v1alpha1.DesiredRunning)}}
	body := `{"mc_uuid":"` + wakeUUID + `"}`

	w := internalWake(api, body)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 at_capacity (body %s)", w.Code, w.Body.String())
	}
	if code := decodeErr(t, w); code != "at_capacity" {
		t.Fatalf("error code = %q, want at_capacity", code)
	}
	if _, set := cl.desired["survival"]; set {
		t.Fatal("desiredState must not change when the cluster is at capacity")
	}
}

func TestInternalStatusServedOnInternalFace(t *testing.T) {
	api, _ := newInternalWakeAPI("public")
	w := do(api.InternalHandler(), "GET", "/api/v1/internal/servers/survival/status", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d body %s", w.Code, w.Body.String())
	}
}
