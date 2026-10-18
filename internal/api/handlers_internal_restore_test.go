package api

import (
	"net/http"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

func TestInternalRestoreRebuildsAWorld(t *testing.T) {
	const path = "/api/v1/internal/servers/survival/restore"
	mk := func() (*API, *fakeRepo, *fakeCluster, *fakeRestorer) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		old, newest := time.Unix(1_700_000_000, 0), time.Unix(1_700_100_000, 0)
		repo.backups = []fakeBackup{
			{view: BackupView{ID: "bk-old", ServerName: "survival", Status: "present", CreatedAt: old}, ref: "/backups/old.tar.gz"},
			{view: BackupView{ID: "bk-new", ServerName: "survival", Status: "present", CreatedAt: newest}, ref: "/backups/new.tar.gz"},
			{view: BackupView{ID: "bk-bad", ServerName: "survival", Status: "present", CreatedAt: newest.Add(time.Hour), Corrupt: true}, ref: "/backups/bad.tar.gz"},
		}
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped", DesiredState: string(v1alpha1.DesiredStopped)}
		cl.noWorld["survival"] = true
		restorer := &fakeRestorer{}
		a := newTestAPI(repo, cl)
		a.Internal = okInternal{caller: CallerOps}
		a.Restorer = restorer
		return a, repo, cl, restorer
	}

	a, repo, cl, restorer := mk()
	w := do(a.InternalHandler(), "POST", path, "", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("restore = %d %s", w.Code, w.Body.String())
	}
	if restorer.calls != 1 || restorer.gotRef != "/backups/new.tar.gz" {
		t.Fatalf("restorer saw %d calls, ref %q; want the newest intact backup", restorer.calls, restorer.gotRef)
	}
	if len(cl.ensured) != 1 || cl.ensured[0] != "survival" {
		t.Errorf("world volume ensured for %v", cl.ensured)
	}
	if len(cl.acquired) != 1 || cl.acquired[0] != "survival:restore" {
		t.Errorf("world lock = %v", cl.acquired)
	}
	if len(repo.audits) != 1 || repo.audits[0].Action != "backup.restore" || repo.audits[0].Actor != "restore-host" {
		t.Errorf("audit = %+v", repo.audits)
	}

	a, _, cl, restorer = mk()
	cl.byName["survival"].Ready, cl.byName["survival"].DesiredState = true, string(v1alpha1.DesiredRunning)
	if w = do(a.InternalHandler(), "POST", path, "", nil); w.Code != http.StatusConflict || restorer.calls != 0 || len(cl.ensured) != 0 {
		t.Errorf("a running server = %d %s (restores %d, ensured %v)", w.Code, w.Body.String(), restorer.calls, cl.ensured)
	}

	a, repo, _, restorer = mk()
	repo.backups = nil
	if w = do(a.InternalHandler(), "POST", path, "", nil); w.Code != http.StatusNotFound || decodeErr(t, w) != "no_backup" || restorer.calls != 0 {
		t.Errorf("no backup = %d %s", w.Code, w.Body.String())
	}

	a, _, _, _ = mk()
	if w = do(a.InternalHandler(), "POST", "/api/v1/internal/servers/ghost/restore", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("an unknown server = %d %s", w.Code, w.Body.String())
	}
}
