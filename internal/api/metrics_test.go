package api

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"felis.lolicon.best/internal/metrics"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMetricsEndpoint locks the scrape surface: the internal face serves the
// felis_* collectors (the API process is felis_image_build_failures_total's
// only producer), and the external face never serves metrics.
func TestMetricsEndpoint(t *testing.T) {
	// Give every collector a sample so the exposition carries all four families:
	// an empty vector family is omitted from the text output entirely. The bumps
	// stay inside this test binary — the collectors are per-process globals.
	metrics.SyncServerGauge([]string{"Running"})
	metrics.StartDurationSeconds.Observe(3)
	metrics.ImageBuildFailuresTotal.Inc()
	metrics.ReaperWorldsDeletedTotal.Inc()

	a := &API{}

	w := do(a.InternalHandler(), "GET", "/metrics", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("internal GET /metrics = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`felis_servers_total{state="Running"} 1`,
		"felis_start_duration_seconds_count 1",
		"felis_image_build_failures_total",
		"felis_reaper_worlds_deleted_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics exposition missing %q", want)
		}
	}

	if w := do(a.ExternalHandler(), "GET", "/metrics", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("external GET /metrics = %d, want 404 (metrics stay internal)", w.Code)
	}
}

// The store pool's counts reach the internal scrape once registered.
func TestMetricsEndpointCarriesTheStorePool(t *testing.T) {
	pool, err := sql.Open("pgx", "postgres://felis@127.0.0.1:1/felis")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(7)
	if err := RegisterStorePool(pool); err != nil {
		t.Fatal(err)
	}

	w := do((&API{}).InternalHandler(), "GET", "/metrics", "", nil)
	for _, want := range []string{
		`go_sql_max_open_connections{db_name="felis"} 7`,
		`go_sql_wait_count_total{db_name="felis"} 0`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("metrics exposition missing %q", want)
		}
	}
}
