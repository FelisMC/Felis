//go:build !linux

package watchdog

// ClockStatus has no adjtimex to read outside Linux; the clock check is skipped.
func ClockStatus() (int32, bool) {
	return 0, false
}
