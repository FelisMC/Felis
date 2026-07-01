package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

// TestListBackups exercises GET /api/v1/backups (spec §7): admin sees every
// present backup, a regular user sees only the worlds they formerly owned, and
// expired/deleted backups are never listed. It also asserts the opaque
// backup_ref never reaches the wire (spec §286 principle).
func TestListBackups(t *testing.T) {
	mk := func() *API {
		repo := newFakeRepo()
		repo.backups = []fakeBackup{
			{view: BackupView{ID: "b1", ServerName: "alpha", FormerOwner: "owner1",
				Status: "present", Reason: "inactive_15d", SizeBytes: 100,
				CreatedAt: time.Unix(1_699_000_000, 0), ExpiresAt: time.Unix(1_706_000_000, 0)}, ref: "ref-b1"},
			{view: BackupView{ID: "b2", ServerName: "beta", FormerOwner: "owner2",
				Status: "present", Reason: "manual", SizeBytes: 200,
				CreatedAt: time.Unix(1_699_500_000, 0), ExpiresAt: time.Unix(1_706_000_000, 0)}, ref: "ref-b2"},
			{view: BackupView{ID: "b3", ServerName: "gamma", FormerOwner: "owner1",
				Status: "deleted", Reason: "manual", SizeBytes: 50,
				CreatedAt: time.Unix(1_698_000_000, 0), ExpiresAt: time.Unix(1_705_000_000, 0)}, ref: "ref-b3"},
		}
		return newTestAPI(repo, newFakeCluster())
	}

	list := func(t *testing.T, w *httptest.ResponseRecorder) []BackupView {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Backups []BackupView `json:"backups"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		return resp.Backups
	}
	ids := func(vs []BackupView) map[string]bool {
		m := map[string]bool{}
		for _, v := range vs {
			m[v.ID] = true
		}
		return m
	}

	t.Run("admin sees all present, not deleted", func(t *testing.T) {
		api := mk()
		api.External = staticExternal{p: &Principal{UserID: "admin1", Role: "admin", ViaAdminAccess: true}}
		got := ids(list(t, do(api.ExternalHandler(), "GET", "/api/v1/backups", "", nil)))
		if !got["b1"] || !got["b2"] || got["b3"] || len(got) != 2 {
			t.Fatalf("admin scope = %v, want {b1,b2}", got)
		}
	})

	t.Run("user sees only own former-owned present backups", func(t *testing.T) {
		api := mk()
		api.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
		got := ids(list(t, do(api.ExternalHandler(), "GET", "/api/v1/backups", "", nil)))
		// b2 belongs to owner2; b3 is owner1's but deleted — neither is visible.
		if !got["b1"] || got["b2"] || got["b3"] || len(got) != 1 {
			t.Fatalf("owner1 scope = %v, want {b1}", got)
		}
	})

	t.Run("user with no backups -> empty array, never null", func(t *testing.T) {
		api := mk()
		api.External = staticExternal{p: &Principal{UserID: "stranger", Role: "user"}}
		w := do(api.ExternalHandler(), "GET", "/api/v1/backups", "", nil)
		vs := list(t, w)
		if vs == nil || len(vs) != 0 {
			t.Fatalf("empty scope = %v, want a non-nil empty slice", vs)
		}
	})

	t.Run("backup_ref never serialized", func(t *testing.T) {
		api := mk()
		api.External = staticExternal{p: &Principal{UserID: "admin1", Role: "admin", ViaAdminAccess: true}}
		body := do(api.ExternalHandler(), "GET", "/api/v1/backups", "", nil).Body.String()
		if strings.Contains(body, "backup_ref") || strings.Contains(body, "ref-b") {
			t.Fatalf("response leaked the internal backup_ref: %s", body)
		}
	})
}

// TestRestoreBackup exercises POST /api/v1/servers/{name}/restore-backup (spec §7,
// §466). The default server is stopped, owned by owner1 who is also the backup's
// former_owner, with one present backup and a wired fakeRestorer. Subtests
// override only what they need. The focus is the layered authorization (owner +
// former-owner), the stopped gate, the 404/503 surface, and that restore is an
// async 202 kick-off.
func TestRestoreBackup(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}

	mk := func() (*API, *fakeRepo, *fakeCluster, *fakeRestorer) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		repo.backups = []fakeBackup{
			{view: BackupView{ID: "bk1", ServerName: "survival", FormerOwner: "owner1",
				Status: "present", Reason: "inactive_15d", SizeBytes: 1024,
				CreatedAt: time.Unix(1_699_000_000, 0), ExpiresAt: time.Unix(1_706_000_000, 0)}, ref: "world-archive-ref"},
		}
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped",
			Ready: false, DesiredState: string(v1alpha1.DesiredStopped)}
		restorer := &fakeRestorer{}
		api := newTestAPI(repo, cl)
		api.Restorer = restorer
		return api, repo, cl, restorer
	}

	const path = "/api/v1/servers/survival/restore-backup"

	t.Run("former owner restores -> 202 + restorer + audit", func(t *testing.T) {
		api, repo, _, restorer := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Name     string `json:"name"`
			Status   string `json:"status"`
			BackupID string `json:"backup_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		if resp.Name != "survival" || resp.Status != "restoring" || resp.BackupID != "bk1" {
			t.Fatalf("unexpected response %+v", resp)
		}
		if restorer.calls != 1 || restorer.gotName != "survival" || restorer.gotRef != "world-archive-ref" {
			t.Fatalf("restorer saw (calls=%d,%q,%q), want (1,survival,world-archive-ref)",
				restorer.calls, restorer.gotName, restorer.gotRef)
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "backup.restore" || repo.audits[0].Actor != "owner1@example.net" {
			t.Fatalf("audit not written as expected: %+v", repo.audits)
		}
	})

	t.Run("admin restores another's world -> 202", func(t *testing.T) {
		api, _, _, restorer := mk()
		api.External = staticExternal{p: &Principal{UserID: "admin1", Email: "admin1@example.net",
			Role: "admin", ViaAdminAccess: true}}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		if restorer.calls != 1 {
			t.Fatalf("admin restore reached restorer %d times, want 1", restorer.calls)
		}
	})

	t.Run("non-owner -> 403, no restore", func(t *testing.T) {
		api, _, _, restorer := mk()
		api.External = staticExternal{p: &Principal{UserID: "stranger", Role: "user"}}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if restorer.calls != 0 {
			t.Fatal("a forbidden caller must not start a restore")
		}
	})

	t.Run("current owner who is not former_owner -> 403, no restore", func(t *testing.T) {
		// The leak guard: user "newowner" re-claimed survival (so they pass the
		// owner gate), but the backup belongs to "olduser". They must NOT be able to
		// resurrect another person's world.
		api, repo, _, restorer := mk()
		repo.byName["survival"].OwnerID = "newowner"
		repo.backups[0].view.FormerOwner = "olduser"
		api.External = staticExternal{p: &Principal{UserID: "newowner", Role: "user"}}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403 (former-owner leak guard)", w.Code)
		}
		if restorer.calls != 0 {
			t.Fatal("restoring another owner's world must not reach the restorer")
		}
	})

	t.Run("unknown server -> 404", func(t *testing.T) {
		api, _, _, _ := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/missing/restore-backup", "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})

	t.Run("no present backup -> 404 no_backup", func(t *testing.T) {
		api, repo, _, restorer := mk()
		repo.backups = nil
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusNotFound || decodeErr(t, w) != "no_backup" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if restorer.calls != 0 {
			t.Fatal("no backup must not reach the restorer")
		}
	})

	t.Run("only a deleted backup -> 404 no_backup", func(t *testing.T) {
		api, repo, _, _ := mk()
		repo.backups[0].view.Status = "deleted"
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusNotFound || decodeErr(t, w) != "no_backup" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})

	t.Run("running server -> 409 not_stopped, no restore", func(t *testing.T) {
		api, _, cl, restorer := mk()
		cl.byName["survival"].Ready = true
		cl.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "not_stopped" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if restorer.calls != 0 {
			t.Fatal("a running server must not be restored into")
		}
	})

	t.Run("starting server -> 409 not_stopped", func(t *testing.T) {
		// desiredState=Running but not yet Ready: the world PVC is already mounted by
		// the starting pod, so the stopped gate must still refuse.
		api, _, cl, _ := mk()
		cl.byName["survival"].Ready = false
		cl.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "not_stopped" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})

	t.Run("nil Restorer -> 503 restore_unavailable", func(t *testing.T) {
		api, _, _, _ := mk()
		api.Restorer = nil
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "restore_unavailable" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})

	t.Run("restorer failure -> 500, not audited", func(t *testing.T) {
		api, repo, _, restorer := mk()
		restorer.err = errors.New("kick-off failed")
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d, want 500 (%s)", w.Code, w.Body.String())
		}
		if len(repo.audits) != 0 {
			t.Fatalf("a failed restore must not be audited: %+v", repo.audits)
		}
	})

	t.Run("restorer reports unknown server -> 404", func(t *testing.T) {
		api, _, _, restorer := mk()
		restorer.err = ErrNotFound
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})

	t.Run("invalid server name -> 400", func(t *testing.T) {
		api, _, _, _ := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/X/restore-backup", "", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})

	// ---- restore by backup_id ----

	// mkTwo supplements the base mk with two present backups for the same server
	// so tests can exercise restoring the older one by id. bk2 is older than bk1,
	// so LatestBackup still returns bk1 — restoring by "bk2" proves it reached the
	// correct record.
	mkTwo := func() (*API, *fakeRepo, *fakeCluster, *fakeRestorer) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		repo.backups = []fakeBackup{
			{view: BackupView{ID: "bk1", ServerName: "survival", FormerOwner: "owner1",
				Status: "present", Reason: "inactive_15d", SizeBytes: 1024,
				CreatedAt: time.Unix(1_699_000_000, 0), ExpiresAt: time.Unix(1_706_000_000, 0)}, ref: "ref-bk1"},
			{view: BackupView{ID: "bk2", ServerName: "survival", FormerOwner: "owner1",
				Status: "present", Reason: "manual", SizeBytes: 2048,
				CreatedAt: time.Unix(1_698_000_000, 0), ExpiresAt: time.Unix(1_706_000_000, 0)}, ref: "ref-bk2"},
		}
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped",
			Ready: false, DesiredState: string(v1alpha1.DesiredStopped)}
		restorer := &fakeRestorer{}
		api := newTestAPI(repo, cl)
		api.Restorer = restorer
		return api, repo, cl, restorer
	}

	jsonHeaders := map[string]string{"Content-Type": "application/json"}

	t.Run("restore by backup_id -> 202, correct BackupRef sent", func(t *testing.T) {
		api, _, _, restorer := mkTwo()
		api.External = staticExternal{p: owner}
		body := `{"backup_id":"bk2"}`
		w := do(api.ExternalHandler(), "POST", path, body, jsonHeaders)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Name     string `json:"name"`
			Status   string `json:"status"`
			BackupID string `json:"backup_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		if resp.BackupID != "bk2" {
			t.Fatalf("backup_id = %q, want bk2", resp.BackupID)
		}
		if restorer.gotRef != "ref-bk2" {
			t.Fatalf("restorer ref = %q, want ref-bk2 (proves BackupByID, not LatestBackup)", restorer.gotRef)
		}
	})

	t.Run("restore by backup_id not found -> 404 no_backup", func(t *testing.T) {
		api, _, _, restorer := mkTwo()
		api.External = staticExternal{p: owner}
		body := `{"backup_id":"nonexistent"}`
		w := do(api.ExternalHandler(), "POST", path, body, jsonHeaders)
		if w.Code != http.StatusNotFound || decodeErr(t, w) != "no_backup" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if restorer.calls != 0 {
			t.Fatal("non-existent backup must not reach the restorer")
		}
	})

	t.Run("restore by backup_id that is deleted -> 404 no_backup", func(t *testing.T) {
		api, repo, _, _ := mkTwo()
		repo.backups[1].view.Status = "deleted"
		api.External = staticExternal{p: owner}
		body := `{"backup_id":"bk2"}`
		w := do(api.ExternalHandler(), "POST", path, body, jsonHeaders)
		if w.Code != http.StatusNotFound || decodeErr(t, w) != "no_backup" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})

	t.Run("restore by backup_id cross-server -> 403", func(t *testing.T) {
		api, repo, _, _ := mkTwo()
		// bk2 belongs to a different server; restoring it onto survival is forbidden.
		repo.backups[1].view.ServerName = "creative"
		api.External = staticExternal{p: owner}
		body := `{"backup_id":"bk2"}`
		w := do(api.ExternalHandler(), "POST", path, body, jsonHeaders)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403 (cross-server guard)", w.Code)
		}
	})

	t.Run("no body -> falls back to LatestBackup (backward compat)", func(t *testing.T) {
		api, _, _, restorer := mkTwo()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		if restorer.gotRef != "ref-bk1" {
			t.Fatalf("restorer ref = %q, want ref-bk1 (LatestBackup)", restorer.gotRef)
		}
	})

	t.Run("empty JSON body -> falls back to LatestBackup", func(t *testing.T) {
		api, _, _, restorer := mkTwo()
		api.External = staticExternal{p: owner}
		body := `{}`
		w := do(api.ExternalHandler(), "POST", path, body, jsonHeaders)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		if restorer.gotRef != "ref-bk1" {
			t.Fatalf("restorer ref = %q, want ref-bk1 (LatestBackup)", restorer.gotRef)
		}
	})

	t.Run("malformed JSON body -> 400", func(t *testing.T) {
		api, _, _, _ := mkTwo()
		api.External = staticExternal{p: owner}
		body := `not json`
		w := do(api.ExternalHandler(), "POST", path, body, jsonHeaders)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
		}
	})
}
