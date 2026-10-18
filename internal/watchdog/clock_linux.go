//go:build linux

package watchdog

import "syscall"

// ClockStatus leaves Modes zero to read adjtimex without requiring privilege.
func ClockStatus() (int32, bool) {
	var t syscall.Timex
	if _, err := syscall.Adjtimex(&t); err != nil {
		return 0, false
	}
	return t.Status, true
}
