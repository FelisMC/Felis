package main

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// cgroupMemoryFiles are where a container reads the memory it is allowed:
// cgroup v2 first, then v1.
var cgroupMemoryFiles = []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"}

// Bound the Go heap below the cgroup limit to avoid extraction OOMs.
// GOMEMLIMIT set by hand wins.
func limitHeapToCgroup() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	for _, f := range cgroupMemoryFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if n, ok := softMemoryLimit(string(b)); ok {
			debug.SetMemoryLimit(n)
		}
		return
	}
}

// Reserve two fifths of cgroup memory for page cache and other kernel charges.
func softMemoryLimit(content string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(content), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n / 5 * 3, true
}
