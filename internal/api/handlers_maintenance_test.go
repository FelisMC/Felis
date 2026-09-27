package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/maintenance"
)

// The world-volume lock (internal/maintenance) from the handlers' side: a wake
// refused because a restore/backup/file write holds the world, and those three
// operations refused while another one does. The lock itself (atomicity, stale
// locks, Job-backed holds) is K8sCluster's and is tested in k8scluster_test.

func TestWakeRefusedDuringMaintenance(t *testing.T) {
	busy := &MaintenanceBusyError{Kind: maintenance.KindRestore}

	t.Run("external wake -> 409 maintenance_in_progress, cooldown kept", func(t *testing.T) {
		repo := newFakeRepo()
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", AutostartPolicy: "public"}
		cl.wakeErr["survival"] = busy
		api := newTestAPI(repo, cl)
		api.External = staticExternal{p: &Principal{UserID: "u1", Role: "user"}}

		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "maintenance_in_progress" {
			t.Fatalf("code = %d body %s, want 409 maintenance_in_progress", w.Code, w.Body.String())
		}
		if _, set := cl.desired["survival"]; set {
			t.Fatal("a refused wake must not flip desiredState")
		}

		// The refusal did not burn the cooldown: once the restore is done the very
		// next wake goes through.
		delete(cl.wakeErr, "survival")
		if w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("wake after maintenance: code = %d body %s", w.Code, w.Body.String())
		}
	})

	t.Run("internal wake (velocity) -> 409 maintenance_in_progress, cooldown kept", func(t *testing.T) {
		api, cl := newInternalWakeAPI("public")
		cl.wakeErr["survival"] = busy
		body := `{"mc_uuid":"` + wakeUUID + `"}`

		w := internalWake(api, body)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "maintenance_in_progress" {
			t.Fatalf("code = %d body %s, want 409 maintenance_in_progress", w.Code, w.Body.String())
		}
		delete(cl.wakeErr, "survival")
		if w := internalWake(api, body); w.Code != http.StatusAccepted {
			t.Fatalf("wake after maintenance: code = %d body %s", w.Code, w.Body.String())
		}
	})

	// The idle reaper is no wait-and-retry: when it lets go the world is archived
	// and the server released, so velocity must be able to tell the player that.
	t.Run("internal wake during an idle reap -> 409 world_reclaiming", func(t *testing.T) {
		api, cl := newInternalWakeAPI("public")
		cl.wakeErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindReap}
		w := internalWake(api, `{"mc_uuid":"`+wakeUUID+`"}`)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "world_reclaiming" {
			t.Fatalf("code = %d body %s, want 409 world_reclaiming", w.Code, w.Body.String())
		}
		if _, set := cl.desired["survival"]; set {
			t.Fatal("a refused wake must not flip desiredState")
		}
	})
}

// maintenanceOp is one world-volume operation as the external face serves it.
type maintenanceOp struct {
	name   string
	kind   string
	method string
	path   string
	body   string
	calls  func() int
}

func maintenanceOps(t *testing.T) (*API, *fakeCluster, []maintenanceOp) {
	t.Helper()
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	repo.backups = []fakeBackup{{view: BackupView{ID: "bk1", ServerName: "survival",
		FormerOwner: "owner1", Status: "present"}, ref: "world-archive-ref"}}
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped",
		DesiredState: string(v1alpha1.DesiredStopped)}
	restorer, backuper, files := &fakeRestorer{}, &fakeBackuper{}, &fakeFileEditor{}
	api := newTestAPI(repo, cl)
	api.Restorer, api.Backuper, api.Files = restorer, backuper, files
	api.FileStage = &fileedit.Stage{Dir: t.TempDir(), MinFree: 1e-9}
	api.InternalBaseURL = "http://felis-api-internal.felis.svc.cluster.local:8081"
	api.External = staticExternal{p: &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}}
	return api, cl, []maintenanceOp{
		{"restore", maintenance.KindRestore, "POST", "/api/v1/servers/survival/restore-backup", "",
			func() int { return restorer.calls }},
		{"backup", maintenance.KindBackup, "POST", "/api/v1/servers/survival/backup", "",
			func() int { return backuper.calls }},
		{"file write", maintenance.KindFileWrite, "PUT", "/api/v1/servers/survival/file?path=server.properties",
			`{"content":"aGk="}`, func() int { return files.calls }},
		{"file mkdir", maintenance.KindFileWrite, "POST", "/api/v1/servers/survival/files/mkdir?path=plugins",
			"", func() int { return files.calls }},
		{"file delete", maintenance.KindFileWrite, "DELETE", "/api/v1/servers/survival/file?path=old.jar",
			"", func() int { return files.calls }},
		{"file rename", maintenance.KindFileWrite, "POST", "/api/v1/servers/survival/files/rename?path=a.txt",
			`{"to":"b.txt"}`, func() int { return files.calls }},
		{"file upload", maintenance.KindFileWrite, "PUT", "/api/v1/servers/survival/files/upload?path=plugins/x.jar",
			"PK-jar-bytes", func() int { return files.calls }},
	}
}

func (op maintenanceOp) do(api *API) *httptest.ResponseRecorder {
	hdr := fileRouteHeader(strings.TrimPrefix(op.name, "file "), op.body)
	return do(api.ExternalHandler(), op.method, op.path, op.body, hdr)
}

func TestMaintenanceOpsTakeAndReleaseTheLock(t *testing.T) {
	_, _, ops := maintenanceOps(t)
	for i := range ops {
		t.Run(ops[i].name, func(t *testing.T) {
			api, cl, ops := maintenanceOps(t)
			op := ops[i]
			if w := op.do(api); w.Code/100 != 2 {
				t.Fatalf("code = %d body %s", w.Code, w.Body.String())
			}
			if op.calls() != 1 {
				t.Fatalf("executor calls = %d, want 1", op.calls())
			}
			if want := []string{"survival:" + op.kind}; !slices.Equal(cl.acquired, want) {
				t.Fatalf("acquired = %v, want %v", cl.acquired, want)
			}
			if want := []string{"survival"}; !slices.Equal(cl.released, want) {
				t.Fatalf("released = %v, want %v (the Job is the lock from here on)", cl.released, want)
			}
		})
	}
}

func TestMaintenanceOpsRefusedWhileHeld(t *testing.T) {
	refusals := []struct {
		name string
		err  error
		code string
	}{
		{"another holder", &MaintenanceBusyError{Kind: maintenance.KindBackup}, "maintenance_in_progress"},
		// The snapshot said Stopped but the atomic re-check found it waking: the
		// wake won the race.
		{"server not stopped", fmt.Errorf("wrapped: %w", ErrNotStopped), "not_stopped"},
	}
	_, _, ops := maintenanceOps(t)
	for i := range ops {
		for _, rf := range refusals {
			t.Run(ops[i].name+" / "+rf.name, func(t *testing.T) {
				api, cl, ops := maintenanceOps(t)
				op := ops[i]
				cl.maintErr["survival"] = rf.err
				w := op.do(api)
				if w.Code != http.StatusConflict || decodeErr(t, w) != rf.code {
					t.Fatalf("code = %d body %s, want 409 %s", w.Code, w.Body.String(), rf.code)
				}
				if op.calls() != 0 {
					t.Fatal("a refused operation must not reach its executor")
				}
				if len(cl.released) != 0 {
					t.Fatalf("released %v a lock that was never taken", cl.released)
				}
			})
		}
	}
}

func TestMaintenanceErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{&MaintenanceBusyError{Kind: maintenance.KindFileWrite}, "maintenance_in_progress"},
		{fmt.Errorf("x: %w", ErrMaintenanceInProgress), "maintenance_in_progress"},
		{ErrNotStopped, "not_stopped"},
	} {
		var ae *apiError
		if !errors.As(maintenanceError(tc.err, "stop it"), &ae) || ae.code != tc.code || ae.status != http.StatusConflict {
			t.Errorf("%v -> %+v, want 409 %s", tc.err, ae, tc.code)
		}
	}
	other := errors.New("boom")
	if got := maintenanceError(other, "stop it"); got != other {
		t.Errorf("unrelated error rewritten to %v", got)
	}
}
