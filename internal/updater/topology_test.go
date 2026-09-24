package updater

import (
	"testing"

	"felis.lolicon.best/internal/updates"
)

// TestTopologyEncodesPolicies pins the user's stated decisions: which components Felis
// tracks and how aggressively. A regression here would silently change what Felis is
// allowed to auto-apply.
func TestTopologyEncodesPolicies(t *testing.T) {
	want := map[string]Spec{
		"felis-api":   {Name: "felis-api", Policy: updates.PolicyScheduled, Manageable: true, Source: sourceGitHub, Coord: "FelisMC/Felis"},
		"k3s":         {Name: "k3s", Policy: updates.PolicyNotify, Manageable: false, Source: sourceGitHub, Coord: "k3s-io/k3s"},
		"cloudflared": {Name: "cloudflared", Policy: updates.PolicyScheduled, Manageable: true, Source: sourceGitHub, Coord: "cloudflare/cloudflared"},
		"velocity":    {Name: "velocity", Policy: updates.PolicyNotify, Manageable: false, Source: sourcePaperMC, Coord: "velocity"},
		"jre":         {Name: "jre", Policy: updates.PolicyNotify, Manageable: false, Source: sourceTemurin},
		"postgresql":  {Name: "postgresql", Policy: updates.PolicyNotify, Manageable: false, Source: sourcePostgres},
	}
	got := Topology()
	if len(got) != len(want) {
		t.Fatalf("Topology has %d specs, want %d", len(got), len(want))
	}
	for _, s := range got {
		w, ok := want[s.Name]
		if !ok {
			t.Errorf("unexpected component %q in topology", s.Name)
			continue
		}
		if s != w {
			t.Errorf("component %q = %+v, want %+v", s.Name, s, w)
		}
	}
}

// TestTopologyPinsMinecraft proves the pin is expressed as ABSENCE: no component in
// the static topology is Pinned, because Minecraft is never force-tracked here — it
// is appended from the live fleet at runtime. And nothing off-cluster is proposed for
// self-apply: velocity must be non-manageable (Notify at most, never Apply).
func TestTopologyPinsMinecraft(t *testing.T) {
	for _, s := range Topology() {
		if s.Policy == updates.PolicyPinned {
			t.Errorf("component %q is Pinned in the static topology; Minecraft pins belong to the runtime fleet, not here", s.Name)
		}
		if s.Name == "velocity" && s.Manageable {
			t.Error("velocity is off-cluster and must be non-manageable (Notify only)")
		}
	}
}
