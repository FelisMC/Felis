package updates

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The three interfaces below are the integration seams of the self-update
// subsystem. This package declares them and orchestrates them (Run), but ships NO
// production implementation of any of them — those are INTEGRATION-ONLY and live
// with the caller (cmd/felis, internal/platform), where the network, SMTP, kubectl
// and systemd actually are. Declaring the seams here lets the whole flow —
// discover → plan → notify → apply — be unit-tested against fakes, exactly as
// internal/cfsetup tests its Setup against a recordingRunner.

// ReleaseSource discovers the latest upstream version of a component.
// INTEGRATION-ONLY implementations: the GitHub Releases API (Felis control-plane,
// k3s, cloudflared) and the PaperMC API (Velocity). A pinned component is never
// queried (Run skips it), so a source need not answer for Minecraft.
type ReleaseSource interface {
	Latest(ctx context.Context, comp Component) (Version, error)
}

// Notifier delivers the pending plan to SysAdmin-level users. The user red line is
// that updates are NEVER silently forced: a SysAdmin is told — over SMTP or an
// in-game message — and then sets the maintenance window in the Panel. Even an
// apply that is about to run inside its window is announced. INTEGRATION-ONLY
// implementations reuse the existing OTP mailer (SMTP) and the velocity control
// channel (in-game).
type Notifier interface {
	Notify(ctx context.Context, pending []Action) error
}

// Applier applies one approved (ActionApply) update to a component Felis manages
// (a control-plane image bump + rollout; a cloudflared binary swap + service
// restart). It is deliberately PARTIAL: there is no blind k3s cluster-upgrade
// applier here, because k3s upgrades the single node the whole platform runs on —
// it defaults to PolicyNotify so a human drives it out of band. An Applier asked to
// handle a component it does not manage MUST return an error, never silently
// succeed, so a mis-scheduled apply is loud, not lost.
type Applier interface {
	Apply(ctx context.Context, action Action) error
}

// errNoApplier is recorded for an ActionApply that had no Applier wired — the plan
// decided to apply but nothing could carry it out.
var errNoApplier = errors.New("updates: no applier wired for an apply action")

// RunResult is the outcome of one Run: the full plan (for the status report), plus
// which pending actions were notified and which applies actually ran. Errors are
// collected rather than fatal — a single source or apply failure must not sink the
// rest of the cycle.
type RunResult struct {
	Plan     []Action
	Notified []Action
	Applied  []Action
	// SourceErrors are per-component "could not discover latest" failures, keyed by
	// component name. A component that errored simply has no latest and plans to
	// ActionNone.
	SourceErrors map[string]error
	// ApplyErrors are per-component apply failures, keyed by component name.
	ApplyErrors map[string]error
	// NotifyErr is a non-nil delivery failure from the Notifier; it does not block
	// applies (the SysAdmin can still see the state in the Panel).
	NotifyErr error
}

// Run executes one self-update cycle: discover each non-pinned component's latest
// version, plan against `now`, notify SysAdmins of everything pending, and apply
// only the ActionApply subset. It reads no clock of its own (`now` is injected) and
// does all I/O through the injected seams, so it is fully unit-testable. A nil
// notifier or applier simply skips that stage (recording errNoApplier for any apply
// that then cannot run), which is how the demo runs report-only before the
// executors are wired. Run never returns a fatal error today — failures are
// collected per component in the result — but the error return is kept so a future
// hard-stop condition (e.g. a cancelled context) has a channel.
func Run(ctx context.Context, source ReleaseSource, notifier Notifier, applier Applier, components []Component, now time.Time) (RunResult, error) {
	res := RunResult{
		SourceErrors: map[string]error{},
		ApplyErrors:  map[string]error{},
	}

	// Discover the latest version for every NON-pinned component. Pinned components
	// ("能不动的就别动") are not even queried upstream — Felis leaves Minecraft alone.
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
			// A single component's discovery failure must not sink the cycle; it simply
			// has no known latest and plans to ActionNone.
			res.SourceErrors[c.Name] = err
			continue
		}
		latest[c.Name] = v
	}

	res.Plan = PlanUpdates(components, latest, now)
	pending := Pending(res.Plan)

	// Tell SysAdmins about everything pending — both notify- and apply-kind — because
	// the requirement is that a human is always informed, even of a scheduled apply.
	if len(pending) > 0 && notifier != nil {
		if err := notifier.Notify(ctx, pending); err != nil {
			res.NotifyErr = err
		} else {
			res.Notified = pending
		}
	}

	// Apply only the ActionApply subset. Everything else is report/notify only.
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
