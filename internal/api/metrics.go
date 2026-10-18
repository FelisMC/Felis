package api

import (
	"database/sql"
	"net/http"

	"felis.lolicon.best/internal/metrics"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// apiMetricsHandler exposes API-process counters; operator metrics are served separately.
var (
	apiRegistry       = prometheus.NewRegistry()
	apiMetricsHandler = newAPIMetricsHandler(apiRegistry)
)

func newAPIMetricsHandler(reg *prometheus.Registry) http.Handler {
	_ = metrics.Register(reg)
	reg.MustRegister(httpRequestsTotal, httpRequestDuration,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

func RegisterStorePool(db *sql.DB) error {
	return apiRegistry.Register(collectors.NewDBStatsCollector(db, "felis"))
}

// handleMetrics is internal-only and tokenless for Prometheus scrapes.
func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	apiMetricsHandler.ServeHTTP(w, r)
}
