package operator

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// reconcileTimeout bounds one reconcile pass. The slowest legitimate pass waits
// on RCON: 5s for a probe, defaultSaveTimeout for the world save ahead of a stop.
// A pass still running past this is waiting on something that will not answer;
// cancelling its context makes the calls that honour it return, and the server
// is retried with backoff.
const reconcileTimeout = 3 * time.Minute

// stuckReconcileAfter is how long one pass may run before the operator reports
// itself unhealthy. It is well past reconcileTimeout, so only a pass blocked in a
// call that ignores its context gets here. Each such pass holds one of the
// maxConcurrentReconciles workers for good; a few of them stall every server's
// start and stop while the Pod still looks healthy. Failing the liveness probe
// gets the operator restarted, the one fix for a goroutine that never returns.
const stuckReconcileAfter = 10 * time.Minute

// ReconcileWatch tracks the reconcile passes in flight so the liveness probe can
// tell a busy operator from a wedged one. An idle operator has nothing in flight
// and is healthy: a quiet fleet reconciles rarely, so the time since the last
// pass says nothing about whether the next one would run.
type ReconcileWatch struct {
	// StuckAfter defaults to stuckReconcileAfter.
	StuckAfter time.Duration
	// Now defaults to time.Now.
	Now func() time.Time

	mu       sync.Mutex
	next     uint64
	inflight map[uint64]inflightPass
}

type inflightPass struct {
	server  string
	started time.Time
}

func (w *ReconcileWatch) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// begin records a pass for server and returns the func that ends it.
func (w *ReconcileWatch) begin(server string) func() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inflight == nil {
		w.inflight = map[uint64]inflightPass{}
	}
	w.next++
	id := w.next
	w.inflight[id] = inflightPass{server: server, started: w.now()}
	return func() {
		w.mu.Lock()
		delete(w.inflight, id)
		w.mu.Unlock()
	}
}

// Check is a healthz.Checker: it fails while any pass has been running longer
// than StuckAfter, naming the server and how long.
func (w *ReconcileWatch) Check(_ *http.Request) error {
	limit := w.StuckAfter
	if limit <= 0 {
		limit = stuckReconcileAfter
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	var oldest *inflightPass
	for id := range w.inflight {
		p := w.inflight[id]
		if oldest == nil || p.started.Before(oldest.started) {
			oldest = &p
		}
	}
	if oldest != nil && now.Sub(oldest.started) > limit {
		return fmt.Errorf("reconcile of %s has been running for %s (limit %s)",
			oldest.server, now.Sub(oldest.started).Truncate(time.Second), limit)
	}
	return nil
}
