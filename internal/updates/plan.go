package updates

import "time"

type Policy string

const (
	PolicyPinned Policy = "pinned"

	PolicyNotify Policy = "notify"

	// PolicyScheduled permits application only for manageable components inside the explicit window; otherwise it notifies.
	PolicyScheduled Policy = "scheduled"
)

// Window is [Start,End); zero or JSON-null endpoints leave it unset, matching the API's stored start and end fields.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Contains rejects unset, empty or inverted windows.
func (w Window) Contains(now time.Time) bool {
	if w.Start.IsZero() || w.End.IsZero() || !w.End.After(w.Start) {
		return false
	}
	return !now.Before(w.Start) && now.Before(w.End)
}

type Component struct {
	Name    string
	Current Version
	Policy  Policy
	// Manageable=false downgrades scheduled application to notification.
	Manageable bool
	Window     Window
}

type ActionKind string

const (
	ActionNone   ActionKind = "none"
	ActionPinned ActionKind = "pinned"
	ActionNotify ActionKind = "notify"
	ActionApply  ActionKind = "apply"
)

type Action struct {
	Component   string
	Current     Version
	Latest      Version
	LatestKnown bool
	Policy      Policy
	Kind        ActionKind
}

// PlanUpdates preserves input order and permits only stable upgrades; pinned, unknown or outside-window components cannot be applied.
func PlanUpdates(components []Component, latest map[string]Version, now time.Time) []Action {
	actions := make([]Action, 0, len(components))
	for _, c := range components {
		a := Action{Component: c.Name, Current: c.Current, Policy: c.Policy}
		lv, known := latest[c.Name]
		if known {
			a.Latest = lv
			a.LatestKnown = true
		}

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

func Pending(actions []Action) []Action {
	out := make([]Action, 0, len(actions))
	for _, a := range actions {
		if a.Kind == ActionNotify || a.Kind == ActionApply {
			out = append(out, a)
		}
	}
	return out
}
