package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

// fakeBackuper records the on-demand backup kick-off the handler makes. It mirrors
// fakeRestorer: the handler only STARTS the work, so the fake just captures the
// arguments and returns a canned error.
type fakeBackuper struct {
	err          error
	calls        int
	gotName      string
	gotFormerOwn string
}

func (f *fakeBackuper) Backup(_ context.Context, name, formerOwner string) error {
	f.calls++
	f.gotName, f.gotFormerOwn = name, formerOwner
	return f.err
}

// TestBackupNow exercises POST /api/v1/servers/{name}/backup (spec §18/§19 WorldArchiver,
// on demand). The default server is stopped and owned by owner1 with a wired
// fakeBackuper. The focus is the owner-or-admin gate, the stopped gate (the RWO world
// PVC must be free), the 404/503 surface, that the recorded former owner is the
// server's CURRENT owner, and that backup is an async 202 kick-off.
func TestBackupNow(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}

	mk := func() (*API, *fakeRepo, *fakeCluster, *fakeBackuper) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped",
			Ready: false, DesiredState: string(v1alpha1.DesiredStopped)}
		backuper := &fakeBackuper{}
		api := newTestAPI(repo, cl)
		api.Backuper = backuper
		return api, repo, cl, backuper
	}

	const path = "/api/v1/servers/survival/backup"

	t.Run("owner backs up -> 202 + backuper(currentOwner) + audit", func(t *testing.T) {
		api, repo, _, backuper := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		if resp.Name != "survival" || resp.Status != "backing_up" {
			t.Fatalf("unexpected response %+v", resp)
		}
		if backuper.calls != 1 || backuper.gotName != "survival" || backuper.gotFormerOwn != "owner1" {
			t.Fatalf("backuper saw (calls=%d,%q,%q), want (1,survival,owner1)",
				backuper.calls, backuper.gotName, backuper.gotFormerOwn)
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "backup.create" || repo.audits[0].Actor != "owner1@example.net" {
			t.Fatalf("audit not written as expected: %+v", repo.audits)
		}
	})

	t.Run("admin backs up an unowned server -> 202, empty former owner recorded", func(t *testing.T) {
		api, repo, _, backuper := mk()
		repo.byName["survival"].OwnerID = "" // released world
		api.External = staticExternal{p: &Principal{UserID: "admin1", Email: "admin1@example.net",
			Role: "admin", ViaAdminAccess: true}}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		if backuper.calls != 1 || backuper.gotFormerOwn != "" {
			t.Fatalf("backuper saw (calls=%d, former=%q), want (1, empty)", backuper.calls, backuper.gotFormerOwn)
		}
	})

	t.Run("non-owner -> 403, no backup", func(t *testing.T) {
		api, _, _, backuper := mk()
		api.External = staticExternal{p: &Principal{UserID: "stranger", Role: "user"}}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if backuper.calls != 0 {
			t.Fatal("a forbidden caller must not start a backup")
		}
	})

	t.Run("unknown server -> 404", func(t *testing.T) {
		api, _, _, _ := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/missing/backup", "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})

	t.Run("running server -> 409 not_stopped, no backup", func(t *testing.T) {
		api, _, cl, backuper := mk()
		cl.byName["survival"].Ready = true
		cl.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "not_stopped" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if backuper.calls != 0 {
			t.Fatal("a running server holds the RWO world PVC — backup must be refused")
		}
	})

	t.Run("starting server -> 409 not_stopped", func(t *testing.T) {
		api, _, cl, _ := mk()
		cl.byName["survival"].Ready = false
		cl.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "not_stopped" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})

	t.Run("no world volume -> 409 no_world_volume, no backup", func(t *testing.T) {
		// A never-started (or reaped) server has no world PVC: the Job would hang
		// Pending on the missing claim with nothing recorded, so the gate must
		// refuse before the backuper is reached.
		api, _, cl, backuper := mk()
		cl.noWorld["survival"] = true
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "no_world_volume" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if backuper.calls != 0 {
			t.Fatal("a world-less server must not reach the backuper")
		}
	})

	t.Run("nil Backuper -> 503 backup_unavailable", func(t *testing.T) {
		api, _, _, _ := mk()
		api.Backuper = nil
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "backup_unavailable" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})

	t.Run("backuper failure -> 500, not audited", func(t *testing.T) {
		api, repo, _, backuper := mk()
		backuper.err = errors.New("kick-off failed")
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d, want 500 (%s)", w.Code, w.Body.String())
		}
		if len(repo.audits) != 0 {
			t.Fatalf("a failed backup must not be audited: %+v", repo.audits)
		}
	})

	t.Run("backuper reports unknown server -> 404", func(t *testing.T) {
		api, _, _, backuper := mk()
		backuper.err = ErrNotFound
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})

	t.Run("invalid server name -> 400", func(t *testing.T) {
		api, _, _, _ := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/X/backup", "", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})

	// data-durability-9: an owner's backups are rationed per server; an admin's
	// are not.
	t.Run("owner inside the cooldown -> 429 backup_cooldown with Retry-After", func(t *testing.T) {
		api, _, _, backuper := mk()
		api.BackupCooldown = 10 * time.Minute
		api.Now = time.Now // the fake stamps backup.create audits with the wall clock
		api.External = staticExternal{p: owner}
		if w := do(api.ExternalHandler(), "POST", path, "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("first backup: code = %d (%s)", w.Code, w.Body.String())
		}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "backup_cooldown" {
			t.Fatalf("second backup: code = %d body %s", w.Code, w.Body.String())
		}
		if ra, _ := strconv.Atoi(w.Header().Get("Retry-After")); ra < 590 || ra > 600 {
			t.Fatalf("Retry-After = %q, want about 600", w.Header().Get("Retry-After"))
		}
		if backuper.calls != 1 {
			t.Fatalf("backuper called %d times, want 1", backuper.calls)
		}
	})

	t.Run("cooldown elapsed -> 202", func(t *testing.T) {
		api, repo, _, _ := mk()
		api.BackupCooldown = 10 * time.Minute
		api.Now = time.Now
		repo.backupRequested = map[string]time.Time{"survival": time.Now().Add(-11 * time.Minute)}
		api.External = staticExternal{p: owner}
		if w := do(api.ExternalHandler(), "POST", path, "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("admin bypasses the cooldown and the store cap", func(t *testing.T) {
		api, repo, _, backuper := mk()
		api.BackupCooldown = 10 * time.Minute
		api.BackupStoreCap = 100
		api.Now = time.Now
		repo.backupRequested = map[string]time.Time{"survival": time.Now()}
		repo.backups = []fakeBackup{{view: BackupView{ID: "b1", ServerName: "other", Status: "present", SizeBytes: 500}}}
		api.External = staticExternal{p: &Principal{UserID: "admin1", Email: "admin1@example.net",
			Role: "admin", ViaAdminAccess: true}}
		if w := do(api.ExternalHandler(), "POST", path, "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if backuper.calls != 1 {
			t.Fatal("the admin's backup did not start")
		}
	})

	t.Run("owner with the store at its cap -> 507 backup_store_full", func(t *testing.T) {
		api, repo, _, backuper := mk()
		api.BackupStoreCap = 1000
		repo.backups = []fakeBackup{
			{view: BackupView{ID: "b1", ServerName: "other", Status: "present", SizeBytes: 600}},
			{view: BackupView{ID: "b2", ServerName: "survival", Status: "present", SizeBytes: 400}},
			{view: BackupView{ID: "b3", ServerName: "survival", Status: "deleted", SizeBytes: 9000}},
		}
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", path, "", nil)
		if w.Code != http.StatusInsufficientStorage || decodeErr(t, w) != "backup_store_full" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if backuper.calls != 0 {
			t.Fatal("a full store still started a backup")
		}
		repo.backups[0].view.Status = "deleted"
		if w := do(api.ExternalHandler(), "POST", path, "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("below the cap: code = %d (%s)", w.Code, w.Body.String())
		}
	})
}

// TestInternalBackup exercises POST /api/v1/internal/servers/{name}/backup, the
// break-glass console's face. It shares enqueueBackup with the external handler, so
// the stopped-gate / 503 / async-202 behaviour is proven there; here the focus is the
// internal-face difference: no Principal (service-token auth), no owner gate — even a
// server owned by someone else backs up (the on-node operator is trusted) — and the
// audit is attributed to "break-glass"/"internal", not an email/"external".
func TestInternalBackup(t *testing.T) {
	mk := func() (*API, *fakeRepo, *fakeCluster, *fakeBackuper) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "someone-else"}
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped",
			Ready: false, DesiredState: string(v1alpha1.DesiredStopped)}
		backuper := &fakeBackuper{}
		api := newTestAPI(repo, cl)
		api.Backuper = backuper
		return api, repo, cl, backuper
	}

	const path = "/api/v1/internal/servers/survival/backup"

	t.Run("stopped server -> 202 + backuper(currentOwner) + break-glass audit", func(t *testing.T) {
		api, repo, _, backuper := mk()
		w := do(api.InternalHandler(), "POST", path, "", jsonHeader)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		// No owner gate on the internal face: a server owned by someone else still backs
		// up, and the recorded former owner is the server's CURRENT owner.
		if backuper.calls != 1 || backuper.gotName != "survival" || backuper.gotFormerOwn != "someone-else" {
			t.Fatalf("backuper saw (calls=%d,%q,%q), want (1,survival,someone-else)",
				backuper.calls, backuper.gotName, backuper.gotFormerOwn)
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "backup.create" ||
			repo.audits[0].Actor != "break-glass" || repo.audits[0].Source != "internal" {
			t.Fatalf("audit not attributed to break-glass/internal: %+v", repo.audits)
		}
	})

	t.Run("os_user body attributes the audit to the operator", func(t *testing.T) {
		api, repo, _, _ := mk()
		w := do(api.InternalHandler(), "POST", path, `{"os_user":"alice"}`, jsonHeader)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		if len(repo.audits) != 1 || repo.audits[0].Actor != "alice" || repo.audits[0].Source != "internal" {
			t.Fatalf("audit actor should be the os_user, not break-glass: %+v", repo.audits)
		}
	})

	t.Run("running server -> 409 not_stopped, no backup", func(t *testing.T) {
		api, _, cl, backuper := mk()
		cl.byName["survival"].Ready = true
		cl.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning)
		w := do(api.InternalHandler(), "POST", path, "", jsonHeader)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "not_stopped" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if backuper.calls != 0 {
			t.Fatal("a running server holds the RWO world PVC — backup must be refused")
		}
	})

	t.Run("no world volume -> 409 no_world_volume, no backup", func(t *testing.T) {
		// The break-glass face shares enqueueBackup, so the world-volume gate must
		// hold here too — this is the face the TUI's Sync picker drives.
		api, _, cl, backuper := mk()
		cl.noWorld["survival"] = true
		w := do(api.InternalHandler(), "POST", path, "", jsonHeader)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "no_world_volume" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if backuper.calls != 0 {
			t.Fatal("a world-less server must not reach the backuper")
		}
	})

	t.Run("nil Backuper -> 503 backup_unavailable", func(t *testing.T) {
		api, _, _, _ := mk()
		api.Backuper = nil
		w := do(api.InternalHandler(), "POST", path, "", jsonHeader)
		if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "backup_unavailable" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})

	t.Run("unknown server -> 404", func(t *testing.T) {
		api, _, _, _ := mk()
		w := do(api.InternalHandler(), "POST", "/api/v1/internal/servers/missing/backup", "", jsonHeader)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})

	t.Run("invalid server name -> 400 bad_name", func(t *testing.T) {
		api, _, _, _ := mk()
		w := do(api.InternalHandler(), "POST", "/api/v1/internal/servers/X/backup", "", jsonHeader)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_name" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
}
