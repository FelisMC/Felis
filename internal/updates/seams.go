package updates

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ReleaseSource is never queried for pinned components.
type ReleaseSource interface {
	Latest(ctx context.Context, comp Component) (Version, error)
}

type Notifier interface {
	Notify(ctx context.Context, pending []Action) error
}

// Applier must reject components it cannot manage rather than reporting success without applying them.
type Applier interface {
	Apply(ctx context.Context, action Action) error
}

var errNoApplier = errors.New("updates: no applier wired for an apply action")

// RunResult collects per-component failures without aborting the update cycle.
type RunResult struct {
	Plan     []Action
	Notified []Action
	Applied  []Action
	// SourceErrors causes affected components to plan ActionNone.
	SourceErrors map[string]error
	ApplyErrors  map[string]error
	// NotifyErr does not block already-scheduled applications.
	NotifyErr error
}

// Run skips nil notification and application stages, recording errNoApplier for unapplied ActionApply entries.
func Run(ctx context.Context, source ReleaseSource, notifier Notifier, applier Applier, components []Component, now time.Time) (RunResult, error) {
	res := RunResult{
		SourceErrors: map[string]error{},
		ApplyErrors:  map[string]error{},
	}

	latest := map[string]Version{}
	for _, c := range components {
		if c.Policy == PolicyPinned {
			continue
		}
		if source == nil {
			res.SourceErrors[c.Name] = errors.New("updates: no release source wired")
			continue
		}
		v, err := source.Latest(ctx, c)
		if err != nil {
			res.SourceErrors[c.Name] = err
			continue
		}
		latest[c.Name] = v
	}

	res.Plan = PlanUpdates(components, latest, now)
	pending := Pending(res.Plan)

	if len(pending) > 0 && notifier != nil {
		if err := notifier.Notify(ctx, pending); err != nil {
			res.NotifyErr = err
		} else {
			res.Notified = pending
		}
	}

	for _, a := range res.Plan {
		if a.Kind != ActionApply {
			continue
		}
		if applier == nil {
			res.ApplyErrors[a.Component] = errNoApplier
			continue
		}
		if err := applier.Apply(ctx, a); err != nil {
			res.ApplyErrors[a.Component] = fmt.Errorf("apply %s: %w", a.Component, err)
			continue
		}
		res.Applied = append(res.Applied, a)
	}

	return res, nil
}
