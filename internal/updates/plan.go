package updates

import "time"

// Policy is how Felis is allowed to act on a component's available update. The user
// red line is "不要强制自动更新" — nothing is force-upgraded. So there is no "auto"
// policy: the strongest a component can be is Scheduled, which still only applies
// inside a maintenance window a SysAdmin set in the Panel.
type Policy string

const (
	// PolicyPinned never changes and is never proposed. Every Minecraft server is
	// Pinned ("能不动的就别动"). A pinned component still appears in the report (so its
	// current-vs-latest is visible) but only ever as an informational line.
	PolicyPinned Policy = "pinned"

	// PolicyNotify detects a newer stable release and notifies SysAdmins, but Felis
	// never applies it — a human does, out of band. This is the default for anything
	// Felis does not itself manage (the off-cluster, admin-operated Velocity proxy)
	// and the safe default for high-blast-radius components (k3s upgrades the single
	// node the whole platform runs on).
	PolicyNotify Policy = "notify"

	// PolicyScheduled lets Felis apply a newer stable release ITSELF, but only for a
	// component it manages AND only while now falls inside the SysAdmin-set Window.
	// Outside the window it degrades to a notify. This is the opt-in the user
	// described: the SysAdmin goes to the Panel and sets WHEN the update runs.
	PolicyScheduled Policy = "scheduled"
)

// Window is a maintenance window a SysAdmin set (via the Panel) during which a
// Scheduled component may be applied. It is an absolute [Start, End) interval — the
// SysAdmin picks a concrete next window; recurrence is a caller-side concern layered
// on top. A zero Window (both ends zero) is "unset" and Contains always returns
// false, so a Scheduled component with no window set can never auto-apply — it holds
// at notify until a human actually schedules a time.
// The json tags are load-bearing across a package boundary: the maintenance window
// is persisted by internal/api (platform_settings key "update_window") as lowercase
// {"start","end"}, and the update runner reads those bytes back into a Window. With
// value (not pointer) time.Time, a stored null unmarshals as a no-op, so a cleared
// or never-set window decodes to the zero Window — Contains false, fails closed.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Contains reports whether now is inside the window. An unset (zero) or inverted
// (End not after Start) window contains nothing — it fails closed so a malformed
// schedule never opens an apply.
func (w Window) Contains(now time.Time) bool {
	if w.Start.IsZero() || w.End.IsZero() || !w.End.After(w.Start) {
		return false
	}
	return !now.Before(w.Start) && now.Before(w.End)
}

// Component is one tracked, versioned piece of the platform.
type Component struct {
	// Name is the stable key used to look up its latest version and to label reports
	// and notifications, e.g. "felis-api", "k3s", "cloudflared", "velocity".
	Name string
	// Current is the version running now (gathered by the caller — INTEGRATION: `k3s
	// --version`, an image tag, a jar inspection — never by this pure package).
	Current Version
	// Policy governs whether an available update is applied, merely notified, or
	// ignored (pinned).
	Policy Policy
	// Manageable is whether Felis can apply an update to this component ITSELF. It is
	// false for the off-cluster Velocity proxy (it runs on a separate macvlan host the
	// admin operates), so even under PolicyScheduled a non-manageable component can
	// only ever be notified, never applied — the plan degrades it honestly rather than
	// proposing an apply Felis cannot perform.
	Manageable bool
	// Window is consulted only when Policy is Scheduled.
	Window Window
}

// ActionKind is what the plan proposes for a component.
type ActionKind string

const (
	// ActionNone: nothing to do — already current, latest unknown, or the only newer
	// release upstream is a prerelease (which is never acted on).
	ActionNone ActionKind = "none"
	// ActionPinned: a pinned component; reported for visibility, never changed.
	ActionPinned ActionKind = "pinned"
	// ActionNotify: a newer stable release exists; notify SysAdmins so a human (or a
	// later scheduled window) can apply it.
	ActionNotify ActionKind = "notify"
	// ActionApply: a newer stable release exists, the component is Scheduled and
	// manageable, and now is inside its window — Felis may apply it.
	ActionApply ActionKind = "apply"
)

// Action is the plan for a single component: the decision plus the current/latest
// pair behind it, so the same slice drives BOTH the "版本号状态" status report and the
// notify/apply executors. It carries enough context to render a human line without
// re-deriving anything.
type Action struct {
	Component   string
	Current     Version
	Latest      Version
	LatestKnown bool
	Policy      Policy
	Kind        ActionKind
}

// PlanUpdates is the whole decision core. Given each component, the latest version
// discovered upstream keyed by Component.Name, and the current time, it returns one
// Action per component IN INPUT ORDER (deterministic — no maps are ranged for
// output). It performs no I/O and reads no clock of its own; `now` is injected so
// the window logic is unit-testable.
//
// The four load-bearing invariants, all provable from this function alone:
//
//   - A PolicyPinned component is ALWAYS ActionPinned — never Notify, never Apply.
//   - An Apply is proposed ONLY when there is a strictly-newer STABLE release
//     (After && !IsPrerelease), so a downgrade or a same-version is never applied and
//     a prerelease is never auto-applied.
//   - An Apply additionally requires PolicyScheduled AND Manageable AND the update
//     time falling inside the SysAdmin-set Window; anything short of all three
//     degrades to Notify (nothing is force-upgraded).
//   - A component with an unknown latest (not in the map) is ActionNone — an
//     undiscoverable version never triggers a change.
func PlanUpdates(components []Component, latest map[string]Version, now time.Time) []Action {
	actions := make([]Action, 0, len(components))
	for _, c := range components {
		a := Action{Component: c.Name, Current: c.Current, Policy: c.Policy}
		lv, known := latest[c.Name]
		if known {
			a.Latest = lv
			a.LatestKnown = true
		}

		// A pinned component is reported and otherwise untouched, regardless of what is
		// available upstream. This is checked FIRST so a pin is absolute.
		if c.Policy == PolicyPinned {
			a.Kind = ActionPinned
			actions = append(actions, a)
			continue
		}

		hasStableUpgrade := known && lv.After(c.Current) && !lv.IsPrerelease()
		switch {
		case !hasStableUpgrade:
			a.Kind = ActionNone
		case c.Policy == PolicyScheduled && c.Manageable && c.Window.Contains(now):
			a.Kind = ActionApply
		default:
			a.Kind = ActionNotify
		}
		actions = append(actions, a)
	}
	return actions
}

// Pending returns the subset of a plan that needs someone told or something done —
// the Notify and Apply actions. It is the input to the notification and apply
// stages; None and Pinned lines are report-only and filtered out here.
func Pending(actions []Action) []Action {
	out := make([]Action, 0, len(actions))
	for _, a := range actions {
		if a.Kind == ActionNotify || a.Kind == ActionApply {
			out = append(out, a)
		}
	}
	return out
}
