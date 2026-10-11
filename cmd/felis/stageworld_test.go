package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStageWorldCopiesTheFrozenWorld pins what the archive step will see: every file with
// its bytes, mode and time, directories and symlinks, and nothing that is not a file.
func TestStageWorldCopiesTheFrozenWorld(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	write := func(rel, body string, mode os.FileMode) {
		p := filepath.Join(from, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	write("world/region/r.0.0.mca", strings.Repeat("R", 70000), 0o644)
	write("world/level.dat", "level", 0o600)
	write("server.properties", "motd=hi", 0o640)
	write("logs/latest.log", "line\n", 0o644)
	if err := os.Symlink("world/level.dat", filepath.Join(from, "level-link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(from, "console.pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(from, "world"), when, when); err != nil {
		t.Fatal(err)
	}

	st, err := stageWorld(from, to)
	if err != nil {
		t.Fatal(err)
	}
	if st.files != 4 || st.bytes != 70000+5+7+5 {
		t.Errorf("stats = %+v", st)
	}
	for rel, want := range map[string]string{"world/region/r.0.0.mca": strings.Repeat("R", 70000), "world/level.dat": "level", "server.properties": "motd=hi", "logs/latest.log": "line\n"} {
		got, err := os.ReadFile(filepath.Join(to, rel))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v)", rel, got, err)
		}
	}
	if info, err := os.Stat(filepath.Join(to, "world/level.dat")); err != nil || info.Mode().Perm() != 0o600 || !info.ModTime().Equal(when) {
		t.Errorf("level.dat mode/time = %v (%v)", info, err)
	}
	if info, err := os.Stat(filepath.Join(to, "world")); err != nil || !info.ModTime().Equal(when) {
		t.Errorf("a directory lost its time: %v (%v)", info, err)
	}
	if target, err := os.Readlink(filepath.Join(to, "level-link")); err != nil || target != "world/level.dat" {
		t.Errorf("symlink = %q (%v)", target, err)
	}
	if _, err := os.Lstat(filepath.Join(to, "console.pipe")); !os.IsNotExist(err) {
		t.Errorf("a pipe was copied: %v", err)
	}
}

// TestStageWorldRefusesWhatItCannotHold pins the two refusals: a scratch directory that
// is not empty, and one without room for the world.
func TestStageWorldRefusesWhatItCannotHold(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(from, "level.dat"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(to, "stale"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := stageWorld(from, to); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("a non-empty scratch directory = %v", err)
	}
	os.Remove(filepath.Join(to, "stale"))
	saved := stageFreeBytes
	t.Cleanup(func() { stageFreeBytes = saved })
	stageFreeBytes = func(string) (int64, error) { return 5, nil }
	if _, err := stageWorld(from, to); err == nil || !strings.Contains(err.Error(), "free") {
		t.Errorf("a scratch space too small = %v", err)
	}
	if entries, _ := os.ReadDir(to); len(entries) != 0 {
		t.Errorf("a refused copy wrote %d entries", len(entries))
	}
}
