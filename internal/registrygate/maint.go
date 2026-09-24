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

// registry garbage-collect is only safe on a registry nobody writes to: it marks
// the blobs every manifest references, then deletes the rest, and a layer pushed
// between the two phases is deleted under the manifest that arrives next. The GC
// sidecar therefore asks the gate for a read-only window over a loopback-only
// maintenance listener (MaintHandler), and the gate grants it only once writes
// have been quiet for a while, so a push that is between two of its requests is
// not cut in half.
//
// While the window is open every write answers 503 with Retry-After; reads keep
// working, so running servers and kubelet re-pulls never notice. The window is a
// lease: a GC sidecar that dies mid-sweep cannot leave the registry read-only for
// longer than the lease it asked for.

// DefaultQuiet is how long writes must have been idle before a read-only window is
// granted. A push issues its requests back to back; two minutes of silence means
// no push is mid-way.
const DefaultQuiet = 2 * time.Minute

// maxLease bounds a read-only window. A sweep over a few GiB takes seconds; an
// hour covers a large registry on a slow disk.
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

// beginWrite admits a write unless a read-only window is open.
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

// acquire opens (or extends) a read-only window for lease. It refuses while a
// write is in flight or the last one finished less than quiet ago.
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

// persist records the window's end in stateFile, so a gate container restarted
// mid-sweep comes back read-only instead of admitting writes into a running GC.
// Called with mu held.
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

// SetMaintenanceState makes the gate keep its read-only window in path (on a
// volume that outlives the container) and resumes a window a previous run of the
// gate left open.
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

// SetQuiet overrides DefaultQuiet (tests and drills shorten it).
func (g *Gate) SetQuiet(d time.Duration) {
	g.maint.mu.Lock()
	g.maint.quiet = d
	g.maint.mu.Unlock()
}

// MaintHandler serves the GC sidecar's side of the handshake. It carries no
// authentication, so it must only ever listen on the pod's loopback:
//
//	POST /readonly?lease=<seconds>  200 once the window is open, 409 while writes are not quiet
//	POST /readwrite                 200, the window is closed
//	GET  /readonly                  200 while read-only, 409 otherwise
//
// POST-only verbs keep the sidecar's busybox wget (which cannot send DELETE) able
// to drive it.
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

// MaintStatePath is where cmd/felis keeps the window inside the maintenance
// volume.
func MaintStatePath(dir string) string { return filepath.Join(dir, "readonly-until") }
