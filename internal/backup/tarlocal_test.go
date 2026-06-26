package backup_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"felis.lolicon.best/internal/backup"
)

// writeTree creates files (path->content) under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o640); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func TestTarLocalRoundTrip(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	backupRoot := t.TempDir()

	want := map[string]string{
		"level.dat":           "world-seed-and-spawn",
		"region/r.0.0.mca":    "chunk-bytes-aaaa",
		"data/scoreboard.dat": "{}",
		"playerdata/uuid.dat": "player-state",
	}
	writeTree(t, src, want)

	archiver := &backup.TarLocal{
		BackupRoot: backupRoot,
		Resolve: backup.StaticResolver(map[string]string{
			"src-pvc": src,
			"dst-pvc": dst,
		}),
	}
	ctx := context.Background()

	ref, size, err := archiver.Archive(ctx, "survival", "src-pvc")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if size <= 0 {
		t.Errorf("archive size = %d, want > 0", size)
	}
	if _, err := os.Stat(string(ref)); err != nil {
		t.Fatalf("archive file missing: %v", err)
	}

	if err := archiver.Restore(ctx, ref, "dst-pvc"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("restored file %s missing: %v", rel, err)
			continue
		}
		if string(got) != content {
			t.Errorf("restored %s = %q, want %q", rel, got, content)
		}
	}

	if err := archiver.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(string(ref)); !os.IsNotExist(err) {
		t.Errorf("archive still present after Delete: %v", err)
	}
	// Delete of an already-gone archive is a no-op.
	if err := archiver.Delete(ctx, ref); err != nil {
		t.Errorf("second Delete should be a no-op, got %v", err)
	}
}

// TestTarLocalRestoreReplacesTarget pins replace semantics (spec §466): after a
// restore the world must equal the archive, not be merged onto whatever the
// target already held. It restores over a populated target and asserts that
//
//	(a) archive files are present with the archive's content (overwriting stale
//	    copies),
//	(b) files the archive did not contain are gone — including a stale chunk
//	    inside a directory the archive *does* keep, which proves per-file prune
//	    within a surviving dir and exercises the rel-path normalization, and
//	(c) a pre-existing lost+found/ with a file inside survives untouched — the
//	    never-delete invariant for the filesystem artifact a non-root restore
//	    Pod cannot remove.
//
// Honesty: this runs as the test user (which *can* delete anything), so it
// proves the prune logic and the lost+found skip but does NOT exercise the
// non-root / FSGroup runtime path. "Restore works as a non-root Pod on a real
// ext4 PVC" remains code-complete-but-unverified (same bucket as the K8s E2E).
func TestTarLocalRestoreReplacesTarget(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	backupRoot := t.TempDir()

	archived := map[string]string{
		"level.dat":           "new-seed",
		"region/r.0.0.mca":    "good-chunk-00",
		"region/nested/a.mca": "good-chunk-nested",
		"playerdata/uuid.dat": "player-state",
	}
	writeTree(t, src, archived)

	// The target already holds an older, divergent world: a stale copy of a file
	// the archive also has, a griefer chunk inside a kept dir, and a whole stale
	// directory the archive never mentions.
	writeTree(t, dst, map[string]string{
		"level.dat":         "OLD-seed-overwrite-me",
		"region/r.9.9.mca":  "griefer-chunk-must-vanish",
		"oldworld/junk.dat": "whole-stale-dir-must-vanish",
	})
	// A pre-existing lost+found with content the prune must never touch.
	if err := os.MkdirAll(filepath.Join(dst, "lost+found"), 0o700); err != nil {
		t.Fatalf("mkdir lost+found: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "lost+found", "0001"), []byte("fsck-recovered"), 0o600); err != nil {
		t.Fatalf("seed lost+found: %v", err)
	}

	archiver := &backup.TarLocal{
		BackupRoot: backupRoot,
		Resolve: backup.StaticResolver(map[string]string{
			"src-pvc": src,
			"dst-pvc": dst,
		}),
	}
	ctx := context.Background()

	ref, _, err := archiver.Archive(ctx, "survival", "src-pvc")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := archiver.Restore(ctx, ref, "dst-pvc"); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// (a) Every archive file present with the archive's content.
	for rel, want := range archived {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("archive file %s missing after restore: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("restored %s = %q, want %q", rel, got, want)
		}
	}

	// (b) Files absent from the archive are gone — both the stale chunk inside the
	// kept region/ dir and the whole stale directory.
	for _, gone := range []string{"region/r.9.9.mca", "oldworld/junk.dat", "oldworld"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("stale entry %s survived the restore (err=%v); replace semantics broken", gone, err)
		}
	}

	// (c) lost+found and its contents survive untouched.
	lf, err := os.ReadFile(filepath.Join(dst, "lost+found", "0001"))
	if err != nil {
		t.Errorf("lost+found content was removed: %v", err)
	} else if string(lf) != "fsck-recovered" {
		t.Errorf("lost+found content = %q, want %q", lf, "fsck-recovered")
	}
}

func TestTarLocalUnknownPVC(t *testing.T) {
	archiver := &backup.TarLocal{
		BackupRoot: t.TempDir(),
		Resolve:    backup.StaticResolver(map[string]string{}),
	}
	if _, _, err := archiver.Archive(context.Background(), "x", "missing"); err == nil {
		t.Fatal("expected error for unknown pvc")
	}
}

// TestTarLocalRejectsZipSlip crafts a malicious archive whose entry escapes the
// target directory and asserts Restore refuses it.
func TestTarLocalRejectsZipSlip(t *testing.T) {
	backupRoot := t.TempDir()
	dst := t.TempDir()
	evil := filepath.Join(backupRoot, "evil.tar.gz")

	f, err := os.Create(evil)
	if err != nil {
		t.Fatalf("create evil archive: %v", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := []byte("pwned")
	if err := tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o640, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write body: %v", err)
	}
	tw.Close()
	gz.Close()
	f.Close()

	archiver := &backup.TarLocal{
		BackupRoot: backupRoot,
		Resolve:    backup.StaticResolver(map[string]string{"dst-pvc": dst}),
	}
	if err := archiver.Restore(context.Background(), backup.ArchiveRef(evil), "dst-pvc"); err == nil {
		t.Fatal("Restore must reject a path-traversal archive")
	}
	// Ensure nothing was written outside the target.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "escape.txt")); !os.IsNotExist(err) {
		t.Errorf("zip-slip wrote outside target: %v", err)
	}
}
