package updater

import (
	"testing"

	"felis.lolicon.best/internal/updates"
)

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
