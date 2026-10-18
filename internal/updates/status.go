package updates

import "time"

const StatusKey = "update_report"

// StatusStaleAfter permits the daily check interval plus timer jitter and margin.
const StatusStaleAfter = 26 * time.Hour

const (
	StateCurrent    = "current"
	StateAvailable  = "available"
	StateUnknown    = "unknown"
	StateUnreadable = "unreadable"
	StatePinned     = "pinned"
)

type StatusReport struct {
	CheckedAt  time.Time         `json:"checked_at"`
	Felis      string            `json:"felis"`
	Components []ComponentStatus `json:"components"`
}

// ComponentStatus.Latest is set only for available updates; Error identifies unknown or unreadable versions and Selector names an apply-help flag.
type ComponentStatus struct {
	Name     string `json:"name"`
	Current  string `json:"current,omitempty"`
	Latest   string `json:"latest,omitempty"`
	State    string `json:"state"`
	Selector string `json:"selector,omitempty"`
	Note     string `json:"note,omitempty"`
	Error    string `json:"error,omitempty"`
}
