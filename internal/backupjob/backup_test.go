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

// The core fix: a repeat on-demand backup of the same server must not collide with
// the previous Job's name (which would silently no-op the retry within the TTL
// window). Two Backup calls must mint two distinct Job names, both under the
// server's base prefix.
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
