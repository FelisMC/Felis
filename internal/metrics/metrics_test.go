package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// wantNames is the exact set of metric names spec §23 mandates "at minimum".
// Pinning them here makes a rename a deliberate, test-visible act rather than a
// silent dashboard/alert break.
var wantNames = []string{
	"felis_servers_total",
	"felis_start_duration_seconds",
	"felis_image_build_failures_total",
	"felis_reaper_worlds_deleted_total",
}

func TestRegisterExposesNamedFelisMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Record into each collector through its typed API so a Gather emits the
	// family — this proves the exported vars are the ones actually registered,
	// not shadow copies.
	ServersTotal.WithLabelValues("Running").Set(3)
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

func TestSyncServerGaugeResetsStaleStates(t *testing.T) {
	// Self-contained (Reset->sync->assert within this one test) so it never
	// clobbers another test's ServersTotal children. The correctness point is the
	// Reset inside SyncServerGauge: a state present in one snapshot but absent from
	// the next must drain to 0, not retain its stale last value forever.
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

func TestCountersRecordExpectedValues(t *testing.T) {
	// These collectors are package-level singletons. This test asserts on a
	// gauge child labelled "Stopped" (no other test touches it) and a >=1 bound
	// on the build-failure counter, so values stay stable regardless of test
	// ordering or accumulation across the binary.
	reg := prometheus.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ImageBuildFailuresTotal.Inc()
	if got := testutil.ToFloat64(ImageBuildFailuresTotal); got < 1 {
		t.Errorf("image build failures = %v, want >= 1", got)
	}

	g := ServersTotal.WithLabelValues("Stopped")
	g.Set(7)
	if got := testutil.ToFloat64(g); got != 7 {
		t.Errorf("servers_total{state=Stopped} = %v, want 7", got)
	}

	StartDurationSeconds.Observe(30)
	if n := testutil.CollectAndCount(StartDurationSeconds); n != 1 {
		t.Errorf("start_duration_seconds collected %d metrics, want 1 histogram", n)
	}
}
