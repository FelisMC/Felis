package backupjob

import (
	"context"
	"strings"
	"testing"
)

// captureJobs records every JobParams it is handed so the test can inspect the
// names the Backuper minted.
type captureJobs struct{ got []JobParams }

func (c *captureJobs) CreateBackupJob(_ context.Context, p JobParams) error {
	c.got = append(c.got, p)
	return nil
}

func TestBackupMintsUniqueJobNamePerCall(t *testing.T) {
	jobs := &captureJobs{}
	b := &Backuper{Jobs: jobs, Config: Config{Image: "img", BackupPVC: "pvc"}}

	for i := 0; i < 2; i++ {
		if err := b.Backup(context.Background(), "survival", "usr-1"); err != nil {
			t.Fatalf("Backup: %v", err)
		}
	}
	if len(jobs.got) != 2 {
		t.Fatalf("created %d jobs, want 2", len(jobs.got))
	}
	base := BackupJobName("survival")
	for _, p := range jobs.got {
		if !strings.HasPrefix(p.JobName, base+"-") {
			t.Errorf("JobName %q must extend the base %q", p.JobName, base)
		}
	}
	if jobs.got[0].JobName == jobs.got[1].JobName {
		t.Errorf("two backups reused the Job name %q — retry would silently no-op", jobs.got[0].JobName)
	}
}

func TestBackupThenRestoreChainsTheRestore(t *testing.T) {
	jobs := &captureJobs{}
	b := &Backuper{Jobs: jobs, Config: Config{Image: "img", BackupPVC: "pvc"}}
	if err := b.BackupThenRestore(context.Background(), "survival", "usr-1", "bk-1", "/backups/a.tar.gz"); err != nil {
		t.Fatalf("BackupThenRestore: %v", err)
	}
	if len(jobs.got) != 1 {
		t.Fatalf("created %d jobs, want 1", len(jobs.got))
	}
	p := jobs.got[0]
	if p.RestoreRef != "/backups/a.tar.gz" || p.RestoreBackupID != "bk-1" || p.FormerOwner != "usr-1" {
		t.Errorf("params = %+v", p)
	}
	if !strings.HasPrefix(p.JobName, BackupJobName("survival")+"-") {
		t.Errorf("JobName %q", p.JobName)
	}
}

func TestBackupScheduledMarksTheJob(t *testing.T) {
	jobs := &captureJobs{}
	b := &Backuper{Jobs: jobs, Config: Config{Image: "img", BackupPVC: "pvc"}}
	if err := b.BackupScheduled(context.Background(), "survival", "usr-1"); err != nil {
		t.Fatalf("BackupScheduled: %v", err)
	}
	if len(jobs.got) != 1 {
		t.Fatalf("created %d jobs, want 1", len(jobs.got))
	}
	p := jobs.got[0]
	if !p.Scheduled || p.FormerOwner != "usr-1" || p.RestoreRef != "" {
		t.Errorf("params = %+v", p)
	}
	if !strings.HasPrefix(p.JobName, BackupJobName("survival")+"-") {
		t.Errorf("JobName %q", p.JobName)
	}

	if err := b.Backup(context.Background(), "survival", "usr-1"); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if jobs.got[1].Scheduled {
		t.Error("an on-demand backup is marked scheduled")
	}
}

func TestBackupLiveReturnsTheJobItCreated(t *testing.T) {
	jobs := &captureJobs{}
	b := &Backuper{Jobs: jobs, Config: Config{Image: "img", BackupPVC: "pvc", StageLimit: "8Gi"}}
	name, err := b.BackupLive(context.Background(), "survival", "usr-1", ReasonScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.got) != 1 {
		t.Fatalf("created %d jobs, want 1", len(jobs.got))
	}
	p := jobs.got[0]
	if !p.Live || p.StageLimit != "8Gi" || !p.Scheduled || p.FormerOwner != "usr-1" || p.JobName != name || name == "" {
		t.Errorf("params = %+v, returned %q", p, name)
	}
	if _, err := b.BackupLive(context.Background(), "survival", "usr-1", ""); err != nil || jobs.got[1].Scheduled || jobs.got[1].Replica {
		t.Errorf("an on-demand live backup = %+v (%v)", jobs.got[1], err)
	}
	if _, err := b.BackupLive(context.Background(), "survival", "usr-1", ReasonReplica); err != nil || jobs.got[2].Scheduled || !jobs.got[2].Replica {
		t.Errorf("a replica = %+v (%v)", jobs.got[2], err)
	}
}
