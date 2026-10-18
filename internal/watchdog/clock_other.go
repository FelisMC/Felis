//go:build !linux

package watchdog

// ClockStatus skips the check outside Linux.
func ClockStatus() (int32, bool) {
	return 0, false
}
