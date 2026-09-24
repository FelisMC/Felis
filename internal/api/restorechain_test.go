package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/restore"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeSnapshotter is a Backuper that can also chain a restore.
type fakeSnapshotter struct {
	fakeBackuper
	err     error
	chained []RestoreChain
}

func (f *fakeSnapshotter) BackupThenRestore(_ context.Context, name, formerOwner, backupID, backupRef string) error {
	f.gotFormerOwn = formerOwner
	f.chained = append(f.chained, RestoreChain{Server: name, BackupID: backupID, BackupRef: backupRef})
	return f.err
}

type settled struct{ job, state, note string }

type fakeChains struct {
	pending   []RestoreChain
	listErr   error
	settleErr error
	settled   []settled
}

func (f *fakeChains) PendingRestoreChains(context.Context) ([]RestoreChain, error) {
	return f.pending, f.listErr
}

func (f *fakeChains) SettleRestoreChain(_ context.Context, job, state, note string) error {
	if f.settleErr != nil {
		return f.settleErr
	}
	f.settled = append(f.settled, settled{job, state, note})
	return nil
}

// otherRestore mirrors the executor's conflict error, which api only knows by
// its method.
type otherRestore struct{}

func (otherRestore) Error() string           { return "another restore" }
func (otherRestore) RestoreInProgress() bool { return true }

var _ RestoreSnapshotter = (*backupjob.Backuper)(nil)

func TestIsRestoreInProgressMatchesTheExecutor(t *testing.T) {
	if !isRestoreInProgress(restore.ErrOtherRestoreRunning) {
		t.Fatal("restore.ErrOtherRestoreRunning is not recognised")
	}
	if isRestoreInProgress(restore.ErrAlreadyExists) || isRestoreInProgress(errors.New("x")) {
		t.Fatal("an unrelated error reads as a restore in progress")
	}
}

// With the chain wired, a restore starts with a safety snapshot of the world as
// it is: the Backuper gets the restore to carry, the Restorer is not called
// yet, and the 202 says so. safety_snapshot:false restores straight away.
func TestRestoreStartsWithSafetySnapshot(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}
	mk := func() (*API, *fakeSnapshotter, *fakeRestorer) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		repo.backups = []fakeBackup{{view: BackupView{ID: "bk1", ServerName: "survival", FormerOwner: "owner1",
			Status: "present", Reason: "manual", CreatedAt: time.Unix(1_699_000_000, 0)}, ref: "ref-1"}}
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped", DesiredState: string(v1alpha1.DesiredStopped)}
		a := newTestAPI(repo, cl)
		snap, restorer := &fakeSnapshotter{}, &fakeRestorer{}
		a.Backuper, a.Restorer, a.RestoreChains = snap, restorer, &fakeChains{}
		a.External = staticExternal{p: owner}
		return a, snap, restorer
	}
	const path = "/api/v1/servers/survival/restore-backup"
	decode := func(t *testing.T, body []byte) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("body: %v", err)
		}
		return m
	}

	t.Run("default -> snapshot first", func(t *testing.T) {
		a, snap, restorer := mk()
		w := do(a.ExternalHandler(), "POST", path, `{"backup_id":"bk1"}`, jsonHeader)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if m := decode(t, w.Body.Bytes()); m["safety_snapshot"] != true || m["status"] != "restoring" || m["backup_id"] != "bk1" {
			t.Fatalf("response = %v", m)
		}
		if len(snap.chained) != 1 || snap.chained[0] != (RestoreChain{Server: "survival", BackupID: "bk1", BackupRef: "ref-1"}) || snap.gotFormerOwn != "owner1" {
			t.Fatalf("chained = %+v (owner %q)", snap.chained, snap.gotFormerOwn)
		}
		if restorer.calls != 0 {
			t.Fatal("the restore ran before its snapshot")
		}
	})

	t.Run("safety_snapshot false -> restore now", func(t *testing.T) {
		a, snap, restorer := mk()
		w := do(a.ExternalHandler(), "POST", path, `{"safety_snapshot":false}`, jsonHeader)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if m := decode(t, w.Body.Bytes()); m["safety_snapshot"] != false {
			t.Fatalf("response = %v", m)
		}
		if restorer.calls != 1 || len(snap.chained) != 0 {
			t.Fatalf("restorer calls %d, chained %d", restorer.calls, len(snap.chained))
		}
	})

	t.Run("chain not wired -> restore now", func(t *testing.T) {
		a, snap, restorer := mk()
		a.RestoreChains = nil
		if w := do(a.ExternalHandler(), "POST", path, "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if restorer.calls != 1 || len(snap.chained) != 0 {
			t.Fatalf("restorer calls %d, chained %d", restorer.calls, len(snap.chained))
		}
	})

	t.Run("another restore running -> 409 restore_in_progress", func(t *testing.T) {
		a, _, restorer := mk()
		restorer.err = otherRestore{}
		w := do(a.ExternalHandler(), "POST", path, `{"safety_snapshot":false}`, jsonHeader)
		if w.Code != http.StatusConflict || errCode(w.Body.Bytes()) != "restore_in_progress" {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
	})
}

func TestSettleRestoreChains(t *testing.T) {
	mk := func(chains ...RestoreChain) (*API, *fakeChains, *fakeRestorer, *fakeCluster) {
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped", DesiredState: string(v1alpha1.DesiredStopped)}
		a := newTestAPI(newFakeRepo(), cl)
		fc, restorer := &fakeChains{pending: chains}, &fakeRestorer{}
		a.RestoreChains, a.Restorer = fc, restorer
		return a, fc, restorer, cl
	}
	chain := func(snapshot string) RestoreChain {
		return RestoreChain{Job: "backup-survival-1", Server: "survival", BackupID: "bk1", BackupRef: "ref-1", Snapshot: snapshot}
	}
	ctx := context.Background()

	t.Run("snapshot succeeded -> restore started", func(t *testing.T) {
		a, fc, restorer, _ := mk(chain(ChainSnapshotSucceeded))
		if err := a.SettleRestoreChains(ctx); err != nil {
			t.Fatal(err)
		}
		if restorer.calls != 1 || restorer.gotName != "survival" || restorer.gotRef != "ref-1" {
			t.Fatalf("restorer = %+v", restorer)
		}
		if len(fc.settled) != 1 || fc.settled[0] != (settled{"backup-survival-1", maintenance.ThenRestoreStarted, ""}) {
			t.Fatalf("settled = %+v", fc.settled)
		}
	})

	t.Run("snapshot running -> left alone", func(t *testing.T) {
		a, fc, restorer, _ := mk(chain(ChainSnapshotRunning))
		if err := a.SettleRestoreChains(ctx); err != nil {
			t.Fatal(err)
		}
		if restorer.calls != 0 || len(fc.settled) != 0 {
			t.Fatalf("restorer %d, settled %+v", restorer.calls, fc.settled)
		}
	})

	for _, tc := range []struct {
		name   string
		setup  func(*fakeRestorer, *fakeCluster)
		snap   string
		reason string
	}{
		{"snapshot failed", func(*fakeRestorer, *fakeCluster) {}, ChainSnapshotFailed, ChainAbandonSnapshotFailed},
		{"server started meanwhile", func(_ *fakeRestorer, cl *fakeCluster) {
			cl.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning)
		}, ChainSnapshotSucceeded, ChainAbandonServerStarted},
		{"server gone", func(_ *fakeRestorer, cl *fakeCluster) { delete(cl.byName, "survival") }, ChainSnapshotSucceeded, ChainAbandonServerGone},
		{"another restore running", func(r *fakeRestorer, _ *fakeCluster) { r.err = otherRestore{} }, ChainSnapshotSucceeded, ChainAbandonRestoreBusy},
	} {
		t.Run(tc.name+" -> abandoned", func(t *testing.T) {
			a, fc, restorer, cl := mk(chain(tc.snap))
			tc.setup(restorer, cl)
			if err := a.SettleRestoreChains(ctx); err != nil {
				t.Fatal(err)
			}
			if len(fc.settled) != 1 || fc.settled[0].state != maintenance.ThenRestoreAbandoned || fc.settled[0].note != tc.reason {
				t.Fatalf("settled = %+v", fc.settled)
			}
			if tc.snap == ChainSnapshotFailed && restorer.calls != 0 {
				t.Fatal("restored after a failed snapshot")
			}
		})
	}

	t.Run("restore error -> stays pending, retried", func(t *testing.T) {
		a, fc, restorer, _ := mk(chain(ChainSnapshotSucceeded))
		restorer.err = errors.New("apiserver hiccup")
		if err := a.SettleRestoreChains(ctx); err == nil {
			t.Fatal("the failure was swallowed")
		}
		if len(fc.settled) != 0 {
			t.Fatalf("settled = %+v", fc.settled)
		}
		restorer.err = nil
		if err := a.SettleRestoreChains(ctx); err != nil || len(fc.settled) != 1 || fc.settled[0].state != maintenance.ThenRestoreStarted {
			t.Fatalf("retry: err %v, settled %+v", err, fc.settled)
		}
	})
}

// The cluster side: the chain the backupjob executor renders is found by the
// label selector, its snapshot outcome read from the Job conditions, and
// settling it releases the world volume.
func TestK8sRestoreChains(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	chain, err := backupjob.BackupJob(backupjob.JobParams{
		Server: "survival", JobName: "backup-survival-aa", WorldPVC: "world-survival-0", BackupPVC: "felis-backups",
		Namespace: "minecraft", Image: "felis:1", ConfigSecret: "felis-config", ConfigMount: "/etc/felis",
		RestoreRef: "/backups/survival/a.tar.gz", RestoreBackupID: "bk-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	chain.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	plain, err := backupjob.BackupJob(backupjob.JobParams{
		Server: "survival", JobName: "backup-survival-bb", WorldPVC: "world-survival-0", BackupPVC: "felis-backups",
		Namespace: "minecraft", Image: "felis:1", ConfigSecret: "felis-config", ConfigMount: "/etc/felis",
	})
	if err != nil {
		t.Fatal(err)
	}
	plain.Status.Conditions = chain.Status.Conditions
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(chain, plain).WithStatusSubresource(&batchv1.Job{}).Build()
	k := NewK8sJobStatus(c, "minecraft")
	ctx := context.Background()

	got, err := k.PendingRestoreChains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := RestoreChain{Job: "backup-survival-aa", Server: "survival", BackupID: "bk-1",
		BackupRef: "/backups/survival/a.tar.gz", Snapshot: ChainSnapshotSucceeded}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("pending = %+v, want [%+v]", got, want)
	}

	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	if kind, held := maintenance.Holder("survival", nil, jobs.Items, time.Now()); !held || kind != maintenance.KindRestore {
		t.Fatalf("pending chain: Holder = %q, %v", kind, held)
	}

	if err := k.SettleRestoreChain(ctx, "backup-survival-aa", maintenance.ThenRestoreAbandoned, ChainAbandonServerStarted); err != nil {
		t.Fatal(err)
	}
	var settledJob batchv1.Job
	if err := c.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "backup-survival-aa"}, &settledJob); err != nil {
		t.Fatal(err)
	}
	if settledJob.Labels[maintenance.LabelThenRestore] != maintenance.ThenRestoreAbandoned ||
		settledJob.Annotations[maintenance.AnnotationThenRestoreReason] != ChainAbandonServerStarted ||
		settledJob.Annotations[maintenance.AnnotationRestoreRef] == "" {
		t.Fatalf("settled job meta: labels %v annotations %v", settledJob.Labels, settledJob.Annotations)
	}
	if got, _ := k.PendingRestoreChains(ctx); len(got) != 0 {
		t.Fatalf("still pending after settling: %+v", got)
	}
	if err := c.List(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	if kind, held := maintenance.Holder("survival", nil, jobs.Items, time.Now()); held {
		t.Fatalf("settled chain still holds the volume as %q", kind)
	}

	aj, ok := jobToAsyncJob(&settledJob)
	if !ok || aj.ThenRestore != maintenance.ThenRestoreAbandoned || aj.RestoreBackupID != "bk-1" || aj.State != "succeeded" ||
		aj.ThenRestoreReason != ChainAbandonServerStarted || aj.Message != chainAbandonText[ChainAbandonServerStarted] {
		t.Fatalf("projection = %+v", aj)
	}
}
