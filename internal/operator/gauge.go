package operator

import (
	"context"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/metrics"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const defaultGaugeInterval = 30 * time.Second

// GaugeSyncer republishes full snapshots because per-server transition counters can drift after missed events.
type GaugeSyncer struct {
	Client   client.Client
	Interval time.Duration
}

// Start logs sync failures and retries on the next tick without stopping the runnable.
func (g *GaugeSyncer) Start(ctx context.Context) error {
	interval := g.Interval
	if interval <= 0 {
		interval = defaultGaugeInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
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

func (g *GaugeSyncer) SyncOnce(ctx context.Context) error {
	var list v1alpha1.MinecraftServerList
	if err := g.Client.List(ctx, &list); err != nil {
		return err
	}
	metrics.SyncServerGauge(serverStates(list.Items))
	metrics.SyncServerPhases(serverPhases(list.Items))
	return nil
}

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
