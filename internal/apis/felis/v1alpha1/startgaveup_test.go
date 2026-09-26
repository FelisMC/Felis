package v1alpha1_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

// StartGaveUp tells whoever waits on a start whether to keep waiting. The
// operator's own timeout path pins the retrying cases (autorestart_test.go);
// these are the failures it never retries.
func TestStartGaveUp(t *testing.T) {
	anchor := metav1.Now()
	status := func(phase v1alpha1.Phase, reason string, restarts int32, anchored bool) *v1alpha1.MinecraftServerStatus {
		s := &v1alpha1.MinecraftServerStatus{Phase: phase, AutoRestarts: restarts}
		if reason != "" {
			s.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason}}
		}
		if anchored {
			s.StartRequestedAt = &anchor
		}
		return s
	}
	cases := []struct {
		name string
		s    *v1alpha1.MinecraftServerStatus
		want bool
	}{
		{"starting", status(v1alpha1.PhaseStarting, "PodNotReady", 0, true), false},
		{"timed out, retries left", status(v1alpha1.PhaseFailed, v1alpha1.ReasonStartupTimeout, 2, true), false},
		{"rcon timed out, retries left", status(v1alpha1.PhaseFailed, v1alpha1.ReasonReadinessTimeout, 0, true), false},
		{"timed out, retries spent", status(v1alpha1.PhaseFailed, v1alpha1.ReasonStartupTimeout, v1alpha1.MaxAutoRestarts, true), true},
		{"invalid spec is never retried", status(v1alpha1.PhaseFailed, "InvalidSpec", 0, true), true},
		{"no start anchor, nothing to retry from", status(v1alpha1.PhaseFailed, v1alpha1.ReasonStartupTimeout, 0, false), true},
		{"failed with no condition", status(v1alpha1.PhaseFailed, "", 0, true), true},
	}
	for _, c := range cases {
		if got := v1alpha1.StartGaveUp(c.s); got != c.want {
			t.Errorf("%s: StartGaveUp = %v, want %v", c.name, got, c.want)
		}
	}
}
