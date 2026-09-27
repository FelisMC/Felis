package api

import (
	"context"
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

	admin := &Principal{UserID: "admin1", Role: "admin", ViaAdminAccess: true}
	total := func(t *testing.T, w *httptest.ResponseRecorder) int {
		t.Helper()
		var resp struct {
			Total *int `json:"total"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Total == nil {
			t.Fatalf("body carries no total: %v (%s)", err, w.Body.String())
		}
		return *resp.Total
	}

	t.Run("server filter narrows inside the scope, never past it", func(t *testing.T) {
		api := mk()
		api.External = staticExternal{p: admin}
		w := do(api.ExternalHandler(), "GET", "/api/v1/backups?server=beta", "", nil)
		if got := ids(list(t, w)); !got["b2"] || len(got) != 1 || total(t, w) != 1 {
			t.Fatalf("admin ?server=beta = %v (total %d), want {b2} of 1", got, total(t, w))
		}
		api.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
		w = do(api.ExternalHandler(), "GET", "/api/v1/backups?server=beta", "", nil)
		if got := ids(list(t, w)); len(got) != 0 || total(t, w) != 0 {
			t.Fatalf("owner1 ?server=beta = %v (total %d), want nothing: beta's backup is owner2's", got, total(t, w))
		}
	})

	t.Run("pages newest first with the match total", func(t *testing.T) {
		api := mk()
		api.External = staticExternal{p: admin}
		w := do(api.ExternalHandler(), "GET", "/api/v1/backups?limit=1", "", nil)
		if vs := list(t, w); len(vs) != 1 || vs[0].ID != "b2" || total(t, w) != 2 {
			t.Fatalf("?limit=1 = %+v (total %d), want [b2] of 2", vs, total(t, w))
		}
		w = do(api.ExternalHandler(), "GET", "/api/v1/backups?limit=1&offset=1", "", nil)
		if vs := list(t, w); len(vs) != 1 || vs[0].ID != "b1" {
			t.Fatalf("?limit=1&offset=1 = %+v, want [b1]", vs)
		}
	})

	t.Run("page bounds", func(t *testing.T) {
		cases := []struct {
			query string
			want  BackupListOpts
		}{
			{"", BackupListOpts{Limit: 20}},
			{"?limit=5000&offset=-3", BackupListOpts{Limit: 100}},
			{"?limit=x&offset=40&server=alpha", BackupListOpts{Server: "alpha", Limit: 20, Offset: 40}},
		}
		for _, c := range cases {
			repo := newFakeRepo()
			api := newTestAPI(repo, newFakeCluster())
			api.External = staticExternal{p: admin}
			if w := do(api.ExternalHandler(), "GET", "/api/v1/backups"+c.query, "", nil); w.Code != http.StatusOK {
				t.Fatalf("%q: code = %d (%s)", c.query, w.Code, w.Body.String())
			}
			if repo.backupListOpts != c.want {
				t.Errorf("%q asked the repo for %+v, want %+v", c.query, repo.backupListOpts, c.want)
			}
		}
	})

	t.Run("malformed server name is a 400", func(t *testing.T) {
		api := mk()
		api.External = staticExternal{p: admin}
		w := do(api.ExternalHandler(), "GET", "/api/v1/backups?server=Bad%20Name", "", nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_name") {
			t.Fatalf("code = %d (%s), want 400 bad_name", w.Code, w.Body.String())
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

	t.Run("former owner after release -> 403, no restore", func(t *testing.T) {
		// Mirror of the guard above, pinning the owner gate rather than the former-owner
		// gate (handler comment: "must re-claim first"). owner1 took this backup, then
		// released survival to "newowner". owner1 is still the backup's former_owner — so
		// the former-owner gate would wave them through — but is no longer the current
		// owner. Only the owner gate stops them; without it a superseded owner could roll
		// a live server back onto their old world. (Disable that gate and this is the one
		// subtest that reddens — the former-owner gate does not backstop this case.)
		api, repo, _, restorer := mk()
		repo.byName["survival"].OwnerID = "newowner"
		api.External = staticExternal{p: owner} // owner1: former_owner, not current owner
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403 (owner gate: former owner is no longer the current owner)", w.Code)
		}
		if restorer.calls != 0 {
			t.Fatal("a released former owner must not restore onto the current owner's server")
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

	t.Run("no world volume -> 409 no_world_volume, no restore", func(t *testing.T) {
		// Restoring into a missing world PVC would leave the Job Pending on the
		// missing claim — a 202 "restoring" that never writes anything.
		api, _, cl, restorer := mk()
		cl.noWorld["survival"] = true
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "no_world_volume" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if restorer.calls != 0 {
			t.Fatal("a world-less server must not reach the restorer")
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

	t.Run("restore by backup_id that failed a read-back -> 409 backup_corrupt", func(t *testing.T) {
		api, repo, _, restorer := mkTwo()
		repo.backups[1].view.Corrupt = true
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, `{"backup_id":"bk2"}`, jsonHeaders)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "backup_corrupt" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if restorer.calls != 0 {
			t.Fatal("a corrupt backup reached the restorer")
		}
	})

	t.Run("no body skips a latest backup that failed a read-back", func(t *testing.T) {
		api, repo, _, restorer := mkTwo()
		repo.backups[0].view.Corrupt = true
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusAccepted || restorer.gotRef != "ref-bk2" {
			t.Fatalf("code = %d ref %q, want 202 restoring the newest intact backup", w.Code, restorer.gotRef)
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

// TestDeleteBackup covers DELETE /api/v1/backups/{id}: the scope that lists a
// backup deletes it, the row turns expired (out of lists, restores and the
// budget, expires_at pulled to now so the reaper and the off-site sync take it
// next), an out-of-scope id reads as unknown, and a restore that may be reading
// the archive holds the delete off.
func TestDeleteBackup(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}
	admin := &Principal{UserID: "admin1", Email: "admin1@example.net", Role: "admin", ViaAdminAccess: true}
	expires := time.Unix(1_706_000_000, 0)

	mk := func(p *Principal) (*API, *fakeRepo) {
		repo := newFakeRepo()
		repo.backups = []fakeBackup{
			{view: BackupView{ID: "bk1", ServerName: "survival", FormerOwner: "owner1",
				Status: "present", Reason: "manual", SizeBytes: 1024,
				CreatedAt: time.Unix(1_699_000_000, 0), ExpiresAt: expires}, ref: "ref-bk1"},
			{view: BackupView{ID: "bk2", ServerName: "creative", FormerOwner: "owner2",
				Status: "present", Reason: "inactive_15d", SizeBytes: 2048,
				CreatedAt: time.Unix(1_699_500_000, 0), ExpiresAt: expires}, ref: "ref-bk2"},
		}
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: p}
		return api, repo
	}
	del := func(api *API, id string) *httptest.ResponseRecorder {
		return do(api.ExternalHandler(), "DELETE", "/api/v1/backups/"+id, "", nil)
	}
	status := func(repo *fakeRepo, id string) string {
		for _, b := range repo.backups {
			if b.view.ID == id {
				return b.view.Status
			}
		}
		return ""
	}

	t.Run("former owner deletes -> 200, expired, audited", func(t *testing.T) {
		api, repo := mk(owner)
		w := del(api, "bk1")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		if len(resp) != 2 || resp["id"] != "bk1" || resp["status"] != "expired" {
			t.Fatalf("body = %v, want {id: bk1, status: expired}", resp)
		}
		b := repo.backups[0].view
		if b.Status != "expired" || !b.ExpiresAt.Equal(api.now()) {
			t.Fatalf("row = %s expiring %v, want expired expiring %v", b.Status, b.ExpiresAt, api.now())
		}
		if status(repo, "bk2") != "present" {
			t.Fatal("deleting bk1 touched bk2")
		}
		if n, _ := repo.BackupStoreBytes(t.Context()); n != 2048 {
			t.Fatalf("backup budget counts %d bytes, want 2048", n)
		}
		lw := do(api.ExternalHandler(), "GET", "/api/v1/backups", "", nil)
		if strings.Contains(lw.Body.String(), `"bk1"`) {
			t.Fatalf("deleted backup still listed: %s", lw.Body.String())
		}
		if len(repo.audits) != 1 {
			t.Fatalf("audits = %+v, want one", repo.audits)
		}
		a := repo.audits[0]
		if a.Action != "backup.delete" || a.Actor != "owner1@example.net" || a.ActorUserID != "owner1" || a.ServerName != "survival" {
			t.Fatalf("audit = %+v", a)
		}
		var payload map[string]any
		if err := json.Unmarshal(a.Payload, &payload); err != nil {
			t.Fatalf("payload not JSON: %v (%s)", err, a.Payload)
		}
		if payload["backup_id"] != "bk1" || payload["former_owner"] != "owner1" || payload["size_bytes"] != float64(1024) {
			t.Fatalf("payload = %v", payload)
		}
		if w := del(api, "bk1"); w.Code != http.StatusNotFound || decodeErr(t, w) != "no_backup" {
			t.Fatalf("second delete: code = %d body %s, want 404 no_backup", w.Code, w.Body.String())
		}
	})

	t.Run("expires_at already past stays put", func(t *testing.T) {
		api, repo := mk(owner)
		past := api.now().Add(-time.Hour)
		repo.backups[0].view.ExpiresAt = past
		if w := del(api, "bk1"); w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if got := repo.backups[0].view.ExpiresAt; !got.Equal(past) {
			t.Fatalf("expires_at = %v, want %v", got, past)
		}
	})

	t.Run("admin deletes another's backup -> 200", func(t *testing.T) {
		api, repo := mk(admin)
		if w := del(api, "bk2"); w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		if status(repo, "bk2") != "expired" {
			t.Fatalf("bk2 = %s, want expired", status(repo, "bk2"))
		}
	})

	for _, tc := range []struct {
		name string
		p    *Principal
		id   string
		prep func(*fakeRepo)
	}{
		{"another user's backup", owner, "bk2", nil},
		{"unknown id", admin, "nope", nil},
		{"already deleted", admin, "bk1", func(r *fakeRepo) { r.backups[0].view.Status = "deleted" }},
		{"already expired", owner, "bk1", func(r *fakeRepo) { r.backups[0].view.Status = "expired" }},
		{"no user id against no former owner", &Principal{Role: "user"}, "bk1",
			func(r *fakeRepo) { r.backups[0].view.FormerOwner = "" }},
	} {
		t.Run(tc.name+" -> 404 no_backup", func(t *testing.T) {
			api, repo := mk(tc.p)
			if tc.prep != nil {
				tc.prep(repo)
			}
			before := []string{status(repo, "bk1"), status(repo, "bk2")}
			w := del(api, tc.id)
			if w.Code != http.StatusNotFound || decodeErr(t, w) != "no_backup" {
				t.Fatalf("code = %d body %s, want 404 no_backup", w.Code, w.Body.String())
			}
			if after := []string{status(repo, "bk1"), status(repo, "bk2")}; after[0] != before[0] || after[1] != before[1] {
				t.Fatalf("statuses %v -> %v, want unchanged", before, after)
			}
			if len(repo.audits) != 0 {
				t.Fatalf("refused delete audited: %+v", repo.audits)
			}
		})
	}

	for _, tc := range []struct {
		name string
		jobs []AsyncJob
		want int
	}{
		{"restore running", []AsyncJob{{Kind: "restore", State: "running"}}, http.StatusConflict},
		{"snapshot before restoring this backup", []AsyncJob{{Kind: "backup", State: "running",
			ThenRestore: "pending", RestoreBackupID: "bk1"}}, http.StatusConflict},
		{"snapshot done, restore of this backup still to start", []AsyncJob{{Kind: "backup", State: "succeeded",
			ThenRestore: "pending", RestoreBackupID: "bk1"}}, http.StatusConflict},
		{"snapshot before restoring another backup", []AsyncJob{{Kind: "backup", State: "running",
			ThenRestore: "pending", RestoreBackupID: "bk9"}}, http.StatusOK},
		{"restore of this backup started and finished", []AsyncJob{
			{Kind: "restore", State: "succeeded"},
			{Kind: "backup", State: "succeeded", ThenRestore: "started", RestoreBackupID: "bk1"}}, http.StatusOK},
		{"chain abandoned", []AsyncJob{{Kind: "backup", State: "failed",
			ThenRestore: "abandoned", RestoreBackupID: "bk1"}}, http.StatusOK},
		{"restore failed", []AsyncJob{{Kind: "restore", State: "failed"}}, http.StatusOK},
		{"plain backup running", []AsyncJob{{Kind: "backup", State: "running"}}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, repo := mk(owner)
			js := &fakeJobStatus{jobs: tc.jobs}
			api.JobStatus = js
			w := del(api, "bk1")
			if w.Code != tc.want {
				t.Fatalf("code = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
			if js.got != "survival" {
				t.Fatalf("jobs read for %q, want survival", js.got)
			}
			want := "expired"
			if tc.want == http.StatusConflict {
				want = "present"
				if decodeErr(t, w) != "restore_in_progress" {
					t.Fatalf("error code %q, want restore_in_progress", decodeErr(t, w))
				}
			}
			if status(repo, "bk1") != want {
				t.Fatalf("bk1 = %s, want %s", status(repo, "bk1"), want)
			}
		})
	}

	t.Run("deleted by someone else after the lookup -> 404, not audited", func(t *testing.T) {
		api, repo := mk(owner)
		api.Repo = expireFails{repo, ErrNotFound}
		if w := del(api, "bk1"); w.Code != http.StatusNotFound || decodeErr(t, w) != "no_backup" {
			t.Fatalf("code = %d body %s, want 404 no_backup", w.Code, w.Body.String())
		}
		if len(repo.audits) != 0 {
			t.Fatalf("lost race audited: %+v", repo.audits)
		}
	})

	t.Run("expire fails -> 500, not audited", func(t *testing.T) {
		api, repo := mk(owner)
		api.Repo = expireFails{repo, errors.New("connection reset")}
		if w := del(api, "bk1"); w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d, want 500 (%s)", w.Code, w.Body.String())
		}
		if len(repo.audits) != 0 {
			t.Fatalf("failed delete audited: %+v", repo.audits)
		}
	})

	t.Run("job status error -> 500, kept", func(t *testing.T) {
		api, repo := mk(owner)
		api.JobStatus = &fakeJobStatus{err: errors.New("apiserver down")}
		if w := del(api, "bk1"); w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d, want 500 (%s)", w.Code, w.Body.String())
		}
		if status(repo, "bk1") != "present" || len(repo.audits) != 0 {
			t.Fatalf("bk1 = %s audits %d, want present and none", status(repo, "bk1"), len(repo.audits))
		}
	})
}

// expireFails is a repo whose ExpireBackup answers err after the handler's
// lookup found the backup: ErrNotFound is a concurrent delete winning.
type expireFails struct {
	*fakeRepo
	err error
}

func (f expireFails) ExpireBackup(context.Context, string, time.Time) error { return f.err }
