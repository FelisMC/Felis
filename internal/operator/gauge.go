package operator

import (
	"context"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/metrics"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// defaultGaugeInterval is the republish cadence used when none is configured.
const defaultGaugeInterval = 30 * time.Second

// GaugeSyncer periodically republishes felis_servers_total (spec §23) and
// felis_server_phase from a full List of MinecraftServers.
//
// felis_servers_total is a fleet-wide gauge partitioned by desiredState, which a
// per-object Reconcile fundamentally cannot maintain: one reconcile observes a
// single server, so it could never Set a correct fleet count and inc/dec on
// transitions would drift on any missed event. A periodic full-snapshot sidesteps
// that — every tick re-derives the entire gauge from the live list (and
// metrics.SyncServerGauge Resets first), so the published value is always exactly
// the current fleet and never accumulates stale state.
//
// It is registered as a manager.Runnable (mgr.Add), sharing the manager's cached,
// namespace-scoped client and lifecycle; it stops when the manager's context is
// cancelled.
type GaugeSyncer struct {
	// Client is the manager's cached client (namespace-scoped like the operator).
	Client client.Client
	// Interval is the republish cadence; defaults to defaultGaugeInterval.
	Interval time.Duration
}

// Start runs the republish loop until ctx is cancelled, satisfying
// manager.Runnable. The ticker loop and the live List are the untestable I/O
// edge; the List->translate->gauge work it delegates to SyncOnce is exercised
// against a fake client. A failed sync is logged best-effort, not fatal — the
// next tick re-derives the gauge from scratch regardless.
func (g *GaugeSyncer) Start(ctx context.Context) error {
	interval := g.Interval
	if interval <= 0 {
		interval = defaultGaugeInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// Publish once up front so the gauge is populated before the first tick.
	if err := g.SyncOnce(ctx); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "gauge: initial fleet sync failed")
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := g.SyncOnce(ctx); err != nil {
				ctrl.LoggerFrom(ctx).Error(err, "gauge: fleet sync failed")
			}
		}
	}
}

// SyncOnce Lists the fleet once and republishes felis_servers_total and
// felis_server_phase from it.
// Separated from Start so the List->translate->gauge path is unit-testable
// against a fake client, leaving only the ticker loop untested.
func (g *GaugeSyncer) SyncOnce(ctx context.Context) error {
	var list v1alpha1.MinecraftServerList
	if err := g.Client.List(ctx, &list); err != nil {
		return err
	}
	metrics.SyncServerGauge(serverStates(list.Items))
	metrics.SyncServerPhases(serverPhases(list.Items))
	return nil
}

// serverPhases maps each server to its felis_server_phase labels. A server the
// operator has not reported on yet is Unknown; desiredState defaults as in
// serverStates.
func serverPhases(items []v1alpha1.MinecraftServer) []metrics.ServerPhaseSample {
	out := make([]metrics.ServerPhaseSample, 0, len(items))
	for i := range items {
		ms := &items[i]
		phase := string(ms.Status.Phase)
		if phase == "" {
			phase = string(v1alpha1.PhaseUnknown)
		}
		desired := string(ms.Spec.DesiredState)
		if desired == "" {
			desired = string(v1alpha1.DesiredStopped)
		}
		out = append(out, metrics.ServerPhaseSample{
			Server:  ms.Name,
			Role:    ms.Labels[v1alpha1.LabelSystemRole],
			Phase:   phase,
			Desired: desired,
		})
	}
	return out
}

// serverStates maps each server to its desiredState, defaulting an unset state
// to Stopped (the CRD default, spec §4). Pure, so the labelling rule is covered
// by SyncOnce's fake-client test rather than only at runtime.
func serverStates(items []v1alpha1.MinecraftServer) []string {
	states := make([]string, 0, len(items))
	for i := range items {
		s := string(items[i].Spec.DesiredState)
		if s == "" {
			s = string(v1alpha1.DesiredStopped)
		}
		states = append(states, s)
	}
	return states
}
