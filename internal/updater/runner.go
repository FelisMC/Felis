package updater

import (
	"context"
	"time"

	"felis.lolicon.best/internal/updates"
)

// VersionGatherer errors omit that component from planning rather than comparing against an invented zero version.
type VersionGatherer interface {
	Current(ctx context.Context, spec Spec) (updates.Version, error)
}

// Runner combines gathered versions with topology and the supplied maintenance window; nil executors allow report-only checks.
type Runner struct {
	Gatherer VersionGatherer
	Source   updates.ReleaseSource
	Notifier updates.Notifier
	Applier  updates.Applier // nil records errNoApplier for apply actions
	Topology func() []Spec   // nil uses Topology
}

type Result struct {
	RunResult    updates.RunResult
	Report       string
	GatherErrors map[string]error
}

// Run records gather failures per component and applies window only to scheduled policies.
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
