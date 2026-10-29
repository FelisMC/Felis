package hibernate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKernel answers cgroupfs writes the way the kernel would: freezing flips cgroup.events and
// reclaim moves up to the requested anonymous memory into swap, keeping back what the swap device cannot take.
type fakeKernel struct {
	cg       string
	keepBack int64
	stop     chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	maxAsk   int64
}

func (k *fakeKernel) largestRequest() int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.maxAsk
}

func newCgroup(t *testing.T, anon int64) string {
	t.Helper()
	root := t.TempDir()
	cg := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-podabc.slice", "cri-containerd-c0ffee.scope")
	if err := os.MkdirAll(cg, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"memory.stat":         fmt.Sprintf("anon %d\nfile 1048576\n", anon),
		"memory.current":      strconv.FormatInt(anon+mib, 10),
		"memory.swap.current": "0",
		"memory.swap.max":     "0",
		"memory.reclaim":      "",
		"cgroup.freeze":       "0",
		"cgroup.events":       "populated 1\nfrozen 0\n",
		"cgroup.procs":        "4242\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(cg, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cg
}

func runKernel(t *testing.T, cg string, keepBack int64) *fakeKernel {
	k := &fakeKernel{cg: cg, keepBack: keepBack, stop: make(chan struct{})}
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		for {
			select {
			case <-k.stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			freeze := strings.TrimSpace(read(t, cg, "cgroup.freeze"))
			_ = os.WriteFile(filepath.Join(cg, "cgroup.events"), []byte("populated 1\nfrozen "+freeze+"\n"), 0o644)
			if ask := strings.Fields(read(t, cg, "memory.reclaim")); len(ask) > 0 {
				_ = os.WriteFile(filepath.Join(cg, "memory.reclaim"), nil, 0o644)
				n, _ := strconv.ParseInt(ask[0], 10, 64)
				k.mu.Lock()
				k.maxAsk = max(k.maxAsk, n)
				k.mu.Unlock()
				anon := num(t, cg, "memory.stat", "anon")
				moved := min(n, anon-k.keepBack)
				if moved > 0 {
					swapped := num(t, cg, "memory.swap.current", "") + moved
					_ = os.WriteFile(filepath.Join(cg, "memory.stat"), []byte(fmt.Sprintf("anon %d\nfile 0\n", anon-moved)), 0o644)
					_ = os.WriteFile(filepath.Join(cg, "memory.swap.current"), []byte(strconv.FormatInt(swapped, 10)), 0o644)
					_ = os.WriteFile(filepath.Join(cg, "memory.current"), []byte(strconv.FormatInt(anon-moved, 10)), 0o644)
				}
			}
		}
	}()
	t.Cleanup(func() { close(k.stop); k.wg.Wait() })
	return k
}

func read(t *testing.T, cg, file string) string {
	raw, err := os.ReadFile(filepath.Join(cg, file))
	if err != nil {
		return ""
	}
	return string(raw)
}

// num reads a memory.stat key, or a single-value file when key is empty.
func num(t *testing.T, cg, file, key string) int64 {
	if key == "" {
		n, _ := readInt(cg, file)
		return n
	}
	n, _ := field(filepath.Join(cg, file), key, 1)
	return n
}

func meminfoFile(t *testing.T, swapFree, available int64) string {
	path := filepath.Join(t.TempDir(), "meminfo")
	body := fmt.Sprintf("MemTotal: 6000000 kB\nMemAvailable: %d kB\nSwapFree: %d kB\n", available/1024, swapFree/1024)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func host(t *testing.T, cg string, swapFree, available int64, prefetched *[]int) Host {
	return Host{
		CgroupRoot: filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(cg)))),
		Meminfo:    meminfoFile(t, swapFree, available),
		Prefetch: func(pid int) error {
			if prefetched != nil {
				*prefetched = append(*prefetched, pid)
			}
			return nil
		},
		Poll:    time.Millisecond,
		Timeout: 2 * time.Second,
	}
}

func refusal(t *testing.T, err error) Code {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	return e.Code
}

func TestFindLocatesTheContainerUnderEitherDriver(t *testing.T) {
	cg := newCgroup(t, 900*mib)
	h := host(t, cg, 0, 0, nil)
	if got, err := h.Find("c0ffee"); err != nil || got != cg {
		t.Fatalf("Find = %q, %v; want %q", got, err, cg)
	}
	if _, err := h.Find("missing"); refusal(t, err) != CodeNotFound {
		t.Fatalf("a missing container: %v", err)
	}
}

func TestFreezeRefusesWhenSwapCannotHoldTheMemoryAndTouchesNothing(t *testing.T) {
	cg := newCgroup(t, 900*mib)
	runKernel(t, cg, 0)
	_, err := host(t, cg, 900*mib, 4096*mib, nil).Freeze(cg)
	if refusal(t, err) != CodeInsufficientSwap {
		t.Fatalf("err = %v", err)
	}
	if read(t, cg, "cgroup.freeze") != "0" || read(t, cg, "memory.swap.max") != "0" {
		t.Fatalf("a refused freeze changed the cgroup: freeze=%q swap.max=%q", read(t, cg, "cgroup.freeze"), read(t, cg, "memory.swap.max"))
	}
}

func TestFreezeLeavesTheProcessFrozenWithItsMemoryInSwap(t *testing.T) {
	cg := newCgroup(t, 900*mib)
	k := runKernel(t, cg, 4*mib)
	got, err := host(t, cg, 2048*mib, 4096*mib, nil).Freeze(cg)
	if err != nil || !got.OK || got.SwappedBytes != 896*mib {
		t.Fatalf("Freeze = %+v, %v; want 896 MiB swapped", got, err)
	}
	// One memory.reclaim write runs until it reaches its target, so a freeze asks a chunk at a time.
	if ask := k.largestRequest(); ask > reclaimChunk {
		t.Fatalf("asked the kernel for %d MiB in one write, more than a %d MiB chunk", ask>>20, reclaimChunk>>20)
	}
	if read(t, cg, "cgroup.freeze") != "1" || read(t, cg, "memory.swap.max") != "max" {
		t.Fatalf("after a freeze: freeze=%q swap.max=%q", read(t, cg, "cgroup.freeze"), read(t, cg, "memory.swap.max"))
	}
}

func TestFreezeThatCannotEmptyMemoryResumesTheProcess(t *testing.T) {
	cg := newCgroup(t, 900*mib)
	runKernel(t, cg, 400*mib)
	_, err := host(t, cg, 2048*mib, 4096*mib, nil).Freeze(cg)
	if refusal(t, err) != CodeIncomplete {
		t.Fatalf("err = %v", err)
	}
	if read(t, cg, "cgroup.freeze") != "0" || read(t, cg, "memory.swap.max") != "0" {
		t.Fatalf("a half-done freeze left the process stopped: freeze=%q swap.max=%q", read(t, cg, "cgroup.freeze"), read(t, cg, "memory.swap.max"))
	}
}

func TestKillWaitsUntilTheContainerHasNoProcessLeft(t *testing.T) {
	cg := newCgroup(t, 900*mib)
	go func() {
		for strings.TrimSpace(read(t, cg, "cgroup.kill")) != "1" {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(cg, "cgroup.events"), []byte("populated 0\nfrozen 1\n"), 0o644)
	}()
	start := time.Now()
	got, err := host(t, cg, 0, 0, nil).Kill(cg)
	if err != nil || !got.OK || time.Since(start) < 20*time.Millisecond {
		t.Fatalf("Kill = %+v, %v after %s; want OK once the cgroup is empty", got, err, time.Since(start))
	}
}

func TestThawRefusesWithoutRoomAndKeepsTheProcessFrozen(t *testing.T) {
	cg := newCgroup(t, 900*mib)
	runKernel(t, cg, 4*mib)
	h := host(t, cg, 2048*mib, 4096*mib, nil)
	if _, err := h.Freeze(cg); err != nil {
		t.Fatal(err)
	}
	h.Meminfo = meminfoFile(t, 1024*mib, 512*mib)
	if _, err := h.Thaw(cg, 896*mib); refusal(t, err) != CodeInsufficientMemory {
		t.Fatalf("err = %v", err)
	}
	if read(t, cg, "cgroup.freeze") != "1" {
		t.Fatal("a refused thaw resumed the process")
	}
}

func TestThawReadsMemoryBackBeforeResuming(t *testing.T) {
	cg := newCgroup(t, 900*mib)
	runKernel(t, cg, 4*mib)
	var prefetched []int
	h := host(t, cg, 2048*mib, 4096*mib, &prefetched)
	if _, err := h.Freeze(cg); err != nil {
		t.Fatal(err)
	}
	got, err := h.Thaw(cg, 896*mib)
	if err != nil || !got.OK {
		t.Fatalf("Thaw = %+v, %v", got, err)
	}
	if len(prefetched) != 1 || prefetched[0] != 4242 {
		t.Fatalf("prefetched %v, want the cgroup's process 4242", prefetched)
	}
	if read(t, cg, "cgroup.freeze") != "0" || read(t, cg, "memory.swap.max") != "0" {
		t.Fatalf("after a thaw: freeze=%q swap.max=%q", read(t, cg, "cgroup.freeze"), read(t, cg, "memory.swap.max"))
	}
}
