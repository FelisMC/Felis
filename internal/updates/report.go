package updates

import (
	"fmt"
	"strings"
)

func Report(plan []Action) string {
	if len(plan) == 0 {
		return "No components tracked."
	}
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
			fmt.Fprintf(&b, " -> %-*s  update available (notify)", curW, a.Latest.String())
		case ActionPinned:
			fmt.Fprintf(&b, " %-*s  pinned", curW, "")
		default:
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
