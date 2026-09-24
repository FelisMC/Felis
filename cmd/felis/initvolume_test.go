package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
)

// openTree builds a small world under a temp dir: nested dirs, a file, and a
// symlink pointing out of the volume that the walk must not follow.
func openTree(t *testing.T) *os.Root {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"world/region", "plugins"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"level.dat", "world/region/r.0.0.mca"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "plugins", "escape")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

// Every entry owned by someone else is handed over, the root dir included, and a
// symlink is re-owned as a link rather than walked into.
func TestChownTreeHandsOverMismatchedEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ownership not represented on Windows")
	}
	root := openTree(t)
	var got []string
	res, err := chownTree(root, os.Getuid()+1, os.Getgid(), func(name string, uid, gid int) error {
		got = append(got, name)
		return nil
	})
	if err != nil {
		t.Fatalf("chownTree: %v", err)
	}
	want := []string{".", "level.dat", "plugins", "plugins/escape", "world", "world/region", "world/region/r.0.0.mca"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("chowned %v, want %v", got, want)
	}
	if res.changed != len(want) || res.checked != len(want) || len(res.failures) != 0 {
		t.Errorf("result = %+v, want %d checked and changed, no failures", res, len(want))
	}
}

// A volume already owned by the game uid costs no chown at all: this is the steady
// state every restart after the first one hits.
func TestChownTreeSkipsMatchingOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ownership not represented on Windows")
	}
	root := openTree(t)
	calls := 0
	res, err := chownTree(root, os.Getuid(), os.Getgid(), func(string, int, int) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("chownTree: %v", err)
	}
	if calls != 0 || res.changed != 0 {
		t.Errorf("chown called %d times on an already-owned tree (result %+v)", calls, res)
	}
}

// One entry that refuses the chown is reported and the walk carries on: a single
// odd file must not keep the whole server from starting.
func TestChownTreeContinuesPastFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ownership not represented on Windows")
	}
	root := openTree(t)
	res, err := chownTree(root, os.Getuid()+1, os.Getgid(), func(name string, uid, gid int) error {
		if name == "level.dat" {
			return os.ErrPermission
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chownTree: %v", err)
	}
	if len(res.failures) != 1 || res.changed != 6 {
		t.Errorf("result = %+v, want 1 failure and 6 changed", res)
	}
}

// Against a real directory the owner already matches, so the command succeeds
// without needing CAP_CHOWN — the path every test runner (non-root) can take.
func TestCmdInitVolumeOwnedTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ownership not represented on Windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := cmdInitVolume([]string{"--data", dir, "--uid", strconv.Itoa(os.Getuid()), "--gid", strconv.Itoa(os.Getgid())}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if code := cmdInitVolume([]string{"--data", filepath.Join(dir, "missing")}, &out, &errb); code != 1 {
		t.Errorf("missing data dir exit = %d, want 1", code)
	}
}
