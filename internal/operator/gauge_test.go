package operator_test

import (
	"context"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/metrics"
	"felis.lolicon.best/internal/operator"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func gaugeServer(name string, state v1alpha1.DesiredState) *v1alpha1.MinecraftServer {
	return &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "minecraft"},
		Spec:       v1alpha1.MinecraftServerSpec{Subdomain: name, DesiredState: state},
	}
}

// TestGaugeSyncerPublishesFleetStateFromCluster drives SyncOnce against a fake
// client to verify the whole production path bar the ticker: List the fleet,
// translate each server to its desiredState (an unset state defaulting to
// Stopped), and republish felis_servers_total partitioned by that state.
func TestGaugeSyncerPublishesFleetStateFromCluster(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			gaugeServer("a", v1alpha1.DesiredRunning),
			gaugeServer("b", v1alpha1.DesiredRunning),
			gaugeServer("c", v1alpha1.DesiredStopped),
			gaugeServer("d", ""), // unset desiredState must be counted as Stopped
		).
		Build()

	g := &operator.GaugeSyncer{Client: c}
	if err := g.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}

	if got := testutil.ToFloat64(metrics.ServersTotal.WithLabelValues("Running")); got != 2 {
		t.Errorf("servers_total{state=Running} = %v, want 2", got)
	}
	// One explicit Stopped plus one unset (defaulted to Stopped) = 2.
	if got := testutil.ToFloat64(metrics.ServersTotal.WithLabelValues("Stopped")); got != 2 {
		t.Errorf("servers_total{state=Stopped} = %v, want 2", got)
	}
}

// TestGaugeSyncerPublishesServerPhases: every server gets one felis_server_phase
// series carrying its system role, observed phase (Unknown before the first
// report) and desired state.
func TestGaugeSyncerPublishesServerPhases(t *testing.T) {
	login := gaugeServer("login", v1alpha1.DesiredRunning)
	login.Labels = map[string]string{v1alpha1.LabelSystemRole: "login"}
	login.Status.Phase = v1alpha1.PhaseFailed
	fresh := gaugeServer("fresh", "")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(login, fresh).Build()

	if err := (&operator.GaugeSyncer{Client: c}).SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	defer metrics.ServerPhase.Reset()
	if n := testutil.CollectAndCount(metrics.ServerPhase); n != 2 {
		t.Fatalf("felis_server_phase series = %d, want 2", n)
	}
	for _, labels := range [][]string{
		{"login", "login", "Failed", "Running"},
		{"fresh", "", "Unknown", "Stopped"},
	} {
		if got := testutil.ToFloat64(metrics.ServerPhase.WithLabelValues(labels...)); got != 1 {
			t.Errorf("felis_server_phase%v = %v, want 1", labels, got)
		}
	}
}
