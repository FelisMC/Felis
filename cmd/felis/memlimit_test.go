package main

import (
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
)

func TestSoftMemoryLimit(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"268435456\n", 161061273, true}, // 256 MiB, as memory.max holds it
		{"max\n", 0, false},
		{"0\n", 0, false},
		{"-1", 0, false},
		{"", 0, false},
	} {
		got, ok := softMemoryLimit(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("softMemoryLimit(%q) = %d %v, want %d %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestLimitHeapToCgroup checks the limit comes from the first cgroup file there
// is, and that GOMEMLIMIT set by hand leaves the heap alone.
func TestLimitHeapToCgroup(t *testing.T) {
	prevFiles, prevLimit := cgroupMemoryFiles, debug.SetMemoryLimit(-1)
	t.Cleanup(func() { cgroupMemoryFiles = prevFiles; debug.SetMemoryLimit(prevLimit) })
	dir := t.TempDir()
	v1, v1b := filepath.Join(dir, "v1"), filepath.Join(dir, "v1b")
	if err := os.WriteFile(v1, []byte("268435456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v1b, []byte("536870912\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cgroupMemoryFiles = []string{filepath.Join(dir, "missing"), v1, v1b}

	t.Setenv("GOMEMLIMIT", "")
	debug.SetMemoryLimit(math.MaxInt64)
	limitHeapToCgroup()
	if got := debug.SetMemoryLimit(-1); got != 161061273 {
		t.Fatalf("limit = %d, want three fifths of 256 MiB", got)
	}

	t.Setenv("GOMEMLIMIT", "1GiB")
	debug.SetMemoryLimit(math.MaxInt64)
	limitHeapToCgroup()
	if got := debug.SetMemoryLimit(-1); got != math.MaxInt64 {
		t.Fatalf("limit = %d with GOMEMLIMIT set, want it left alone", got)
	}
}
