package updater

import (
	"context"
	"time"

	"felis.lolicon.best/internal/updates"
)

// VersionGatherer reads the CURRENT version of a component from the running system.
// It is an integration seam: the real implementation shells out (`k3s --version`),
// inspects a running image tag, or reads a jar manifest — none of which this package
// does. Tests inject a fake. A gather failure for one component drops it from the
// plan (Felis will not compare a version it cannot read) rather than sinking the
// whole cycle.
type VersionGatherer interface {
	Current(ctx context.Context, spec Spec) (updates.Version, error)
}

// Runner is the caller that gives updates.Run its first production entry point. It
// assembles the platform topology into updates.Components — filling Current from the
// gatherer and the shared maintenance Window into Scheduled components — runs the
// pure plan, and renders the report. Notifier and Applier are optional: nil/nil is
// the report-only mode (per seams.go) Felis runs before the executors are wired.
type Runner struct {
	Gatherer VersionGatherer
	Source   updates.ReleaseSource
	Notifier updates.Notifier // optional; nil = do not notify
	Applier  updates.Applier  // optional; nil = report-only (apply actions record errNoApplier)
	Topology func() []Spec    // injectable for tests; nil defaults to the package Topology
}

// Result is one Run outcome: the decision core's RunResult, the rendered "版本号状态"
// report, and any per-component current-version gather failures.
type Result struct {
	RunResult    updates.RunResult
	Report       string
	GatherErrors map[string]error
}

// Run performs one update cycle for `now` against the maintenance `window`. The
// window is read by the caller (from platform_settings "update_window" via the API)
// and passed in, so the runner needs no DB seam; it is applied to every Scheduled
// component. A component whose current version cannot be gathered is skipped and
// recorded in GatherErrors rather than planned against a zero version. The error
// return mirrors updates.Run's (collected, non-fatal today) so a future hard-stop has
// a channel.
func (rn *Runner) Run(ctx context.Context, now time.Time, window updates.Window) (Result, error) {
	topo := rn.Topology
	if topo == nil {
		topo = Topology
	}

	res := Result{GatherErrors: map[string]error{}}
	var comps []updates.Component
	for _, spec := range topo() {
		cur, err := rn.Gatherer.Current(ctx, spec)
		if err != nil {
			res.GatherErrors[spec.Name] = err
			continue
		}
		c := updates.Component{
			Name:       spec.Name,
			Current:    cur,
			Policy:     spec.Policy,
			Manageable: spec.Manageable,
		}
		// The window is only ever consulted for a Scheduled component (PlanUpdates
		// ignores it otherwise), but setting it only where it applies keeps intent clear.
		if spec.Policy == updates.PolicyScheduled {
			c.Window = window
		}
		comps = append(comps, c)
	}

	rr, err := updates.Run(ctx, rn.Source, rn.Notifier, rn.Applier, comps, now)
	res.RunResult = rr
	res.Report = updates.Report(rr.Plan)
	return res, err
}
