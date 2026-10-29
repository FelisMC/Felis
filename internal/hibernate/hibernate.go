// Package hibernate freezes a container's processes and moves their memory to swap, and brings them back, through cgroup v2.
package hibernate

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	mib = 1 << 20
	// swapMargin is room left in swap beyond the anonymous memory being moved.
	swapMargin = 64 * mib
	// residentAllowance is anonymous memory a freeze may leave behind and still count as done.
	residentAllowance = 32 * mib
	// reclaimChunk bounds one memory.reclaim write, which the kernel retries for as long as it makes any progress.
	reclaimChunk = 256 * mib
)

// Code names a refusal the operator acts on.
type Code string

const (
	CodeInsufficientSwap   Code = "insufficient_swap"
	CodeIncomplete         Code = "incomplete"
	CodeInsufficientMemory Code = "insufficient_memory"
	CodeNotFound           Code = "not_found"
	CodeFailed             Code = "failed"
)

// Error is a refusal that leaves the container running unfrozen.
type Error struct {
	Code Code
	Msg  string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Msg }

// Result is what a freeze or thaw reports back to the operator.
type Result struct {
	OK            bool   `json:"ok"`
	Code          Code   `json:"code,omitempty"`
	Message       string `json:"message,omitempty"`
	SwappedBytes  int64  `json:"swappedBytes,omitempty"`
	ResidentBytes int64  `json:"residentBytes,omitempty"`
}

// Host is the node as the freeze sees it; tests replace the paths and the prefetch.
type Host struct {
	CgroupRoot string
	Meminfo    string
	// Prefetch asks the kernel to read a process's swapped memory back in.
	Prefetch func(pid int) error
	Poll     time.Duration
	Timeout  time.Duration
}

// Find returns the cgroup directory of a container under either cgroup driver's naming.
func (h Host) Find(containerID string) (string, error) {
	want := map[string]bool{containerID: true, "cri-containerd-" + containerID + ".scope": true}
	var found string
	err := filepath.WalkDir(h.CgroupRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return fs.SkipDir
		}
		if d.IsDir() && want[d.Name()] {
			found = path
			return fs.SkipAll
		}
		if d.IsDir() && strings.Count(strings.TrimPrefix(path, h.CgroupRoot), "/") > 5 {
			return fs.SkipDir
		}
		return nil
	})
	if found == "" {
		if err == nil {
			err = errors.New("no cgroup for container " + containerID)
		}
		return "", &Error{CodeNotFound, err.Error()}
	}
	return found, nil
}

// Freeze stops the container's processes and swaps out their anonymous memory, or thaws them again and refuses.
func (h Host) Freeze(cg string) (Result, error) {
	anon, err := statValue(cg, "anon")
	if err != nil {
		return Result{}, err
	}
	free, err := meminfo(h.Meminfo, "SwapFree")
	if err != nil {
		return Result{}, err
	}
	if need := anon + swapMargin; free < need {
		return Result{}, &Error{CodeInsufficientSwap, fmt.Sprintf("needs %d MiB of swap, %d MiB free", need/mib, free/mib)}
	}
	if err := write(cg, "memory.swap.max", "max"); err != nil {
		return Result{}, err
	}
	if err := h.setFrozen(cg, true); err != nil {
		_ = h.release(cg)
		return Result{}, err
	}
	allowed := max(residentAllowance, anon/50)
	deadline := time.Now().Add(h.timeout())
	left, progressed := anon, time.Now()
	for time.Since(progressed) < 50*h.poll() && time.Now().Before(deadline) {
		reclaimAnon(cg, min(left, reclaimChunk))
		time.Sleep(h.poll())
		now, err := statValue(cg, "anon")
		if err != nil {
			break
		}
		if now <= allowed {
			swapped, _ := readInt(cg, "memory.swap.current")
			resident, _ := readInt(cg, "memory.current")
			return Result{OK: true, SwappedBytes: swapped, ResidentBytes: resident}, nil
		}
		if now < left {
			left, progressed = now, time.Now()
		}
	}
	left, _ = statValue(cg, "anon")
	if err := h.release(cg); err != nil {
		return Result{}, err
	}
	return Result{}, &Error{CodeIncomplete, fmt.Sprintf("%d MiB stayed in memory", left/mib)}
}

// Thaw reads the swapped memory back in while the processes are still frozen, then resumes them.
func (h Host) Thaw(cg string, expect int64) (Result, error) {
	swapped, err := readInt(cg, "memory.swap.current")
	if err != nil {
		return Result{}, err
	}
	available, err := meminfo(h.Meminfo, "MemAvailable")
	if err != nil {
		return Result{}, err
	}
	if available < swapped {
		return Result{}, &Error{CodeInsufficientMemory, fmt.Sprintf("needs %d MiB of memory, %d MiB available", swapped/mib, available/mib)}
	}
	pids, err := procs(cg)
	if err != nil {
		return Result{}, err
	}
	for _, pid := range pids {
		if err := h.Prefetch(pid); err != nil {
			return Result{}, &Error{CodeFailed, fmt.Sprintf("read memory of process %d back: %v", pid, err)}
		}
	}
	h.waitResident(cg, expect)
	if err := h.release(cg); err != nil {
		return Result{}, err
	}
	resident, _ := readInt(cg, "memory.current")
	return Result{OK: true, ResidentBytes: resident}, nil
}

// Kill ends every process in the container at once, frozen or not, and waits until none is left.
func (h Host) Kill(cg string) (Result, error) {
	if err := write(cg, "cgroup.kill", "1"); err != nil {
		return Result{}, err
	}
	deadline := time.Now().Add(h.timeout())
	for {
		events, err := os.ReadFile(filepath.Join(cg, "cgroup.events"))
		if errors.Is(err, fs.ErrNotExist) || (err == nil && strings.Contains(string(events), "populated 0")) {
			return Result{OK: true}, nil
		}
		if err != nil {
			return Result{}, &Error{CodeFailed, err.Error()}
		}
		if time.Now().After(deadline) {
			return Result{}, &Error{CodeFailed, "processes outlived cgroup.kill"}
		}
		time.Sleep(h.poll())
	}
}

// waitResident gives swap-in time to finish until the memory is back or stops growing; pages read back sit in the swap cache, which memory.current counts and anon does not.
func (h Host) waitResident(cg string, expect int64) {
	deadline := time.Now().Add(h.timeout())
	last, still := int64(-1), 0
	for time.Now().Before(deadline) {
		resident, err := readInt(cg, "memory.current")
		if err != nil || resident >= expect-expect/20 {
			return
		}
		if resident == last {
			if still++; still >= 10 {
				return
			}
		} else {
			last, still = resident, 0
		}
		time.Sleep(h.poll())
	}
}

// release resumes the processes and stops further swapping out.
func (h Host) release(cg string) error {
	if err := write(cg, "memory.swap.max", "0"); err != nil {
		return err
	}
	return h.setFrozen(cg, false)
}

func (h Host) setFrozen(cg string, frozen bool) error {
	value, want := "0", "frozen 0"
	if frozen {
		value, want = "1", "frozen 1"
	}
	if err := write(cg, "cgroup.freeze", value); err != nil {
		return err
	}
	deadline := time.Now().Add(h.timeout())
	for {
		events, err := os.ReadFile(filepath.Join(cg, "cgroup.events"))
		if err != nil {
			return &Error{CodeFailed, err.Error()}
		}
		if strings.Contains(string(events), want) {
			return nil
		}
		if time.Now().After(deadline) {
			return &Error{CodeFailed, "cgroup did not reach " + want}
		}
		time.Sleep(h.poll())
	}
}

func (h Host) poll() time.Duration {
	if h.Poll > 0 {
		return h.Poll
	}
	return 20 * time.Millisecond
}

func (h Host) timeout() time.Duration {
	if h.Timeout > 0 {
		return h.Timeout
	}
	return 120 * time.Second
}

// reclaimAnon asks the kernel to swap out n bytes of anonymous memory; kernels before 6.9 refuse the swappiness argument and reclaim from both lists.
func reclaimAnon(cg string, n int64) {
	path := filepath.Join(cg, "memory.reclaim")
	err := os.WriteFile(path, []byte(strconv.FormatInt(n, 10)+" swappiness=200"), 0)
	if errors.Is(err, syscall.EINVAL) {
		_ = os.WriteFile(path, []byte(strconv.FormatInt(n, 10)), 0)
	}
}

func write(cg, file, value string) error {
	if err := os.WriteFile(filepath.Join(cg, file), []byte(value), 0); err != nil {
		return &Error{CodeFailed, fmt.Sprintf("write %s: %v", file, err)}
	}
	return nil
}

func readInt(cg, file string) (int64, error) {
	raw, err := os.ReadFile(filepath.Join(cg, file))
	if err != nil {
		return 0, &Error{CodeFailed, err.Error()}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, &Error{CodeFailed, fmt.Sprintf("parse %s: %v", file, err)}
	}
	return n, nil
}

func statValue(cg, key string) (int64, error) {
	return field(filepath.Join(cg, "memory.stat"), key, 1)
}

// meminfo reads a /proc/meminfo line, which counts in KiB.
func meminfo(path, key string) (int64, error) {
	return field(path, key+":", 1024)
}

func field(path, key string, unit int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, &Error{CodeFailed, err.Error()}
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) >= 2 && parts[0] == key {
			n, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return 0, &Error{CodeFailed, fmt.Sprintf("parse %s in %s: %v", key, path, err)}
			}
			return n * unit, nil
		}
	}
	return 0, &Error{CodeFailed, fmt.Sprintf("%s missing from %s", key, path)}
}

func procs(cg string) ([]int, error) {
	raw, err := os.ReadFile(filepath.Join(cg, "cgroup.procs"))
	if err != nil {
		return nil, &Error{CodeFailed, err.Error()}
	}
	var pids []int
	for _, line := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(line); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
