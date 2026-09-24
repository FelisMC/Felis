package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"felis.lolicon.best/internal/backup"
)

// archiveTempWorld tars a freshly populated world directory with the real
// TarLocal and returns the absolute archive ref plus the backup root it lives
// under, so the restore round-trip below exercises production code end to end
// without a cluster.
func archiveTempWorld(t *testing.T, files map[string]string) (ref string, backupRoot string) {
	t.Helper()
	srcDir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(srcDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o640); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	backupRoot = t.TempDir()
	ar := &backup.TarLocal{
		BackupRoot: backupRoot,
		Resolve:    func(string) (string, error) { return srcDir, nil },
	}
	got, err := ar.Archive(context.Background(), "survival", "world-survival-0")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	return string(got.Ref), backupRoot
}

// The restore subcommand must extract the archived world into the target world
// mount — the real reverse of what the reaper wrote.
func TestRestoreSubcommandRoundTrips(t *testing.T) {
	files := map[string]string{
		"level.dat":            "world-seed",
		"region/r.0.0.mca":     "chunk-bytes",
		"playerdata/uuid.json": "{}",
	}
	ref, backupRoot := archiveTempWorld(t, files)

	// Seed the target with a stale file the archive does not contain: the restore
	// must prune it (replace semantics), not leave it behind. This is the path the
	// admin/owner rollback-onto-a-stopped-populated-PVC case actually takes.
	targetDir := t.TempDir()
	stale := filepath.Join(targetDir, "region", "r.9.9.mca")
	if err := os.MkdirAll(filepath.Dir(stale), 0o750); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	if err := os.WriteFile(stale, []byte("griefer-chunk"), 0o640); err != nil {
		t.Fatalf("seed stale: %v", err)
	}

	var out, errBuf bytes.Buffer
	code := run([]string{
		"restore",
		"--server", "survival",
		"--ref", ref,
		"--archive-store", "tarLocal",
		"--backup-root", backupRoot,
		"--worlds-root", targetDir,
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("restore exit = %d, stderr=%q", code, errBuf.String())
	}

	for name, want := range files {
		full := filepath.Join(targetDir, filepath.FromSlash(name))
		got, err := os.ReadFile(full)
		if err != nil {
			t.Errorf("restored file %q missing: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("restored %q = %q, want %q", name, got, want)
		}
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file survived restore (err=%v); replace semantics broken", err)
	}
}

// A ref outside the backup mount must be rejected before any file is opened —
// the restore Pod must never read an arbitrary host path.
func TestRestoreSubcommandRejectsRefOutsideBackupRoot(t *testing.T) {
	backupRoot := t.TempDir()
	outside := filepath.Join(t.TempDir(), "evil.tar.gz")

	var out, errBuf bytes.Buffer
	code := run([]string{
		"restore",
		"--server", "survival",
		"--ref", outside,
		"--backup-root", backupRoot,
		"--worlds-root", t.TempDir(),
	}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for an out-of-root ref", code)
	}
	if !strings.Contains(errBuf.String(), "not under backup root") {
		t.Errorf("expected containment error, got %q", errBuf.String())
	}
}

// An unimplemented archive store must fail loudly, not silently no-op.
func TestRestoreSubcommandRejectsUnknownStore(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{
		"restore",
		"--ref", "/backups/x.tar.gz",
		"--archive-store", "s3snapshot",
		"--backup-root", "/backups",
	}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for an unknown store", code)
	}
	if !strings.Contains(errBuf.String(), "not implemented") {
		t.Errorf("expected not-implemented error, got %q", errBuf.String())
	}
}

// --ref is mandatory: a missing ref is a usage error, not a panic.
func TestRestoreSubcommandRequiresRef(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"restore", "--backup-root", "/backups"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 for a missing --ref", code)
	}
}
