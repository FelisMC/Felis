//go:build linux

package watchdog

import "syscall"

// ClockStatus reads the kernel's clock status word. Modes stays zero, so the
// call only reads and needs no privilege.
func ClockStatus() (int32, bool) {
	var t syscall.Timex
	if _, err := syscall.Adjtimex(&t); err != nil {
		return 0, false
	}
	return t.Status, true
}
