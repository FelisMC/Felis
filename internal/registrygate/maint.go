package registrygate

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry GC needs a write-free lease: writes return 503 while reads continue, and lease expiry recovers from sidecar failure.

// DefaultQuiet requires idle time between the last write and a GC lease to avoid interrupting multi-request pushes.
const DefaultQuiet = 2 * time.Minute

const maxLease = time.Hour

type maintenance struct {
	mu        sync.Mutex
	inflight  int
	lastWrite time.Time
	until     time.Time
	quiet     time.Duration
	stateFile string
	now       func() time.Time
}

func (m *maintenance) beginWrite() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if now.Before(m.until) {
		return false
	}
	m.inflight++
	m.lastWrite = now
	return true
}

func (m *maintenance) endWrite() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inflight--
	m.lastWrite = m.now()
}

// acquire refuses while writes are active or the quiet period has not elapsed.
func (m *maintenance) acquire(lease time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if !now.Before(m.until) {
		if m.inflight > 0 {
			return fmt.Errorf("%d write(s) in flight", m.inflight)
		}
		if idle := now.Sub(m.lastWrite); idle < m.quiet {
			return fmt.Errorf("last write %s ago, waiting for %s of quiet", idle.Round(time.Second), m.quiet)
		}
	}
	m.until = now.Add(lease)
	m.persist()
	return nil
}

func (m *maintenance) release() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.until = time.Time{}
	m.persist()
}

func (m *maintenance) readOnly() (bool, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now().Before(m.until), m.until
}

// Persist the lease across gate restarts; caller holds mu.
func (m *maintenance) persist() {
	if m.stateFile == "" {
		return
	}
	if m.until.IsZero() {
		_ = os.Remove(m.stateFile)
		return
	}
	tmp := m.stateFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(m.until.Unix(), 10)), 0o600); err == nil {
		_ = os.Rename(tmp, m.stateFile)
	}
}

// SetMaintenanceState restores and persists the read-only lease on a durable volume.
func (g *Gate) SetMaintenanceState(path string) error {
	g.maint.mu.Lock()
	defer g.maint.mu.Unlock()
	g.maint.stateFile = path
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return fmt.Errorf("maintenance state %s: %w", path, err)
	}
	if until := time.Unix(sec, 0); g.maint.now().Before(until) {
		g.maint.until = until
	}
	return nil
}

func (g *Gate) SetQuiet(d time.Duration) {
	g.maint.mu.Lock()
	g.maint.quiet = d
	g.maint.mu.Unlock()
}

// MaintHandler is unauthenticated and must listen only on Pod loopback.
func (g *Gate) MaintHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /readonly", func(w http.ResponseWriter, r *http.Request) {
		lease := maxLease
		if s := r.URL.Query().Get("lease"); s != "" {
			sec, err := strconv.Atoi(s)
			if err != nil || sec <= 0 {
				http.Error(w, "lease must be a positive number of seconds", http.StatusBadRequest)
				return
			}
			lease = min(time.Duration(sec)*time.Second, maxLease)
		}
		if err := g.maint.acquire(lease); err != nil {
			http.Error(w, "busy: "+err.Error(), http.StatusConflict)
			return
		}
		_, until := g.maint.readOnly()
		if g.Log != nil {
			g.Log.Info("registry read-only for garbage collection", "until", until.UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(w, "read-only until %s\n", until.UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("POST /readwrite", func(w http.ResponseWriter, r *http.Request) {
		g.maint.release()
		if g.Log != nil {
			g.Log.Info("registry writable again")
		}
		fmt.Fprintln(w, "writable")
	})
	mux.HandleFunc("GET /readonly", func(w http.ResponseWriter, r *http.Request) {
		if ro, until := g.maint.readOnly(); ro {
			fmt.Fprintf(w, "read-only until %s\n", until.UTC().Format(time.RFC3339))
			return
		}
		http.Error(w, "writable", http.StatusConflict)
	})
	return mux
}

func MaintStatePath(dir string) string { return filepath.Join(dir, "readonly-until") }
