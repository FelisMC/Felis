package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/backupjob"
)

// liveCluster records persistent freezes and the progress observed by recovery.
type liveCluster struct {
	*fakeCluster
	freezes             map[string]*LiveFreeze
	progress            map[string]LiveProgress
	acquireErr, saveErr error
	deleted             []string
	podUID              string
}

func newLiveCluster() *liveCluster {
	return &liveCluster{fakeCluster: newFakeCluster(), freezes: map[string]*LiveFreeze{}, progress: map[string]LiveProgress{}, podUID: "pod-1"}
}

func (c *liveCluster) AcquireLiveBackup(_ context.Context, name string) (LiveFreeze, error) {
	if c.acquireErr != nil {
		return LiveFreeze{}, c.acquireErr
	}
	if _, held := c.freezes[name]; held {
		return LiveFreeze{}, ErrMaintenanceInProgress
	}
	f := LiveFreeze{Server: name, Started: time.Unix(1_700_000_000, 0), PodUID: c.podUID}
	c.freezes[name] = &f
	return f, nil
}
func (c *liveCluster) SaveLiveBackup(_ context.Context, f LiveFreeze) error {
	if c.saveErr != nil {
		return c.saveErr
	}
	c.freezes[f.Server] = &f
	return nil
}
func (c *liveCluster) ClearLiveBackup(_ context.Context, name string) error {
	delete(c.freezes, name)
	return nil
}
func (c *liveCluster) ListLiveBackups(context.Context) ([]LiveFreeze, error) {
	var out []LiveFreeze
	for _, f := range c.freezes {
		out = append(out, *f)
	}
	return out, nil
}
func (c *liveCluster) LiveBackupProgress(_ context.Context, f LiveFreeze) (LiveProgress, error) {
	return c.progress[f.Server], nil
}
func (c *liveCluster) DeleteLiveBackupJob(_ context.Context, name, job string) error {
	c.deleted = append(c.deleted, name+"/"+job)
	return nil
}

// scriptedConsole answers each command from a table and records the order they came in.
type scriptedConsole struct {
	replies map[string]string
	fail    map[string]bool
	got     []string
}

func (s *scriptedConsole) RunCommand(_ context.Context, name, command string) (string, error) {
	s.got = append(s.got, command)
	if s.fail[command] {
		return "", ErrConsoleUnavailable
	}
	return s.replies[command], nil
}

type liveBackuper struct {
	fakeBackuper
	live    []string
	reasons []string
	err     error
}

func (b *liveBackuper) BackupLive(_ context.Context, name, _, reason string) (string, error) {
	if b.err != nil {
		return "", b.err
	}
	b.live = append(b.live, name)
	b.reasons = append(b.reasons, reason)
	return "backup-" + name + "-live", nil
}

type liveFixture struct {
	a       *API
	cl      *liveCluster
	console *scriptedConsole
	backup  *liveBackuper
}

func newLiveFixture() *liveFixture {
	f := &liveFixture{cl: newLiveCluster(), console: &scriptedConsole{replies: map[string]string{"save-off": "Automatic saving is now disabled"}, fail: map[string]bool{}}, backup: &liveBackuper{}}
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	f.cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Running", Ready: true, DesiredState: string(v1alpha1.DesiredRunning)}
	f.a = newTestAPI(repo, f.cl)
	f.a.Console = f.console
	f.a.Backuper = f.backup
	f.a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
	return f
}

func TestLiveBackupFreezesCopiesAndThaws(t *testing.T) {
	f := newLiveFixture()
	w := do(f.a.ExternalHandler(), "POST", "/api/v1/servers/survival/backup", "", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("backup of a running server = %d %s", w.Code, w.Body.String())
	}
	if got := strings.Join(f.console.got, ","); got != "save-off,save-all flush" {
		t.Fatalf("console = %s, want save-off then save-all flush", got)
	}
	if len(f.backup.live) != 1 || f.backup.reasons[0] != "" || f.backup.calls != 0 {
		t.Fatalf("jobs: live %v (reasons %v), stopped-server backups %d", f.backup.live, f.backup.reasons, f.backup.calls)
	}
	frozen := f.cl.freezes["survival"]
	if frozen == nil || frozen.Job != "backup-survival-live" || frozen.WasOff {
		t.Fatalf("freeze = %+v", frozen)
	}

	f.cl.progress["survival"] = LiveProgress{PodUID: "pod-1", JobExists: true}
	if err := f.a.SettleLiveBackups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.cl.freezes["survival"] == nil || len(f.console.got) != 2 {
		t.Fatalf("saving came back before the copy was done: %v", f.console.got)
	}

	f.cl.progress["survival"] = LiveProgress{PodUID: "pod-1", JobExists: true, StagingDone: true}
	if err := f.a.SettleLiveBackups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.console.got[len(f.console.got)-1] != "save-on" || f.cl.freezes["survival"] != nil || len(f.cl.deleted) != 0 {
		t.Fatalf("after the copy: console %v, freeze %+v, deleted %v", f.console.got, f.cl.freezes["survival"], f.cl.deleted)
	}
}

func TestLiveBackupLeavesSavingOffWhenItWasOff(t *testing.T) {
	f := newLiveFixture()
	f.console.replies["save-off"] = "Saving is already turned off"
	if err := f.a.startLiveBackup(context.Background(), "survival", "owner1", backupjob.ReasonScheduled); err != nil {
		t.Fatal(err)
	}
	if !f.cl.freezes["survival"].WasOff || f.backup.reasons[0] != backupjob.ReasonScheduled {
		t.Fatalf("freeze = %+v, reasons %v", f.cl.freezes["survival"], f.backup.reasons)
	}
	f.cl.progress["survival"] = LiveProgress{PodUID: "pod-1", JobExists: true, StagingDone: true}
	if err := f.a.SettleLiveBackups(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.console.got {
		if c == "save-on" {
			t.Fatal("saving was turned on for a world whose owner had it off")
		}
	}
	if f.cl.freezes["survival"] != nil {
		t.Error("the freeze was kept")
	}
}

func TestLiveBackupUndoesAFailedStart(t *testing.T) {
	for name, setup := range map[string]func(*liveFixture){
		"save-off refused":   func(f *liveFixture) { f.console.fail["save-off"] = true },
		"flush timed out":    func(f *liveFixture) { f.console.fail["save-all flush"] = true },
		"job not created":    func(f *liveFixture) { f.backup.err = errors.New("cluster down") },
		"freeze not written": func(f *liveFixture) { f.cl.saveErr = errors.New("conflict") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newLiveFixture()
			setup(f)
			if err := f.a.startLiveBackup(context.Background(), "survival", "owner1", ""); err == nil {
				t.Fatal("a failed start reported success")
			}
			if f.cl.freezes["survival"] != nil {
				t.Error("the freeze outlived the failure")
			}
			offSent := len(f.console.got) > 0 && f.console.got[0] == "save-off" && !f.console.fail["save-off"]
			if offSent && f.console.got[len(f.console.got)-1] != "save-on" {
				t.Errorf("saving was left off: %v", f.console.got)
			}
			if name == "freeze not written" && len(f.cl.deleted) != 1 {
				t.Errorf("an unrecorded Job was left copying: %v", f.cl.deleted)
			}
		})
	}
}

func TestSettleLiveBackupDropsATornCopy(t *testing.T) {
	f := newLiveFixture()
	f.cl.freezes["survival"] = &LiveFreeze{Server: "survival", Job: "j1", Started: f.a.now(), PodUID: "pod-1"}
	f.cl.progress["survival"] = LiveProgress{PodUID: "pod-2", JobExists: true}
	if err := f.a.SettleLiveBackups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.cl.deleted) != 1 || f.cl.deleted[0] != "survival/j1" || f.cl.freezes["survival"] != nil || len(f.console.got) != 0 {
		t.Fatalf("restarted game: deleted %v, freeze %+v, console %v", f.cl.deleted, f.cl.freezes["survival"], f.console.got)
	}

	f = newLiveFixture()
	f.cl.freezes["survival"] = &LiveFreeze{Server: "survival", Job: "j2", Started: f.a.now().Add(-liveFreezeCap - time.Second), PodUID: "pod-1"}
	f.cl.progress["survival"] = LiveProgress{PodUID: "pod-1", JobExists: true}
	if err := f.a.SettleLiveBackups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.cl.deleted) != 1 || f.cl.freezes["survival"] != nil || strings.Join(f.console.got, ",") != "save-on" {
		t.Fatalf("a copy past the cap: deleted %v, freeze %+v, console %v", f.cl.deleted, f.cl.freezes["survival"], f.console.got)
	}
}

func TestSettleLiveBackupKeepsTryingSaveOn(t *testing.T) {
	f := newLiveFixture()
	f.cl.freezes["survival"] = &LiveFreeze{Server: "survival", Job: "j1", Started: f.a.now(), PodUID: "pod-1"}
	f.cl.progress["survival"] = LiveProgress{PodUID: "pod-1", JobExists: true, JobFinished: true, StagingDone: true}
	f.console.fail["save-on"] = true
	if err := f.a.SettleLiveBackups(context.Background()); err == nil || f.cl.freezes["survival"] == nil {
		t.Fatalf("a refused save-on dropped the freeze (err %v)", err)
	}
	f.console.fail["save-on"] = false
	if err := f.a.SettleLiveBackups(context.Background()); err != nil || f.cl.freezes["survival"] != nil {
		t.Fatalf("the retry = %v, freeze %+v", err, f.cl.freezes["survival"])
	}
}

func TestSettleLiveBackupRecoversAnUnrecordedJob(t *testing.T) {
	f := newLiveFixture()
	f.cl.freezes["survival"] = &LiveFreeze{Server: "survival", Started: f.a.now(), PodUID: "pod-1"}
	f.cl.progress["survival"] = LiveProgress{PodUID: "pod-1"}
	if err := f.a.SettleLiveBackups(context.Background()); err != nil || f.cl.freezes["survival"] == nil {
		t.Fatalf("a fresh freeze without its Job was settled early (err %v)", err)
	}
	f.cl.freezes["survival"].Started = f.a.now().Add(-liveUnrecordedGrace - time.Second)
	if err := f.a.SettleLiveBackups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.cl.deleted) != 1 || f.cl.deleted[0] != "survival/" || f.cl.freezes["survival"] != nil || strings.Join(f.console.got, ",") != "save-on" {
		t.Fatalf("deleted %v, freeze %+v, console %v", f.cl.deleted, f.cl.freezes["survival"], f.console.got)
	}
}

func TestLiveBackupRefusesAServerInTransit(t *testing.T) {
	f := newLiveFixture()
	f.cl.byName["survival"].Ready = false
	w := do(f.a.ExternalHandler(), "POST", "/api/v1/servers/survival/backup", "", nil)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "not_stopped" || len(f.console.got) != 0 {
		t.Fatalf("a starting server = %d %s (console %v)", w.Code, w.Body.String(), f.console.got)
	}
}

// liveScheduledBackuper takes live backups and scheduled ones of stopped worlds.
type liveScheduledBackuper struct {
	liveBackuper
	stopped []string
}

func (b *liveScheduledBackuper) BackupScheduled(_ context.Context, name, _ string) error {
	b.stopped = append(b.stopped, name)
	return nil
}

func TestBackupSchedulerTakesARunningWorldLive(t *testing.T) {
	f := newLiveFixture()
	f.cl.maintErr["survival"] = ErrNotStopped
	b := &liveScheduledBackuper{}
	f.a.Backuper = b
	store := &fakeScheduleStore{due: []ScheduledCandidate{{"survival", "owner1"}}}
	s := &BackupScheduler{API: f.a, Store: store, Jobs: &fakeWorldJobs{}, Every: 24 * time.Hour}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.live) != 1 || b.reasons[0] != backupjob.ReasonScheduled || len(b.stopped) != 0 {
		t.Fatalf("live %v reasons %v stopped %v", b.live, b.reasons, b.stopped)
	}
	repo := f.a.Repo.(*fakeRepo)
	if len(repo.audits) != 1 || repo.audits[0].Action != "backup.scheduled" {
		t.Errorf("audits = %+v", repo.audits)
	}

	f = newLiveFixture()
	f.cl.maintErr["survival"] = ErrNotStopped
	f.console.fail["save-off"] = true
	b = &liveScheduledBackuper{}
	f.a.Backuper = b
	s = &BackupScheduler{API: f.a, Store: store, Jobs: &fakeWorldJobs{}, Every: 24 * time.Hour}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.live) != 0 || len(f.console.got) != 1 {
		t.Fatalf("a silent console: live %v, console %v", b.live, f.console.got)
	}
	if err := s.Tick(context.Background()); err != nil || len(f.console.got) != 1 {
		t.Fatalf("the retry came at once: console %v (%v)", f.console.got, err)
	}
}

func TestScheduleRunnerBacksUpARunningServerLive(t *testing.T) {
	r := newSchedRig(t)
	lc := newLiveCluster()
	lc.fakeCluster = r.cl
	console := &scriptedConsole{replies: map[string]string{}, fail: map[string]bool{}}
	b := &liveScheduledBackuper{}
	r.a.Cluster, r.a.Console, r.a.Backuper = lc, console, b
	id := r.schedule([]string{StepBackup}, schedT0)
	r.tick(t)
	r.step(t, id, phaseBackingUp)
	if len(b.live) != 1 || b.reasons[0] != backupjob.ReasonScheduled || r.cl.desired["survival"] != "" {
		t.Fatalf("live %v reasons %v, desired %q", b.live, b.reasons, r.cl.desired["survival"])
	}
	if strings.Join(console.got, ",") != "save-off,save-all flush" {
		t.Fatalf("console = %v", console.got)
	}
	r.jobs.jobs = []AsyncJob{{Kind: "backup", State: "succeeded", StartedAt: schedT0, Scheduled: true}}
	r.tick(t)
	r.outcome(t, id, ScheduleOK, "")
	if r.cl.desired["survival"] != "" {
		t.Errorf("a live backup touched the desired state: %q", r.cl.desired["survival"])
	}

	r = newSchedRig(t)
	lc = newLiveCluster()
	lc.fakeCluster = r.cl
	console = &scriptedConsole{replies: map[string]string{}, fail: map[string]bool{"save-off": true}}
	r.a.Cluster, r.a.Console, r.a.Backuper = lc, console, &liveScheduledBackuper{}
	id = r.schedule([]string{StepBackup}, schedT0)
	r.tick(t)
	r.step(t, id, phaseBackupStopping)
	if r.cl.desired["survival"] != v1alpha1.DesiredStopped {
		t.Errorf("the fallback did not stop the server: %q", r.cl.desired["survival"])
	}
}

func TestScheduleWarningSaysTheServerStaysUp(t *testing.T) {
	if got := scheduleWarning(StepBackup, 5, true); !strings.Contains(got, "saving pauses for a few seconds") {
		t.Errorf("live warning = %q", got)
	}
	if got := scheduleWarning(StepBackup, 5, false); !strings.Contains(got, "Server pauses for a backup") {
		t.Errorf("stop warning = %q", got)
	}
}
