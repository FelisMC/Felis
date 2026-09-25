package api

import (
	"database/sql"
	"net/http"

	"felis.lolicon.best/internal/metrics"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// apiMetricsHandler serves the felis_* collectors from a process-local registry.
// The API process is the only producer of felis_image_build_failures_total (the
// build reconcile loop lives in cmd/felis.reconcileBuilds), so this endpoint is
// that counter's sole scrape path; the operator's :8080 carries the fleet
// gauges instead.
var (
	apiRegistry       = prometheus.NewRegistry()
	apiMetricsHandler = newAPIMetricsHandler(apiRegistry)
)

func newAPIMetricsHandler(reg *prometheus.Registry) http.Handler {
	// A fresh registry cannot already hold a collector, so Register's
	// AlreadyRegistered tolerance arm never triggers here.
	_ = metrics.Register(reg)
	// The request series (observe.go) and the runtime: goroutines, heap, GC and
	// the process's fds and RSS, which is where a leaked stream or a slow
	// connection pile-up shows first.
	reg.MustRegister(httpRequestsTotal, httpRequestDuration,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// RegisterStorePool adds the store pool's go_sql_* series (db_name="felis"):
// connections open, in use and idle against the cap, and how often and how
// long requests waited for one, which is where a pool pinned at its cap shows.
func RegisterStorePool(db *sql.DB) error {
	return apiRegistry.Register(collectors.NewDBStatsCollector(db, "felis"))
}

// handleMetrics is mounted as a Public route on the internal face (the same
// stance as the health probes): the internal listener is ClusterIP-only and a
// Prometheus scrape carries no token. The external face never serves metrics.
// See docs/troubleshooting.md §14 for what to scrape and the alert rules that
// consume these series.
func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	apiMetricsHandler.ServeHTTP(w, r)
}
