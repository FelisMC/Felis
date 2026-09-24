package offsite

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// DefaultStatusFile is where `felis offsite sync` records its last run; the
// watchdog and `felis offsite status` read it.
const DefaultStatusFile = "/var/lib/felis/offsite/status.json"

// StaleAfter is how long the sync may go without a clean run before the
// watchdog mails the owners. The timer runs hourly, so this rides out a
// provider's bad morning, and still leaves a day's database bundle uncopied
// for at most half a day.
const StaleAfter = 12 * time.Hour

// Status is the record of the last run.
type Status struct {
	LastAttempt time.Time `json:"last_attempt"`
	// LastSuccess is the last run in which every step succeeded.
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Endpoint    string    `json:"endpoint"`
	Bucket      string    `json:"bucket"`
	Prefix      string    `json:"prefix,omitempty"`
	KeyID       string    `json:"key_id"`
	Result      Result    `json:"result"`
}

// ReadStatus reads the status file. A missing file is (nil, nil): no sync has
// run yet.
func ReadStatus(p string) (*Status, error) {
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// WriteStatus replaces the status file atomically.
func WriteStatus(p string, st Status) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
