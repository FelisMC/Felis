package fileedit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"felis.lolicon.best/internal/placement"
	"golang.org/x/sys/unix"
)

func diskAllocated(t *testing.T, name string) int64 {
	t.Helper()
	info, err := os.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("allocated blocks unavailable")
	}
	return stat.Blocks * 512
}

func TestDiskUsageAllocatedBytesAndPrivateFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"region/sparse.mca", "config/paper-global.yml", "server.properties", ".felis-preserved-fixture/old"} {
		putTransferFile(t, root, name, "fake-private-value")
	}
	file, err := os.OpenFile(filepath.Join(root, "region/sparse.mca"), os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(64 << 20); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "region/sparse.mca"), filepath.Join(root, "config/same-inode")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	putTransferFile(t, outside, "external", strings.Repeat("outside", 8192))
	for name, target := range map[string]string{"outside-link": outside, "inside-link": "region", "cycle": ".", "dangling": "absent"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	var want int64
	for _, name := range []string{".", "region", "region/sparse.mca", "config", "config/paper-global.yml", "server.properties", ".felis-preserved-fixture", ".felis-preserved-fixture/old", "outside-link", "inside-link", "cycle", "dangling", "pipe"} {
		want += diskAllocated(t, filepath.Join(root, name))
	}
	if want >= 64<<20 {
		t.Fatal("fixture did not produce a sparse allocation")
	}
	res, err := Execute(root, Request{Op: OpDiskUsage})
	if err != nil || res.Code != "" || res.UsedBytes == nil || *res.UsedBytes != want {
		t.Fatalf("allocated disk usage: result=%+v err=%v want=%d", res, err, want)
	}
	raw := mustPayload(t, res)
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil || len(response) != 1 || response["used_bytes"] == nil || strings.Contains(string(raw), "fake-private") {
		t.Fatal("disk usage response contains more than its byte count")
	}
	res, err = Execute(root, Request{Op: OpDiskUsage, Path: "region"})
	if err != nil || res.Code != CodeBadPath || res.UsedBytes != nil {
		t.Fatalf("subdirectory sampling accepted: %+v, %v", res, err)
	}
}

func TestDiskUsageStreamsLargeDirectories(t *testing.T) {
	root := t.TempDir()
	const count = 3*MaxEntries + 1
	var want int64
	for i := range count {
		name := filepath.Join(root, fmt.Sprintf("entry-%04d", i))
		if err := os.WriteFile(name, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		want += diskAllocated(t, name)
	}
	putTransferFile(t, root, "region/real.mca", strings.Repeat("x", 8192))
	want += diskAllocated(t, root) + diskAllocated(t, filepath.Join(root, "region")) + diskAllocated(t, filepath.Join(root, "region/real.mca"))
	res, err := Execute(root, Request{Op: OpDiskUsage})
	if err != nil || res.Code != "" || res.UsedBytes == nil || *res.UsedBytes != want {
		t.Fatalf("large directory was not fully sampled: result=%+v err=%v want=%d", res, err, want)
	}
}

func TestDiskUsageLimitsReturnNoPartialSuccess(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, "first", "allocated")
	putTransferFile(t, root, "second", "allocated")
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, tc := range []struct {
		name     string
		entries  int
		deadline time.Time
	}{
		{"entry budget", 2, time.Now().Add(time.Minute)},
		{"expired before scan", 100, time.Now().Add(-time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			used, err := measureDiskUsage(r, tc.entries, tc.deadline)
			if !errors.Is(err, errDiskUsageLimit) || used != 0 {
				t.Fatalf("incomplete scan returned bytes=%d error=%v", used, err)
			}
		})
	}
	deep := root
	for range diskUsageMaxDepth + 1 {
		deep = filepath.Join(deep, "d")
		if err := os.Mkdir(deep, 0700); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Execute(root, Request{Op: OpDiskUsage})
	if err != nil || res.Code != CodeTooLarge || res.UsedBytes != nil || res.Entry != "" || strings.Contains(res.Error, "first") {
		t.Fatalf("depth limit exposed a partial sample: %+v, %v", res, err)
	}
}

func TestDiskUsageRejectsCrossDeviceAndOverflow(t *testing.T) {
	for _, tc := range []struct {
		name string
		used int64
		stat unix.Stat_t
		want error
	}{
		{"different device", 4096, unix.Stat_t{Dev: 2, Ino: 3, Mode: unix.S_IFREG, Nlink: 1, Blocks: 8}, errDiskUsageBoundary},
		{"total overflow", math.MaxInt64 - 512, unix.Stat_t{Dev: 1, Ino: 3, Mode: unix.S_IFREG, Nlink: 1, Blocks: 2}, errDiskUsageOverflow},
		{"block multiplication overflow", 0, unix.Stat_t{Dev: 1, Ino: 3, Mode: unix.S_IFREG, Nlink: 1, Blocks: math.MaxInt64}, errDiskUsageOverflow},
		{"negative blocks", 4096, unix.Stat_t{Dev: 1, Ino: 3, Mode: unix.S_IFREG, Nlink: 1, Blocks: -1}, errDiskUsageOverflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scan := diskUsageScan{deadline: time.Now().Add(time.Minute), device: 1, remaining: 100, used: tc.used, seen: map[diskInode]struct{}{}}
			if err := scan.add(tc.stat); !errors.Is(err, tc.want) || scan.used != tc.used {
				t.Fatalf("invalid metadata changed total: bytes=%d err=%v", scan.used, err)
			}
		})
	}
}

func TestDiskUsageRefusesReplacedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	var before unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), "child", &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "child"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("moved", filepath.Join(root, "child")); err != nil {
		t.Fatal(err)
	}
	scan := diskUsageScan{deadline: time.Now().Add(time.Minute), device: uint64(before.Dev), remaining: 100, seen: map[diskInode]struct{}{}}
	if err := scan.walkChild(parent, "child", before, 1); err == nil {
		t.Fatal("a directory replaced by a symbolic link was followed")
	}
	if err := os.Remove(filepath.Join(root, "child")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := scan.walkChild(parent, "child", before, 1); !errors.Is(err, errDiskUsageChanged) {
		t.Fatalf("replacement directory identity accepted: %v", err)
	}
}

func TestEditorDiskUsageValidatesCompleteSample(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result Result
		want   int64
		err    error
	}{
		{"zero", Result{UsedBytes: new(int64)}, 0, nil},
		{"allocated", Result{UsedBytes: new(int64(4096))}, 4096, nil},
		{"missing", Result{}, 0, ErrReadDamaged},
		{"negative", Result{UsedBytes: new(int64(-1))}, 0, ErrReadDamaged},
		{"failed with partial bytes", Result{Code: CodeConflict, UsedBytes: new(int64(4096))}, 0, ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{payload: mustPayload(t, tc.result)}
			editor := &Editor{Runner: runner, Config: Config{Image: "img", ResolveWorld: func(_ context.Context, server string) (placement.World, error) {
				if server != "survival" {
					t.Fatal("wrong server resolved")
				}
				return placement.World{Claim: "world-survival-ha", Node: "worker"}, nil
			}}}
			used, err := editor.DiskUsage(context.Background(), "survival")
			if !errors.Is(err, tc.err) || used != tc.want {
				t.Fatalf("disk sample=%d, err=%v", used, err)
			}
			p := runner.got[0]
			if p.Op != OpDiskUsage || p.Path != "" || p.To != "" || p.WorldPVC != "world-survival-ha" || p.NodeName != "worker" || len(p.Content) != 0 {
				t.Fatalf("disk sample params: %+v", p)
			}
		})
	}
}
