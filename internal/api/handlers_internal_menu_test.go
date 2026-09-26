package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

// The internal-face claim + menu pair (spec §9.3, §12) is what velocity drives for
// the felis-paper lobby, which holds no token of its own. The claim mirrors the
// external Principal-gated claim but keys identity off the verified online-mode
// UUID; the menu adds the ownership-derived `claimable` flag the §11 lifecycle
// views never carry. These assert the exact wire keys, not just status, because a
// key velocity's parser reads but the handler never emits fails silently — every
// tile would read claimable=false forever and a status-only test would still pass.

const menuUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func internalClaim(api *API, body string) *httptest.ResponseRecorder {
	return do(api.InternalHandler(), "POST", "/api/v1/internal/servers/survival/claim", body, nil)
}

func internalMenu(api *API) *httptest.ResponseRecorder {
	return do(api.InternalHandler(), "GET", "/api/v1/internal/servers/survival/menu", "", nil)
}

func TestInternalClaim(t *testing.T) {
	body := `{"mc_uuid":"` + menuUUID + `"}`

	// linkAndQuota wires an API whose menuUUID is linked to userID and (optionally)
	// has quota headroom, with the survival server present and claimable.
	setup := func() (*API, *fakeRepo) {
		repo := newFakeRepo()
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped"}
		return newTestAPI(repo, cl), repo
	}

	t.Run("happy path: linked + quota + ownerless → 200 claimed", func(t *testing.T) {
		api, repo := setup()
		repo.links[menuUUID] = "user1" // account_links: UUID → user1 (implies linked)
		repo.quota["user1"] = true
		repo.claimOK["survival"] = true // UPDATE ... WHERE owner_id IS NULL hits 1 row

		w := internalClaim(api, body)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		// Exact wire contract velocity's ClaimResponse parser reads.
		if got["name"] != "survival" {
			t.Fatalf("name = %v, want survival", got["name"])
		}
		if got["claimed"] != true {
			t.Fatalf("claimed = %v, want true", got["claimed"])
		}
		// The claim is by the resolved user, and it is audited as a velocity action.
		repo.assertClaimAudit(t, "survival")
	})

	t.Run("unlinked UUID → 412 not_linked, no claim", func(t *testing.T) {
		api, repo := setup()
		repo.claimOK["survival"] = true // would succeed if we got that far
		// menuUUID is absent from repo.links → UserByMCUUID returns ErrNotFound.

		w := internalClaim(api, body)
		if w.Code != http.StatusPreconditionFailed {
			t.Fatalf("code = %d, want 412 (%s)", w.Code, w.Body.String())
		}
		if code := decodeErr(t, w); code != "not_linked" {
			t.Fatalf("error code = %q, want not_linked", code)
		}
		if len(repo.audits) != 0 {
			t.Fatal("an unlinked claim must not be audited as a claim")
		}
	})

	t.Run("over quota → 403 quota_exceeded", func(t *testing.T) {
		api, repo := setup()
		repo.links[menuUUID] = "user1"
		repo.quota["user1"] = false // at the ceiling
		repo.claimOK["survival"] = true

		w := internalClaim(api, body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403 (%s)", w.Code, w.Body.String())
		}
		if code := decodeErr(t, w); code != "quota_exceeded" {
			t.Fatalf("error code = %q, want quota_exceeded", code)
		}
	})

	t.Run("already claimed (lost race) → 409", func(t *testing.T) {
		api, repo := setup()
		repo.links[menuUUID] = "user1"
		repo.quota["user1"] = true
		repo.claimOK["survival"] = false // present but UPDATE hits 0 rows

		w := internalClaim(api, body)
		if w.Code != http.StatusConflict {
			t.Fatalf("code = %d, want 409 (%s)", w.Code, w.Body.String())
		}
		if code := decodeErr(t, w); code != "already_claimed" {
			t.Fatalf("error code = %q, want already_claimed", code)
		}
	})

	t.Run("missing server → 404, distinct from the unlinked 412", func(t *testing.T) {
		api, repo := setup()
		repo.links[menuUUID] = "user1"
		repo.quota["user1"] = true
		// survival absent from repo.claimOK → ClaimServer returns ErrNotFound.

		w := internalClaim(api, body)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404 (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("missing mc_uuid is rejected", func(t *testing.T) {
		api, repo := setup()
		repo.claimOK["survival"] = true
		w := internalClaim(api, `{}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})
}

func TestInternalMenuStatus(t *testing.T) {
	setup := func() (*API, *fakeRepo, *fakeCluster) {
		repo := newFakeRepo()
		cl := newFakeCluster()
		return newTestAPI(repo, cl), repo, cl
	}

	t.Run("ownerless server → claimable, with exact keys", func(t *testing.T) {
		api, _, cl := setup()
		cl.byName["survival"] = &ServerInfo{
			Name: "survival", Phase: "Running", Ready: true,
			PlayersOnline: 3, PlayersMax: 20,
		}
		// no servers-row at all → treated as ownerless → claimable.

		w := internalMenu(api)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		got := decodeMenu(t, w)
		// Assert every key the lobby's MenuStatus parser reads, by exact name —
		// the §11-trap guard: a renamed/absent key would read as a zero value.
		assertEq(t, "name", got["name"], "survival")
		assertEq(t, "phase", got["phase"], "Running")
		assertEq(t, "ready", got["ready"], true)
		assertEq(t, "playersOnline", got["playersOnline"], float64(3))
		assertEq(t, "playersMax", got["playersMax"], float64(20))
		assertEq(t, "claimable", got["claimable"], true)
	})

	t.Run("owned server → not claimable", func(t *testing.T) {
		api, repo, cl := setup()
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped"}
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}

		got := decodeMenu(t, internalMenu(api))
		assertEq(t, "claimable", got["claimable"], false)
	})

	t.Run("seeded but ownerless (empty owner_id) → claimable", func(t *testing.T) {
		api, repo, cl := setup()
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped"}
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: ""}

		got := decodeMenu(t, internalMenu(api))
		assertEq(t, "claimable", got["claimable"], true)
	})

	t.Run("unknown server → 404", func(t *testing.T) {
		api, _, _ := setup()
		if w := internalMenu(api); w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404 (%s)", w.Code, w.Body.String())
		}
	})
}

// ---- local helpers ----

func decodeMenu(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
	}
	return got
}

func assertEq(t *testing.T, key string, got, want any) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

func (f *fakeRepo) assertClaimAudit(t *testing.T, name string) {
	t.Helper()
	for _, e := range f.audits {
		if e.Action == "claim" && e.ServerName == name && e.Actor == "velocity" && e.Source == "internal:velocity" {
			return
		}
	}
	t.Fatalf("no velocity/internal claim audit for %q in %+v", name, f.audits)
}

func internalMenuAccess(api *API, uuid string) *httptest.ResponseRecorder {
	return do(api.InternalHandler(), "GET", "/api/v1/internal/player/menu-access/"+uuid, "", nil)
}

// The menu-access verdicts tell the lobby, per tile, whether this player may start
// the server and why not, with the wake's own gates behind each one.
func TestInternalMenuAccess(t *testing.T) {
	gone := &RetireState{RequestedAt: time.Now()}
	setup := func() (*API, *fakeRepo) {
		repo := newFakeRepo()
		cl := newFakeCluster()
		running := string(v1alpha1.DesiredRunning)
		cl.list = []ServerInfo{
			{Name: "pub", AutostartPolicy: "public"},
			{Name: "mine", AutostartPolicy: "ownerOnly"},
			{Name: "theirs", AutostartPolicy: "ownerOnly"},
			{Name: "unset"},
			{Name: "listed", AutostartPolicy: "allowlist"},
			{Name: "unlisted", AutostartPolicy: "allowlist"},
			{Name: "ownerless", AutostartPolicy: "ownerOnly"},
			{Name: "gone", AutostartPolicy: "public"},
			{Name: "mine-gone", AutostartPolicy: "ownerOnly"},
			{Name: "broken", AutostartPolicy: "public", StartGaveUp: true, DesiredState: running},
			{Name: "mine-broken", AutostartPolicy: "ownerOnly", StartGaveUp: true, DesiredState: running},
			{Name: "broken-stopped", AutostartPolicy: "public", StartGaveUp: true},
			{Name: "lobby", AutostartPolicy: "public"},
		}
		repo.owners = map[string]ServerOwnership{
			"pub":            {OwnerID: "u2"},
			"mine":           {OwnerID: "user1"},
			"theirs":         {OwnerID: "u2"},
			"unset":          {OwnerID: "u2"},
			"listed":         {OwnerID: "u2"},
			"unlisted":       {OwnerID: "u2"},
			"ownerless":      {},
			"gone":           {OwnerID: "u2", Retire: gone},
			"mine-gone":      {OwnerID: "user1", Retire: gone},
			"broken":         {OwnerID: "u2"},
			"mine-broken":    {OwnerID: "user1"},
			"broken-stopped": {OwnerID: "u2"},
		}
		repo.allowUUID["listed"] = map[string]bool{menuUUID: true}
		return newTestAPI(repo, cl), repo
	}
	verdicts := func(t *testing.T, api *API) map[string]any {
		t.Helper()
		got := decodeMenu(t, internalMenuAccess(api, menuUUID))
		servers, ok := got["servers"].(map[string]any)
		if !ok {
			t.Fatalf("servers = %v, want an object", got["servers"])
		}
		return servers
	}

	t.Run("a linked player", func(t *testing.T) {
		api, repo := setup()
		repo.links[menuUUID] = "user1"
		got := verdicts(t, api)
		for name, want := range map[string]string{
			"pub":            "wake",
			"mine":           "owner",
			"theirs":         "owner_only",
			"unset":          "owner_only",
			"listed":         "wake",
			"unlisted":       "allowlist",
			"ownerless":      "owner_only",
			"gone":           "retiring",
			"mine-gone":      "retiring",
			"broken":         "start_failed",
			"mine-broken":    "start_failed",
			"broken-stopped": "wake",
		} {
			assertEq(t, name, got[name], want)
		}
		if _, listed := got["lobby"]; listed {
			t.Fatal("the lobby is not a menu tile, yet it has a verdict")
		}
		assertEq(t, "verdict count", len(got), 12)
	})

	t.Run("staff start any server that is not retiring or failed", func(t *testing.T) {
		api, repo := setup()
		repo.links[menuUUID] = "a1"
		repo.staff["op"] = &StaffUser{ID: "a1", Username: "op", Role: "admin"}
		got := verdicts(t, api)
		assertEq(t, "theirs", got["theirs"], "wake")
		assertEq(t, "unlisted", got["unlisted"], "wake")
		assertEq(t, "ownerless", got["ownerless"], "wake")
		assertEq(t, "gone", got["gone"], "retiring")
	})

	t.Run("an unlinked UUID owns nothing, not even an ownerless server", func(t *testing.T) {
		api, _ := setup()
		got := verdicts(t, api)
		assertEq(t, "mine", got["mine"], "owner_only")
		assertEq(t, "ownerless", got["ownerless"], "owner_only")
		assertEq(t, "listed", got["listed"], "wake")
		assertEq(t, "pub", got["pub"], "wake")
	})

	t.Run("a failed read fails the call", func(t *testing.T) {
		api, repo := setup()
		repo.ownersErr = errors.New("db down")
		if w := internalMenuAccess(api, menuUUID); w.Code != http.StatusInternalServerError {
			t.Fatalf("owners unreadable: code = %d, want 500 (%s)", w.Code, w.Body.String())
		}
		api, _ = setup()
		api.Cluster.(*fakeCluster).listErr = errors.New("apiserver down")
		if w := internalMenuAccess(api, menuUUID); w.Code < 500 {
			t.Fatalf("cluster unreadable: code = %d, want 5xx (%s)", w.Code, w.Body.String())
		}
	})
}
