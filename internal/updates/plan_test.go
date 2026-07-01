package updates

import (
	"testing"
	"time"
)

func mustV(t *testing.T, s string) Version {
	t.Helper()
	v, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return v
}

func TestWindowContains(t *testing.T) {
	start := time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC)
	w := Window{Start: start, End: end}

	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"before", start.Add(-time.Minute), false},
		{"at start (inclusive)", start, true},
		{"inside", start.Add(30 * time.Minute), true},
		{"at end (exclusive)", end, false},
		{"after", end.Add(time.Minute), false},
	}
	for _, c := range cases {
		if got := w.Contains(c.now); got != c.want {
			t.Errorf("%s: Contains(%v) = %v, want %v", c.name, c.now, got, c.want)
		}
	}

	// Fail-closed windows contain nothing.
	if (Window{}).Contains(start) {
		t.Error("zero window must contain nothing")
	}
	if (Window{Start: end, End: start}).Contains(start.Add(30 * time.Minute)) {
		t.Error("inverted window must contain nothing")
	}
	if (Window{Start: start}).Contains(start) {
		t.Error("half-set window (no end) must contain nothing")
	}
}

// The load-bearing invariants live here. Each row is a single component evaluated
// against a latest map, at a fixed `now`, asserting the Kind the plan must yield.
func TestPlanUpdatesInvariants(t *testing.T) {
	now := time.Date(2026, 7, 1, 3, 30, 0, 0, time.UTC)     // inside the window below
	openWin := Window{
		Start: time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC),
	}
	closedWin := Window{
		Start: time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC), // tomorrow — now is outside
		End:   time.Date(2026, 7, 2, 4, 0, 0, 0, time.UTC),
	}

	cases := []struct {
		name    string
		comp    Component
		latest  string // "" ⇒ absent from the map (unknown latest)
		want    ActionKind
	}{
		{
			name:   "pinned is never touched even with a newer stable upstream",
			comp:   Component{Name: "mc-survival", Current: mustV(t, "1.20.1"), Policy: PolicyPinned, Manageable: true, Window: openWin},
			latest: "1.21.0",
			want:   ActionPinned,
		},
		{
			name:   "no downgrade: latest older than current",
			comp:   Component{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyScheduled, Manageable: true, Window: openWin},
			latest: "1.3.9",
			want:   ActionNone,
		},
		{
			name:   "same version is a no-op",
			comp:   Component{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyScheduled, Manageable: true, Window: openWin},
			latest: "1.4.0",
			want:   ActionNone,
		},
		{
			name:   "a newer PRERELEASE is never acted on",
			comp:   Component{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyScheduled, Manageable: true, Window: openWin},
			latest: "1.5.0-rc.1",
			want:   ActionNone,
		},
		{
			name:   "notify policy notifies on a newer stable",
			comp:   Component{Name: "k3s", Current: mustV(t, "v1.30.2+k3s1"), Policy: PolicyNotify, Manageable: true, Window: openWin},
			latest: "v1.30.3+k3s1",
			want:   ActionNotify,
		},
		{
			name:   "scheduled + manageable + inside window ⇒ apply",
			comp:   Component{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyScheduled, Manageable: true, Window: openWin},
			latest: "1.5.0",
			want:   ActionApply,
		},
		{
			name:   "scheduled but OUTSIDE window degrades to notify (nothing force-upgraded)",
			comp:   Component{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyScheduled, Manageable: true, Window: closedWin},
			latest: "1.5.0",
			want:   ActionNotify,
		},
		{
			name:   "scheduled but NOT manageable (off-cluster velocity) degrades to notify",
			comp:   Component{Name: "velocity", Current: mustV(t, "3.3.0"), Policy: PolicyScheduled, Manageable: false, Window: openWin},
			latest: "3.4.0",
			want:   ActionNotify,
		},
		{
			name:   "scheduled with an UNSET window degrades to notify",
			comp:   Component{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyScheduled, Manageable: true, Window: Window{}},
			latest: "1.5.0",
			want:   ActionNotify,
		},
		{
			name:   "unknown latest (not discovered) is a no-op",
			comp:   Component{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyScheduled, Manageable: true, Window: openWin},
			latest: "",
			want:   ActionNone,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			latest := map[string]Version{}
			if c.latest != "" {
				latest[c.comp.Name] = mustV(t, c.latest)
			}
			got := PlanUpdates([]Component{c.comp}, latest, now)
			if len(got) != 1 {
				t.Fatalf("PlanUpdates returned %d actions, want 1", len(got))
			}
			if got[0].Kind != c.want {
				t.Errorf("Kind = %q, want %q", got[0].Kind, c.want)
			}
			// The current/latest pair must always be carried for the report.
			if got[0].Component != c.comp.Name {
				t.Errorf("Component = %q, want %q", got[0].Component, c.comp.Name)
			}
			if (c.latest != "") != got[0].LatestKnown {
				t.Errorf("LatestKnown = %v, want %v", got[0].LatestKnown, c.latest != "")
			}
		})
	}
}

// TestPlanUpdatesPreservesOrderAndPending runs a realistic fleet through the engine
// in one call and checks both output ordering and the Pending filter.
func TestPlanUpdatesPreservesOrderAndPending(t *testing.T) {
	now := time.Date(2026, 7, 1, 3, 30, 0, 0, time.UTC)
	win := Window{
		Start: time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC),
	}
	comps := []Component{
		{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyScheduled, Manageable: true, Window: win},   // apply
		{Name: "k3s", Current: mustV(t, "v1.30.2+k3s1"), Policy: PolicyNotify, Manageable: true},                  // notify
		{Name: "cloudflared", Current: mustV(t, "2024.2.1"), Policy: PolicyScheduled, Manageable: true},           // no window ⇒ notify
		{Name: "velocity", Current: mustV(t, "3.3.0"), Policy: PolicyScheduled, Manageable: false},                // off-cluster ⇒ notify
		{Name: "mc-survival", Current: mustV(t, "1.20.1"), Policy: PolicyPinned},                                  // pinned
	}
	latest := map[string]Version{
		"felis-api":   mustV(t, "1.5.0"),
		"k3s":         mustV(t, "v1.30.3+k3s1"),
		"cloudflared": mustV(t, "2024.3.0"),
		"velocity":    mustV(t, "3.4.0"),
		"mc-survival": mustV(t, "1.21.0"),
	}

	got := PlanUpdates(comps, latest, now)
	wantKinds := []ActionKind{ActionApply, ActionNotify, ActionNotify, ActionNotify, ActionPinned}
	if len(got) != len(wantKinds) {
		t.Fatalf("got %d actions, want %d", len(got), len(wantKinds))
	}
	for i, a := range got {
		if a.Component != comps[i].Name {
			t.Errorf("action %d component = %q, want %q (order not preserved)", i, a.Component, comps[i].Name)
		}
		if a.Kind != wantKinds[i] {
			t.Errorf("action %d (%s) Kind = %q, want %q", i, a.Component, a.Kind, wantKinds[i])
		}
	}

	pending := Pending(got)
	// felis-api (apply) + k3s, cloudflared, velocity (notify) = 4; pinned & none excluded.
	if len(pending) != 4 {
		t.Fatalf("Pending returned %d, want 4", len(pending))
	}
	for _, a := range pending {
		if a.Kind != ActionApply && a.Kind != ActionNotify {
			t.Errorf("Pending included a %q action for %s", a.Kind, a.Component)
		}
	}
}
