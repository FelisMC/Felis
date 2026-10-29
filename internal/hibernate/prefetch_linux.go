//go:build linux

package hibernate

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// iovec addresses memory in the other process, so it carries plain integers rather than Go pointers.
type iovec struct {
	base uintptr
	len  uintptr
}

// PrefetchSwap asks the kernel to read every writable private mapping of a process back from swap.
func PrefetchSwap(pid int) error {
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("pidfd_open: %w", err)
	}
	defer unix.Close(pidfd)
	ranges, err := privateRanges(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return err
	}
	for _, r := range ranges {
		v := []iovec{r}
		_, _, errno := unix.Syscall6(unix.SYS_PROCESS_MADVISE, uintptr(pidfd), uintptr(unsafe.Pointer(&v[0])), 1, unix.MADV_WILLNEED, 0, 0)
		runtime.KeepAlive(v)
		// A mapping that cannot take the advice, such as a guard page, is skipped; the rest still come back.
		if errno != 0 && !errors.Is(errno, unix.EINVAL) && !errors.Is(errno, unix.ENOMEM) {
			return fmt.Errorf("process_madvise: %w", errno)
		}
	}
	return nil
}

func privateRanges(path string) ([]iovec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []iovec
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) < 2 || len(parts[1]) < 4 || parts[1][1] != 'w' || parts[1][3] != 'p' {
			continue
		}
		lo, hi, ok := strings.Cut(parts[0], "-")
		if !ok {
			continue
		}
		start, err1 := strconv.ParseUint(lo, 16, 64)
		end, err2 := strconv.ParseUint(hi, 16, 64)
		if err1 != nil || err2 != nil || end <= start {
			continue
		}
		out = append(out, iovec{base: uintptr(start), len: uintptr(end - start)})
	}
	return out, scanner.Err()
}
