package updates

import "time"

// StatusKey is the platform_settings key `felis update --record` writes the
// newest version check to (felis-update-check.timer runs it daily on the host,
// where the versions are readable); internal/api serves it to the panel. Felis
// applies nothing on its own, so this record is how a SysAdmin learns an update
// exists without opening a shell on the node.
const StatusKey = "update_report"

// StatusStaleAfter is how old the newest check may get before the panel says the
// daily timer stopped: a day plus the timer's randomized delay and a margin.
const StatusStaleAfter = 26 * time.Hour

// Component states in a StatusReport.
const (
	StateCurrent    = "current"    // the newest stable release is installed
	StateAvailable  = "available"  // a newer stable release exists
	StateUnknown    = "unknown"    // the release feed could not be read
	StateUnreadable = "unreadable" // the installed version could not be read
	StatePinned     = "pinned"     // never proposed a change by policy
)

// StatusReport is the value stored under StatusKey.
type StatusReport struct {
	CheckedAt time.Time `json:"checked_at"`
	// Felis is the version of the felis binary that ran the check.
	Felis      string            `json:"felis"`
	Components []ComponentStatus `json:"components"`
}

// ComponentStatus is one component's line of a StatusReport. Latest is empty
// unless State is StateAvailable; Error names why a version is missing (State
// StateUnknown or StateUnreadable); Selector is the `felis update --<selector>`
// flag that prints how to apply it, empty for a component with none.
type ComponentStatus struct {
	Name     string `json:"name"`
	Current  string `json:"current,omitempty"`
	Latest   string `json:"latest,omitempty"`
	State    string `json:"state"`
	Selector string `json:"selector,omitempty"`
	Note     string `json:"note,omitempty"`
	Error    string `json:"error,omitempty"`
}
