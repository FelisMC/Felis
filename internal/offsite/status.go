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
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	Endpoint    string    `json:"endpoint"`
	Bucket      string    `json:"bucket"`
	Prefix      string    `json:"prefix,omitempty"`
	KeyID       string    `json:"key_id"`
	Result      Result    `json:"result"`
	// KeyMismatch is a run refused because the bucket's objects are sealed
	// with another key (ErrKeyMismatch): no later run copies anything until
	// the key is fixed, so the watchdog reports it at once.
	KeyMismatch bool `json:"key_mismatch,omitempty"`
	// Standby and Displaced are a run refused because another host, Writer,
	// writes the bucket (ErrStandby, ErrDisplaced).
	Standby   bool    `json:"standby,omitempty"`
	Displaced bool    `json:"displaced,omitempty"`
	Writer    *Writer `json:"writer,omitempty"`
	// Format is StatusFormat in every record this release writes; 0 is a
	// record from before writers were recorded, whose host had been copying
	// to the bucket (Lease.Inherited).
	Format int `json:"format,omitempty"`
	// Inherited carries Lease.Inherited over runs that ended before the host
	// recorded itself (an unreachable bucket on the first run after the
	// upgrade), until it has an id.
	Inherited bool `json:"inherited,omitempty"`
}

// StatusFormat marks a status record that knows about felis-writer.
const StatusFormat = 2

// StandsBy is the host this one stands by for: the last run was refused
// because that host writes the bucket, and it wrote it within WriterLive of
// now. While it keeps writing, this host is a rehearsal (or a rebuild not yet
// taken over), and the owners in its restored database are that host's: the
// watchdog here mails them nothing.
func (st *Status) StandsBy(now time.Time) *Writer {
	if st == nil || !st.Standby || st.Writer == nil || now.Sub(st.Writer.At) > WriterLive {
		return nil
	}
	return st.Writer
}

// HostLease is the Lease of the host whose status file is statusFile: its id
// next to it, and Inherited when that file was written by an older release
// (or carries Inherited from one).
func HostLease(statusFile string) Lease {
	host, _ := os.Hostname()
	l := Lease{IDFile: filepath.Join(filepath.Dir(statusFile), HostIDFile), Host: host}
	if prev, err := ReadStatus(statusFile); err == nil && prev != nil && (prev.Format == 0 || prev.Inherited) {
		l.Inherited = true
	}
	return l
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
