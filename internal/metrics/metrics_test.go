package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// wantNames is the metric set dashboards and alerts depend on; a rename must be deliberate.
var wantNames = []string{
	"felis_servers_total",
	"felis_server_phase",
	"felis_start_duration_seconds",
	"felis_image_build_failures_total",
	"felis_reaper_worlds_deleted_total",
	"felis_build_info",
}

func TestRegisterExposesNamedFelisMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Record through each typed collector so Gather proves these are the registered vars.
	ServersTotal.WithLabelValues("Running").Set(3)
	ServerPhase.WithLabelValues("survival", "", "Running", "Running").Set(1)
	SetBuildInfo("operator", "v1.2.3")
	StartDurationSeconds.Observe(12.5)
	ImageBuildFailuresTotal.Inc()
	ReaperWorldsDeletedTotal.Add(2)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]bool{}
	for _, mf := range mfs {
		got[mf.GetName()] = true
	}
	for _, name := range wantNames {
		if !got[name] {
			t.Errorf("mandated metric %q not exposed by the registry", name)
		}
	}
	// Nothing may leak out from under the felis_ namespace — a stray name would
	// mean a collector was declared without the namespace prefix.
	for name := range got {
		if !strings.HasPrefix(name, "felis_") {
			t.Errorf("metric %q is not under the felis_ namespace", name)
		}
	}
}

func TestSyncServerPhasesDropsDeletedServers(t *testing.T) {
	SyncServerPhases([]ServerPhaseSample{
		{Server: "login", Role: "login", Phase: "Starting", Desired: "Running"},
		{Server: "survival", Phase: "Running", Desired: "Running"},
	})
	SyncServerPhases([]ServerPhaseSample{
		{Server: "login", Role: "login", Phase: "Running", Desired: "Running"},
	})
	if n := testutil.CollectAndCount(ServerPhase); n != 1 {
		t.Fatalf("series after the second sync = %d, want 1", n)
	}
	if got := testutil.ToFloat64(ServerPhase.WithLabelValues("login", "login", "Running", "Running")); got != 1 {
		t.Errorf("login Running = %v, want 1", got)
	}
	ServerPhase.Reset()
}

func TestSyncServerGaugeResetsStaleStates(t *testing.T) {
	// Self-contained: a state missing from the next snapshot must drain to 0.
	SyncServerGauge([]string{"Running", "Running", "Stopped"})
	if got := testutil.ToFloat64(ServersTotal.WithLabelValues("Running")); got != 2 {
		t.Errorf("Running = %v, want 2", got)
	}
	if got := testutil.ToFloat64(ServersTotal.WithLabelValues("Stopped")); got != 1 {
		t.Errorf("Stopped = %v, want 1", got)
	}

	// Next snapshot: the two Running servers are gone. Without the Reset the gauge
	// would still report Running=2; with it, the child drains to 0.
	SyncServerGauge([]string{"Stopped"})
	if got := testutil.ToFloat64(ServersTotal.WithLabelValues("Running")); got != 0 {
		t.Errorf("Running after drain = %v, want 0", got)
	}
	if got := testutil.ToFloat64(ServersTotal.WithLabelValues("Stopped")); got != 1 {
		t.Errorf("Stopped = %v, want 1", got)
	}
}

func TestRegisterIsIdempotent(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// A second Register into the same registry must not error: setup that runs
	// twice (manager restart, test re-entry) should be harmless, not fatal.
	if err := Register(reg); err != nil {
		t.Fatalf("second Register should be idempotent, got: %v", err)
	}
}
