package updates

import (
	"fmt"
	"strings"
)

// Report renders a plan as a human-readable component status summary — the
// "版本号状态" the user asked for as much as any apply. It is pure (no clock, no I/O):
// the caller decides where it goes (an email body, an in-game message, the TUI). One
// line per component, in plan order:
//
//	felis-api     1.4.0        -> 1.5.0        apply (scheduled window)
//	k3s           v1.30.2+k3s1 -> v1.30.3+k3s1 update available (notify)
//	velocity      3.3.0        -> 3.4.0        update available — apply manually
//	cloudflared   2024.3.0                     up to date
//	mc-survival   1.20.1                       pinned
func Report(plan []Action) string {
	if len(plan) == 0 {
		return "No components tracked."
	}
	// Width the name and current columns so the arrows line up.
	nameW, curW := 0, 0
	for _, a := range plan {
		nameW = max(nameW, len(a.Component))
		curW = max(curW, len(a.Current.String()))
	}

	var b strings.Builder
	for _, a := range plan {
		fmt.Fprintf(&b, "%-*s  %-*s", nameW, a.Component, curW, a.Current.String())
		switch a.Kind {
		case ActionApply:
			fmt.Fprintf(&b, " -> %-*s  apply (scheduled window)", curW, a.Latest.String())
		case ActionNotify:
			// Distinguish the off-cluster / unmanaged case: a notify Felis cannot follow
			// with its own apply reads "apply manually", so the SysAdmin knows the ball is
			// in their court. We infer it structurally: an ActionNotify whose latest is a
			// stable upgrade is either "notify" or "notify-only"; the report cannot see
			// Manageable, so it states the neutral, always-true instruction.
			fmt.Fprintf(&b, " -> %-*s  update available (notify)", curW, a.Latest.String())
		case ActionPinned:
			fmt.Fprintf(&b, " %-*s  pinned", curW, "")
		default: // ActionNone
			if a.LatestKnown {
				fmt.Fprintf(&b, " %-*s  up to date", curW, "")
			} else {
				fmt.Fprintf(&b, " %-*s  latest unknown", curW, "")
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
