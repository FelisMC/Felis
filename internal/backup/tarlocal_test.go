package backup_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

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

	a, err := archiver.Archive(ctx, "survival", "src-pvc")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	ref, size := a.Ref, a.Size
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

	a, err := archiver.Archive(ctx, "survival", "src-pvc")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	ref := a.Ref
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
	if _, err := archiver.Archive(context.Background(), "x", "missing"); err == nil {
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

// TestTarLocalArchiveIsDurableAndChecksummed pins the write path: the archive
// lands under its final name only, no .partial is left beside it, and the
// SHA256 it reports is the one of the bytes on disk, which Verify reads back.
func TestTarLocalArchiveIsDurableAndChecksummed(t *testing.T) {
	src, backupRoot := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{"level.dat": "seed", "region/r.0.0.mca": "chunk"})
	archiver := &backup.TarLocal{BackupRoot: backupRoot, Resolve: backup.StaticResolver(map[string]string{"src": src})}
	ctx := context.Background()

	a, err := archiver.Archive(ctx, "survival", "src")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	ents, err := os.ReadDir(backupRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || filepath.Join(backupRoot, ents[0].Name()) != string(a.Ref) {
		t.Fatalf("backup root holds %v, want only %s", ents, a.Ref)
	}
	body, err := os.ReadFile(string(a.Ref))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if a.SHA256 != hex.EncodeToString(sum[:]) || a.Size != int64(len(body)) {
		t.Errorf("Archived = %+v, want sha256 %x size %d", a, sum, len(body))
	}
	got, err := archiver.Verify(ctx, a.Ref, a.SHA256)
	if err != nil || got != a.SHA256 {
		t.Errorf("Verify = %q, %v; want %q, nil", got, err, a.SHA256)
	}
	// With no digest recorded, Verify still reads it through and reports one.
	if got, err := archiver.Verify(ctx, a.Ref, ""); err != nil || got != a.SHA256 {
		t.Errorf("Verify without a digest = %q, %v", got, err)
	}
}

// TestTarLocalFailedArchiveLeavesNothing: an Archive that fails part way (here
// a cancelled context) removes its .partial, so no leftover can pass for an
// archive.
func TestTarLocalFailedArchiveLeavesNothing(t *testing.T) {
	src, backupRoot := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{"level.dat": "seed"})
	archiver := &backup.TarLocal{BackupRoot: backupRoot, Resolve: backup.StaticResolver(map[string]string{"src": src})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := archiver.Archive(ctx, "survival", "src"); err == nil {
		t.Fatal("Archive with a cancelled context succeeded")
	}
	if ents, _ := os.ReadDir(backupRoot); len(ents) != 0 {
		t.Errorf("failed Archive left %v behind", ents)
	}
}

// TestTarLocalKeepsModesAndTimes: a restore gives files and directories back
// their permission bits (setuid, setgid and sticky dropped) and modification
// times, and what the archive cannot hold is reported rather than dropped
// silently.
func TestTarLocalKeepsModesAndTimes(t *testing.T) {
	src, dst, backupRoot := t.TempDir(), t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{
		"start.sh":          "#!/bin/sh",
		"secret.properties": "rcon",
		"private/notes.txt": "n",
		"setuid-bin":        "x",
	})
	mtime := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	modes := map[string]os.FileMode{
		"start.sh":          0o755,
		"secret.properties": 0o600,
		"private/notes.txt": 0o640,
		"setuid-bin":        0o755 | os.ModeSetuid,
		"private":           0o700,
	}
	for rel, m := range modes {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{"start.sh", "secret.properties", "private/notes.txt", "setuid-bin", "private"} {
		if err := os.Chtimes(filepath.Join(src, filepath.FromSlash(rel)), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	archiver := &backup.TarLocal{BackupRoot: backupRoot, Resolve: backup.StaticResolver(map[string]string{"src": src, "dst": dst})}
	ctx := context.Background()
	a, err := archiver.Archive(ctx, "survival", "src")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if !reflect.DeepEqual(a.Skipped, []string{"link"}) {
		t.Errorf("Skipped = %v, want [link]", a.Skipped)
	}
	if err := archiver.Restore(ctx, a.Ref, "dst"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for rel, m := range modes {
		info, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if got, want := info.Mode().Perm(), m.Perm(); got != want || info.Mode()&os.ModeSetuid != 0 {
			t.Errorf("%s restored with mode %v, want %v", rel, info.Mode(), want)
		}
		if !info.ModTime().Equal(mtime) {
			t.Errorf("%s restored with mtime %v, want %v", rel, info.ModTime(), mtime)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); !os.IsNotExist(err) {
		t.Errorf("symlink came back from the archive: %v", err)
	}
}

// damage rewrites the archive at ref with f applied to its bytes.
func damage(t *testing.T, ref backup.ArchiveRef, f func([]byte) []byte) {
	t.Helper()
	b, err := os.ReadFile(string(ref))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(string(ref), f(b), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTarLocalVerifyFindsCorruption: a flipped byte, a truncated file, a digest
// that no longer matches and a missing file all come back as ErrCorrupt.
func TestTarLocalVerifyFindsCorruption(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"level.dat": "seed", "region/r.0.0.mca": string(make([]byte, 64<<10))})
	ctx := context.Background()
	fresh := func(t *testing.T) (*backup.TarLocal, backup.Archived) {
		archiver := &backup.TarLocal{BackupRoot: t.TempDir(), Resolve: backup.StaticResolver(map[string]string{"src": src})}
		a, err := archiver.Archive(ctx, "survival", "src")
		if err != nil {
			t.Fatalf("Archive: %v", err)
		}
		return archiver, a
	}
	cases := map[string]struct {
		change func(*testing.T, backup.Archived)
		want   func(backup.Archived) string
	}{
		"flipped byte": {
			change: func(t *testing.T, a backup.Archived) {
				damage(t, a.Ref, func(b []byte) []byte { b[len(b)/2] ^= 0xff; return b })
			},
			want: func(a backup.Archived) string { return "" },
		},
		"truncated": {
			change: func(t *testing.T, a backup.Archived) {
				damage(t, a.Ref, func(b []byte) []byte { return b[:len(b)-9] })
			},
			want: func(a backup.Archived) string { return "" },
		},
		"digest mismatch": {
			change: func(*testing.T, backup.Archived) {},
			want:   func(backup.Archived) string { return hex.EncodeToString(make([]byte, 32)) },
		},
		"missing": {
			change: func(t *testing.T, a backup.Archived) {
				if err := os.Remove(string(a.Ref)); err != nil {
					t.Fatal(err)
				}
			},
			want: func(a backup.Archived) string { return a.SHA256 },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			archiver, a := fresh(t)
			tc.change(t, a)
			if _, err := archiver.Verify(ctx, a.Ref, tc.want(a)); !errors.Is(err, backup.ErrCorrupt) {
				t.Errorf("Verify = %v, want ErrCorrupt", err)
			}
		})
	}
}

// TestTarLocalRestoreRefusesCorruptArchive: the archive is read back before
// anything is extracted, so a corrupt one leaves the world exactly as it was.
func TestTarLocalRestoreRefusesCorruptArchive(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{"level.dat": "archived", "region/r.0.0.mca": string(make([]byte, 64<<10))})
	writeTree(t, dst, map[string]string{"level.dat": "live"})
	archiver := &backup.TarLocal{BackupRoot: t.TempDir(), Resolve: backup.StaticResolver(map[string]string{"src": src, "dst": dst})}
	ctx := context.Background()
	a, err := archiver.Archive(ctx, "survival", "src")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	damage(t, a.Ref, func(b []byte) []byte { return b[:len(b)-9] })
	if err := archiver.Restore(ctx, a.Ref, "dst"); !errors.Is(err, backup.ErrCorrupt) {
		t.Fatalf("Restore = %v, want ErrCorrupt", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "level.dat"))
	if err != nil || string(got) != "live" {
		t.Errorf("world changed by a refused restore: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "region")); !os.IsNotExist(err) {
		t.Errorf("refused restore extracted entries: %v", err)
	}
}

// TestTarLocalSweep removes old .partial files and orphaned archives past the
// orphan cutoff, reports younger orphans, and leaves everything else: fresh
// leftovers (an Archive may still be writing, or its record not yet inserted),
// claimed archives, and files it did not write.
func TestTarLocalSweep(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	ancient, old, recent := now.Add(-100*24*time.Hour), now.Add(-7*time.Hour), now.Add(-time.Minute)
	files := map[string]time.Time{
		".survival-100.tar.gz.partial": old,
		".survival-200.tar.gz.partial": recent,
		"survival-300.tar.gz":          ancient, // orphan past the cutoff
		"survival-310.tar.gz":          old,     // orphan, kept and reported
		"survival-400.tar.gz":          ancient, // claimed
		"survival-500.tar.gz":          recent,  // may be about to be claimed
		"notes.txt":                    ancient,
		"felis.dump":                   ancient,
	}
	for name, at := range files {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte("xyz"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	archiver := &backup.TarLocal{BackupRoot: root}
	live := func(ref backup.ArchiveRef) bool {
		return ref == backup.ArchiveRef(filepath.Join(root, "survival-400.tar.gz"))
	}
	got, err := archiver.Sweep(context.Background(), live, now.Add(-6*time.Hour), now.Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	sort.Strings(got.Removed)
	want := backup.Swept{
		Removed:     []string{filepath.Join(root, ".survival-100.tar.gz.partial"), filepath.Join(root, "survival-300.tar.gz")},
		Orphans:     []string{filepath.Join(root, "survival-310.tar.gz")},
		OrphanBytes: 3,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Sweep = %+v, want %+v", got, want)
	}
	ents, _ := os.ReadDir(root)
	if len(ents) != len(files)-2 {
		t.Errorf("%d files left, want %d", len(ents), len(files)-2)
	}
	// A store that does not exist yet has nothing to sweep.
	if _, err := (&backup.TarLocal{BackupRoot: filepath.Join(root, "absent")}).Sweep(context.Background(), live, now, now); err != nil {
		t.Errorf("Sweep of a missing root: %v", err)
	}
}
