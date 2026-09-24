package main

import (
	"bytes"
	"strings"
	"testing"
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
