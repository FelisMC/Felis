package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/reaper"
)

// The reason decides which backups the new one's prune may remove, so an
// unknown one is refused before anything is archived.
func TestBackupSubcommandRejectsUnknownReason(t *testing.T) {
	var stderr bytes.Buffer
	if code := cmdBackup([]string{"--server", "survival", "--reason", "inactive_15d"}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2 (%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown --reason") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// Each reason is pruned and expired by its own [archive] keys: a daily
// restore point must never count against, or take the lifetime of, the
// backups an owner asked for.
func TestBackupPolicyPerReason(t *testing.T) {
	rcfg := reaper.DefaultConfig()
	rcfg.ManualKeep, rcfg.ManualRetention = 5, 30*reaper.Day
	rcfg.ScheduledKeep, rcfg.ScheduledRetention = 7, 90*reaper.Day
	for _, tc := range []struct {
		reason    string
		keep      int
		retention time.Duration
	}{
		{"manual", 5, 30 * reaper.Day},
		{"pre_restore", preRestoreKeep, 30 * reaper.Day},
		{"scheduled", 7, 90 * reaper.Day},
	} {
		keep, retention, ok := backupPolicy(tc.reason, rcfg)
		if !ok || keep != tc.keep || retention != tc.retention {
			t.Errorf("backupPolicy(%q) = (%d, %v, %v); want (%d, %v, true)", tc.reason, keep, retention, ok, tc.keep, tc.retention)
		}
	}
	if _, _, ok := backupPolicy("inactive_15d", rcfg); ok {
		t.Error("backupPolicy accepted inactive_15d; the reaper records those itself")
	}
}
