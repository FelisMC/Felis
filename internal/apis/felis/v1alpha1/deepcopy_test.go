package v1alpha1_test

import (
	"slices"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

func TestDeepCopyPreservesIndependentStopCommands(t *testing.T) {
	for _, commands := range [][]string{nil, {}, {"say before stop", "save-all"}} {
		source := &v1alpha1.MinecraftServer{Spec: v1alpha1.MinecraftServerSpec{
			Lifecycle: v1alpha1.LifecycleSpec{StopCommands: commands, TerminationGracePeriodSeconds: 45},
		}}
		copied := source.DeepCopy()
		if (copied.Spec.Lifecycle.StopCommands == nil) != (commands == nil) || !slices.Equal(copied.Spec.Lifecycle.StopCommands, commands) {
			t.Fatal("deep copy changed the command list shape")
		}
		if copied.Spec.Lifecycle.TerminationGracePeriodSeconds != 45 {
			t.Fatal("deep copy changed the grace period")
		}
		if len(commands) == 0 {
			continue
		}
		copied.Spec.Lifecycle.StopCommands[0] = "changed"
		if source.Spec.Lifecycle.StopCommands[0] != "say before stop" {
			t.Fatal("changing the copied commands modified the source")
		}
	}
}
