package maintenance

import (
	"testing"
	"time"

	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/restore"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

var now = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func restoreJob(t *testing.T, server string) batchv1.Job {
	t.Helper()
	j, err := restore.RestoreJob(restore.JobParams{
		Server: server, WorldPVC: "world-" + server + "-0", BackupPVC: "felis-backups",
		BackupRef: "/backups/" + server + "/a.tar.gz", ArchiveStore: "tarLocal",
		Namespace: "minecraft", ServiceAccount: "felis-restore", Image: "felis:1",
		BackupRoot: "/backups", WorldsRoot: "/world", Deadline: time.Minute,
		CPULimit: "1", MemLimit: "1Gi", TTLAfterFinished: time.Minute,
	})
	if err != nil {
		t.Fatalf("RestoreJob: %v", err)
	}
	return *j
}

func backupJob(t *testing.T, server string) batchv1.Job {
	t.Helper()
	j, err := backupjob.BackupJob(backupjob.JobParams{
		Server: server, FormerOwner: "u", WorldPVC: "world-" + server + "-0", BackupPVC: "felis-backups",
		Namespace: "minecraft", ServiceAccount: "felis-restore", Image: "felis:1",
		ConfigSecret: "felis-config", ConfigMount: "/etc/felis", BackupRoot: "/backups",
		WorldsRoot: "/world", Deadline: time.Minute, CPULimit: "1", MemLimit: "1Gi",
		TTLAfterFinished: time.Minute,
	})
	if err != nil {
		t.Fatalf("BackupJob: %v", err)
	}
	return *j
}

func filesJob(t *testing.T, server, op string) batchv1.Job {
	t.Helper()
	p := fileedit.JobParams{
		Server: server, OpID: "0011223344556677", Op: op, Path: "server.properties",
		WorldPVC: "world-" + server + "-0", Namespace: "minecraft", ServiceAccount: "felis-restore",
		Image: "felis:1", WorldsRoot: "/data", Deadline: time.Minute, CPULimit: "500m",
		MemLimit: "256Mi", TTLAfterFinished: time.Minute,
	}
	switch op {
	case fileedit.OpWrite:
		p.Content = []byte("motd=hi\n")
	case fileedit.OpRename:
		p.To = "server.properties.bak"
	case fileedit.OpUpload:
		p.SourceURL, p.UploadToken = "http://felis-api-internal.felis.svc.cluster.local:8081/x", "t"
	}
	j, err := fileedit.FilesJob(p)
	if err != nil {
		t.Fatalf("FilesJob: %v", err)
	}
	return *j
}

func finished(j batchv1.Job, cond batchv1.JobConditionType) batchv1.Job {
	j.Status.Conditions = append(j.Status.Conditions, batchv1.JobCondition{Type: cond, Status: corev1.ConditionTrue})
	return j
}

// The executors keep their own label copies (they must not import each other or
// this package's callers); this pins what they actually render against JobKind.
func TestJobKindMatchesTheExecutors(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  batchv1.Job
		kind string
		ok   bool
	}{
		{"restore", restoreJob(t, "survival"), KindRestore, true},
		{"backup", backupJob(t, "survival"), KindBackup, true},
		{"file write", filesJob(t, "survival", fileedit.OpWrite), KindFileWrite, true},
		{"file mkdir", filesJob(t, "survival", fileedit.OpMkdir), KindFileWrite, true},
		{"file delete", filesJob(t, "survival", fileedit.OpDelete), KindFileWrite, true},
		{"file rename", filesJob(t, "survival", fileedit.OpRename), KindFileWrite, true},
		{"file upload", filesJob(t, "survival", fileedit.OpUpload), KindFileWrite, true},
		{"file read", filesJob(t, "survival", fileedit.OpRead), "", false},
		{"file list", filesJob(t, "survival", fileedit.OpList), "", false},
	} {
		kind, ok := JobKind(&tc.job)
		if kind != tc.kind || ok != tc.ok {
			t.Errorf("%s: JobKind = %q, %v; want %q, %v", tc.name, kind, ok, tc.kind, tc.ok)
		}
		if tc.job.Labels[LabelServer] != "survival" {
			t.Errorf("%s: server label = %q", tc.name, tc.job.Labels[LabelServer])
		}
	}
}

func TestUnlabelledFilesJobCountsAsWrite(t *testing.T) {
	j := filesJob(t, "survival", fileedit.OpRead)
	delete(j.Labels, LabelFilesMode)
	if kind, ok := JobKind(&j); !ok || kind != KindFileWrite {
		t.Fatalf("a files Job from before the mode label = %q, %v; want file-write", kind, ok)
	}
	// An operation a later build adds holds too, until this build learns it.
	j.Labels[LabelFilesMode] = "chmod"
	if kind, ok := JobKind(&j); !ok || kind != KindFileWrite {
		t.Fatalf("a files Job with an unknown mode = %q, %v; want file-write", kind, ok)
	}
}

func TestHolderFromJobs(t *testing.T) {
	restoreRunning := restoreJob(t, "survival")
	if kind, ok := Holder("survival", nil, []batchv1.Job{restoreRunning}, now); !ok || kind != KindRestore {
		t.Fatalf("running restore: Holder = %q, %v", kind, ok)
	}
	for _, cond := range []batchv1.JobConditionType{
		batchv1.JobComplete, batchv1.JobFailed, batchv1.JobSuccessCriteriaMet, batchv1.JobFailureTarget,
	} {
		if kind, ok := Holder("survival", nil, []batchv1.Job{finished(restoreRunning, cond)}, now); ok {
			t.Errorf("restore with %s still holds as %q", cond, kind)
		}
	}
	other := backupJob(t, "creative")
	if kind, ok := Holder("survival", nil, []batchv1.Job{other}, now); ok {
		t.Fatalf("another server's backup holds survival as %q", kind)
	}
	read := filesJob(t, "survival", fileedit.OpRead)
	if kind, ok := Holder("survival", nil, []batchv1.Job{read}, now); ok {
		t.Fatalf("a running file read holds the volume as %q", kind)
	}
	write := filesJob(t, "survival", fileedit.OpWrite)
	if kind, ok := Holder("survival", nil, []batchv1.Job{read, write}, now); !ok || kind != KindFileWrite {
		t.Fatalf("running file write: Holder = %q, %v", kind, ok)
	}
}

// A safety snapshot holds the volume as a restore from its creation until
// felis-api settles the chain, finished or not, so nothing wakes the server
// between the backup Job and the restore Job it is followed by.
func TestHolderFromRestoreChain(t *testing.T) {
	j, err := backupjob.BackupJob(backupjob.JobParams{
		Server: "survival", WorldPVC: "world-survival-0", BackupPVC: "felis-backups",
		Namespace: "minecraft", Image: "felis:1", ConfigSecret: "felis-config", ConfigMount: "/etc/felis",
		RestoreRef: "/backups/survival/a.tar.gz", RestoreBackupID: "bk-1",
	})
	if err != nil {
		t.Fatalf("BackupJob: %v", err)
	}
	if j.Annotations[AnnotationRestoreRef] != "/backups/survival/a.tar.gz" || j.Annotations[AnnotationRestoreBackupID] != "bk-1" {
		t.Fatalf("chain annotations = %v", j.Annotations)
	}
	if !RestorePending(j) {
		t.Fatalf("a fresh safety snapshot is not a pending chain: labels %v", j.Labels)
	}
	for _, tc := range []struct {
		name string
		job  batchv1.Job
		held bool
		kind string
	}{
		{"running", *j, true, KindRestore},
		{"succeeded, restore not started yet", finished(*j, batchv1.JobComplete), true, KindRestore},
		{"failed, not settled yet", finished(*j, batchv1.JobFailed), true, KindRestore},
		{"restore started", settled(finished(*j, batchv1.JobComplete), ThenRestoreStarted), false, ""},
		{"abandoned", settled(finished(*j, batchv1.JobFailed), ThenRestoreAbandoned), false, ""},
		{"abandoned while running", settled(*j, ThenRestoreAbandoned), true, KindBackup},
	} {
		kind, held := Holder("survival", nil, []batchv1.Job{tc.job}, now)
		if held != tc.held || kind != tc.kind {
			t.Errorf("%s: Holder = %q, %v; want %q, %v", tc.name, kind, held, tc.kind, tc.held)
		}
	}
	if plain := backupJob(t, "survival"); RestorePending(&plain) {
		t.Fatal("a plain backup is a pending chain")
	}
}

func settled(j batchv1.Job, state string) batchv1.Job {
	j.Labels = map[string]string{LabelServer: j.Labels[LabelServer], LabelManagedBy: j.Labels[LabelManagedBy], LabelThenRestore: state}
	return j
}

func TestHolderFromLock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		held  bool
	}{
		{"fresh", LockValue(KindBackup, now.Add(-10*time.Second)), true},
		{"just inside grace", LockValue(KindBackup, now.Add(-Grace+time.Second)), true},
		{"stale", LockValue(KindBackup, now.Add(-Grace)), false},
		{"far future", LockValue(KindBackup, now.Add(time.Hour)), false},
		{"garbage", "yes", false},
		{"no kind", "@" + now.Format(time.RFC3339), false},
		{"bad time", "restore@yesterday", false},
	} {
		kind, held := Holder("survival", map[string]string{Annotation: tc.value}, nil, now)
		if held != tc.held {
			t.Errorf("%s (%q): held = %v, want %v", tc.name, tc.value, held, tc.held)
		}
		if held && kind != KindBackup {
			t.Errorf("%s: kind = %q", tc.name, kind)
		}
	}
}
