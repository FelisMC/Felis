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

// limitHeapToCgroup sets the Go heap's soft limit from the container's memory
// limit, so the collector works harder as a Job nears it and the kernel does not
// kill the Job first. An extraction or a folder zipped for download keeps a few
// hundred bytes per entry for as long as it runs; without the limit the heap
// grows to twice that before a collection, and a 256 MiB Job was killed at
// 400,000 entries whose live heap was 115 MB. GOMEMLIMIT set by hand wins.
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

// softMemoryLimit answers three fifths of the limit a cgroup memory file holds,
// or false for "max" (no limit) and anything unreadable. The rest is left for
// what the kernel charges the container beyond the Go heap: the page cache of
// the files it reads and writes, and the inodes it creates. Under a 256 MiB
// limit, 400,000 extracted entries peaked at 184 MB resident with the heap held
// to 150 MiB.
func softMemoryLimit(content string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(content), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n / 5 * 3, true
}
