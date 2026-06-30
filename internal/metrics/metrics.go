// Package metrics defines and registers the named felis_* Prometheus
// collectors mandated by spec §23.
//
// The collectors are package-level vars so any subsystem (operator, reaper,
// image builder, prober) can record into them without importing back into a
// metrics owner and risking an import cycle. Wiring them into a registry is a
// single Register call that takes a prometheus.Registerer — the same
// inject-the-interface, fake-at-the-edge pattern used elsewhere in Felis
// (ownerStore, PVCResolver): production passes controller-runtime's global
// Registry (served by the operator's :metrics endpoint), tests pass a fresh
// prometheus.NewRegistry() so assertions never collide with global state.
package metrics

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// namespace prefixes every collector, so the exposed names are exactly
// felis_<name> — matching the metric names spec §23 mandates.
const namespace = "felis"

var (
	// ServersTotal is the current number of MinecraftServers the operator knows
	// about, partitioned by desiredState. It is a gauge, not a monotonic counter:
	// the reconcile loop Set()s it to the live fleet size, so it can fall as
	// servers are deleted. (The _total suffix follows the name spec §23 fixed.)
	ServersTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "servers_total",
		Help:      "Current number of Minecraft servers known to the operator, by desired state.",
	}, []string{"state"})

	// StartDurationSeconds observes the wall-clock time from desiredState=Running
	// to a server reporting ready. Buckets are tuned for Minecraft cold starts
	// (seconds to a few minutes), not the default sub-second web-latency buckets.
	StartDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "start_duration_seconds",
		Help:      "Time from desiredState=Running to a server reporting ready, in seconds.",
		Buckets:   []float64{1, 2, 5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 600},
	})

	// ImageBuildFailuresTotal counts modpack/image build failures (spec §8 lane).
	ImageBuildFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "image_build_failures_total",
		Help:      "Total number of image build failures.",
	})

	// ReaperWorldsDeletedTotal counts world volumes deleted by the reaper.
	ReaperWorldsDeletedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reaper_worlds_deleted_total",
		Help:      "Total number of world volumes deleted by the reaper.",
	})
)

// Collectors returns every felis_* collector in a stable order. Production and
// tests register the same slice, so the test asserting the full set is exposed
// also pins the production surface.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		ServersTotal,
		StartDurationSeconds,
		ImageBuildFailuresTotal,
		ReaperWorldsDeletedTotal,
	}
}

// Register wires every felis_* collector into r.
//
// It is idempotent: re-registering an already-registered collector (a manager
// restart in-process, a test that calls Register twice) is tolerated rather
// than fatal, so a benign double-call never takes the operator down. Any other
// registration error is returned to the caller to surface at startup.
func Register(r prometheus.Registerer) error {
	for _, c := range Collectors() {
		if err := r.Register(c); err != nil {
			var already prometheus.AlreadyRegisteredError
			if errors.As(err, &already) {
				continue
			}
			return err
		}
	}
	return nil
}
