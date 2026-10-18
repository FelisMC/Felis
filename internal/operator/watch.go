package operator

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// reconcileTimeout is longer than normal probe and save waits; expiry cancels I/O and retries with backoff.
const reconcileTimeout = 3 * time.Minute

// stuckReconcileAfter triggers liveness failure for passes that ignore cancellation, allowing restart to free wedged workers.
const stuckReconcileAfter = 10 * time.Minute

// ReconcileWatch measures in-flight duration; idle operators remain healthy regardless of time since their last pass.
type ReconcileWatch struct {
	StuckAfter time.Duration
	Now        func() time.Time

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
