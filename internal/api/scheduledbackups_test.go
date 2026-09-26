package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/maintenance"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ ScheduledBackuper = (*backupjob.Backuper)(nil)

// fakeScheduledBackuper is a Backuper that can also take a scheduled backup.
type fakeScheduledBackuper struct {
	fakeBackuper
	err       error
	scheduled []ScheduledCandidate
}

func (f *fakeScheduledBackuper) BackupScheduled(_ context.Context, name, formerOwner string) error {
	f.scheduled = append(f.scheduled, ScheduledCandidate{Name: name, OwnerID: formerOwner})
	return f.err
}

type fakeScheduleStore struct {
	due       []ScheduledCandidate
	gotBefore time.Time
	calls     int
}

func (f *fakeScheduleStore) ScheduledBackupCandidates(_ context.Context, before time.Time) ([]ScheduledCandidate, error) {
	f.calls++
	f.gotBefore = before
	return f.due, nil
}

type fakeWorldJobs struct {
	running int
	err     error
}

func (f *fakeWorldJobs) RunningWorldJobs(context.Context) (int, error) { return f.running, f.err }

func TestBackupScheduler(t *testing.T) {
	const every = 24 * time.Hour
	type rig struct {
		a     *API
		repo  *fakeRepo
		cl    *fakeCluster
		b     *fakeScheduledBackuper
		store *fakeScheduleStore
		jobs  *fakeWorldJobs
		s     *BackupScheduler
	}
	mk := func() rig {
		repo := newFakeRepo()
		cl := newFakeCluster()
		for _, n := range []string{"alpha", "bravo"} {
			cl.byName[n] = &ServerInfo{Name: n, Phase: "Stopped"}
		}
		a := newTestAPI(repo, cl)
		b := &fakeScheduledBackuper{}
		a.Backuper = b
		store := &fakeScheduleStore{due: []ScheduledCandidate{{"alpha", "usr-a"}, {"bravo", "usr-b"}}}
		jobs := &fakeWorldJobs{}
		return rig{a, repo, cl, b, store, jobs, &BackupScheduler{API: a, Store: store, Jobs: jobs, Every: every}}
	}
	tick := func(t *testing.T, r rig) {
		t.Helper()
		if err := r.s.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	launched := func(r rig) []ScheduledCandidate { return r.b.scheduled }

	t.Run("backs up the first due world as its owner, one per tick", func(t *testing.T) {
		r := mk()
		tick(t, r)
		if got := launched(r); len(got) != 1 || got[0] != (ScheduledCandidate{"alpha", "usr-a"}) {
			t.Fatalf("launched = %+v; want alpha as usr-a only", got)
		}
		if want := r.a.now().Add(-every); !r.store.gotBefore.Equal(want) {
			t.Fatalf("asked for points before %v; want %v", r.store.gotBefore, want)
		}
		if len(r.cl.acquired) != 1 || r.cl.acquired[0] != "alpha:"+maintenance.KindBackup ||
			len(r.cl.released) != 1 || r.cl.released[0] != "alpha" {
			t.Fatalf("lock: acquired %v released %v; want alpha held as a backup and let go", r.cl.acquired, r.cl.released)
		}
		if len(r.repo.audits) != 1 || r.repo.audits[0].Action != "backup.scheduled" || r.repo.audits[0].ServerName != "alpha" {
			t.Fatalf("audits = %+v; want one backup.scheduled of alpha", r.repo.audits)
		}
		if at, _ := r.repo.LastBackupRequest(context.Background(), "alpha", time.Time{}); !at.IsZero() {
			t.Fatal("the scheduled backup started the owner's manual cooldown")
		}
		if r.b.calls != 0 {
			t.Fatal("the scheduled backup went through the manual Backup")
		}
	})

	t.Run("a running or busy world waits for the next tick", func(t *testing.T) {
		for _, err := range []error{ErrNotStopped, &MaintenanceBusyError{Kind: maintenance.KindRestore}, ErrMaintenanceInProgress} {
			r := mk()
			r.cl.maintErr["alpha"] = err
			tick(t, r)
			if got := launched(r); len(got) != 1 || got[0].Name != "bravo" {
				t.Fatalf("%v: launched = %+v; want bravo", err, got)
			}
			delete(r.cl.maintErr, "alpha")
			r.b.scheduled = nil
			tick(t, r)
			if got := launched(r); len(got) != 1 || got[0].Name != "alpha" {
				t.Fatalf("%v: once alpha is free, launched = %+v; want alpha", err, got)
			}
		}
	})

	t.Run("waits while a backup or restore job runs", func(t *testing.T) {
		r := mk()
		r.jobs.running = 1
		tick(t, r)
		if len(launched(r)) != 0 || len(r.cl.acquired) != 0 {
			t.Fatalf("launched %+v, acquired %v beside a running job", launched(r), r.cl.acquired)
		}
		r.jobs.running = 0
		tick(t, r)
		if len(launched(r)) != 1 {
			t.Fatalf("launched = %+v once the job finished", launched(r))
		}
	})

	t.Run("pauses while the store is full", func(t *testing.T) {
		r := mk()
		r.a.BackupStoreCap = 100
		r.repo.backups = []fakeBackup{{view: BackupView{ID: "bk1", ServerName: "bravo", Status: "present", SizeBytes: 100}}}
		tick(t, r)
		if len(launched(r)) != 0 {
			t.Fatalf("launched = %+v into a full store", launched(r))
		}
		r.repo.backups[0].view.SizeBytes = 99
		tick(t, r)
		if len(launched(r)) != 1 {
			t.Fatalf("launched = %+v below the cap", launched(r))
		}
	})

	t.Run("a world whose backup failed is retried after a quarter period", func(t *testing.T) {
		r := mk()
		r.store.due = r.store.due[:1]
		r.b.err = errors.New("apiserver down")
		if err := r.s.Tick(context.Background()); err == nil {
			t.Fatal("Tick hid the failed launch")
		}
		if len(r.cl.released) != 1 {
			t.Fatalf("released = %v; the lock must go when the launch fails", r.cl.released)
		}
		r.b.err = nil
		start := r.a.now()
		r.a.Now = func() time.Time { return start.Add(every/4 - time.Minute) }
		tick(t, r)
		if len(launched(r)) != 1 {
			t.Fatalf("launched = %+v; retried before a quarter period", launched(r))
		}
		r.a.Now = func() time.Time { return start.Add(every / 4) }
		tick(t, r)
		if len(launched(r)) != 2 {
			t.Fatalf("launched = %+v; not retried after a quarter period", launched(r))
		}
	})

	t.Run("a world without a volume is passed over", func(t *testing.T) {
		r := mk()
		r.cl.noWorld["alpha"] = true
		tick(t, r)
		if got := launched(r); len(got) != 1 || got[0].Name != "bravo" || len(r.cl.acquired) != 1 {
			t.Fatalf("launched = %+v, acquired %v; want bravo alone", got, r.cl.acquired)
		}
	})

	t.Run("a world deleted since the listing is passed over", func(t *testing.T) {
		r := mk()
		r.cl.maintErr["alpha"] = ErrNotFound
		tick(t, r)
		if got := launched(r); len(got) != 1 || got[0].Name != "bravo" {
			t.Fatalf("launched = %+v; want bravo", got)
		}
	})

	t.Run("a lock failure stops the tick", func(t *testing.T) {
		r := mk()
		r.cl.maintErr["alpha"] = errors.New("conflict storm")
		if err := r.s.Tick(context.Background()); err == nil || len(launched(r)) != 0 {
			t.Fatalf("Tick = %v, launched %+v; want the error and nothing started", err, launched(r))
		}
	})

	t.Run("off without a scheduling backuper or a period", func(t *testing.T) {
		r := mk()
		r.a.Backuper = &fakeBackuper{}
		tick(t, r)
		r2 := mk()
		r2.s.Every = 0
		tick(t, r2)
		if r.store.calls != 0 || r2.store.calls != 0 || len(r2.b.scheduled) != 0 {
			t.Fatal("the scheduler ran while off")
		}
	})
}

// The jobs route marks the executor's scheduled backups, and the scheduler
// counts every unfinished backup and restore Job and nothing else.
func TestK8sScheduledBackupJobs(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	params := func(name string, scheduled bool) backupjob.JobParams {
		return backupjob.JobParams{
			Server: "survival", JobName: name, WorldPVC: "world-survival-0", BackupPVC: "felis-backups",
			Namespace: "minecraft", Image: "felis:1", ConfigSecret: "felis-config", ConfigMount: "/etc/felis",
			Scheduled: scheduled,
		}
	}
	scheduled, err := backupjob.BackupJob(params("backup-survival-aa", true))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := backupjob.BackupJob(params("backup-survival-bb", false))
	if err != nil {
		t.Fatal(err)
	}
	plain.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	restoring := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "minecraft", Name: "restore-other-cc",
		Labels: map[string]string{jobServerLabel: "other", jobManagedByLabel: jobManagedByRestore}}}
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "minecraft", Name: "files-survival-dd",
		Labels: map[string]string{jobServerLabel: "survival", jobManagedByLabel: "felis-files"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scheduled, plain, restoring, foreign).
		WithStatusSubresource(&batchv1.Job{}).Build()
	k := NewK8sJobStatus(c, "minecraft")
	ctx := context.Background()

	if n, err := k.RunningWorldJobs(ctx); err != nil || n != 2 {
		t.Fatalf("RunningWorldJobs = %d, %v; want the scheduled backup and the restore", n, err)
	}
	jobs, err := k.LatestJobs(ctx, "survival")
	if err != nil {
		t.Fatal(err)
	}
	marks := map[string]bool{}
	for _, j := range jobs {
		marks[j.Name] = j.Scheduled
	}
	if len(marks) != 2 || !marks["backup-survival-aa"] || marks["backup-survival-bb"] {
		t.Fatalf("scheduled marks = %v; want only backup-survival-aa", marks)
	}
}
