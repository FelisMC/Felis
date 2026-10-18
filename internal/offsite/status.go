package offsite

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const DefaultStatusFile = "/var/lib/felis/offsite/status.json"

// StaleAfter tolerates missed hourly runs before notifying owners.
const StaleAfter = 12 * time.Hour

type Status struct {
	LastAttempt time.Time `json:"last_attempt"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	Endpoint    string    `json:"endpoint"`
	Bucket      string    `json:"bucket"`
	Prefix      string    `json:"prefix,omitempty"`
	KeyID       string    `json:"key_id"`
	Result      Result    `json:"result"`
	// KeyMismatch requires immediate notification because sync cannot resume until the bucket key matches.
	KeyMismatch bool    `json:"key_mismatch,omitempty"`
	Standby     bool    `json:"standby,omitempty"`
	Displaced   bool    `json:"displaced,omitempty"`
	Writer      *Writer `json:"writer,omitempty"`
	// Format=0 identifies legacy records from hosts that copied before writer tracking.
	Format int `json:"format,omitempty"`
	// Inherited persists legacy writer ownership until the host can record a stable ID.
	Inherited bool `json:"inherited,omitempty"`
}

const StatusFormat = 2

// StandsBy suppresses local owner notices while another recently active host is the bucket writer.
func (st *Status) StandsBy(now time.Time) *Writer {
	if st == nil || !st.Standby || st.Writer == nil || now.Sub(st.Writer.At) > WriterLive {
		return nil
	}
	return st.Writer
}

func HostLease(statusFile string) Lease {
	host, _ := os.Hostname()
	l := Lease{IDFile: filepath.Join(filepath.Dir(statusFile), HostIDFile), Host: host}
	if prev, err := ReadStatus(statusFile); err == nil && prev != nil && (prev.Format == 0 || prev.Inherited) {
		l.Inherited = true
	}
	return l
}

// ReadStatus returns (nil,nil) before the first recorded sync.
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
