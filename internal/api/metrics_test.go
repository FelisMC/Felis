package api

import (
	"net/http"
	"strings"
	"testing"

	"felis.lolicon.best/internal/metrics"
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
