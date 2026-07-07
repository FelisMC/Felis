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
