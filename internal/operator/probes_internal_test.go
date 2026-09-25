package operator

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

func TestReadyAndStopResetAutoRestarts(t *testing.T) {
	r := &Reconciler{Now: func() metav1.Time { return metav1.NewTime(time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)) }}
	s := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft"}}
	s.Status.AutoRestarts = 2
	r.markRunningReady(s, PlayerCount{}, "10.43.0.9")
	if s.Status.AutoRestarts != 0 {
		t.Fatalf("ready: restarts = %d", s.Status.AutoRestarts)
	}
	s.Status.AutoRestarts = 3
	r.markStopped(s)
	if s.Status.AutoRestarts != 0 {
		t.Fatalf("stopped: restarts = %d", s.Status.AutoRestarts)
	}
}

func TestGameContainerProbes(t *testing.T) {
	srv := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft"}}
	srv.Spec.Image = "itzg/minecraft-server:java21"
	srv.Spec.Startup.TimeoutSeconds = 300
	sts, err := buildStatefulSet(srv, 1, "felis:test")
	if err != nil {
		t.Fatal(err)
	}
	c := sts.Spec.Template.Spec.Containers[0]
	if p := c.StartupProbe; p == nil || p.TCPSocket == nil || p.TCPSocket.Port.IntValue() != 25565 || p.PeriodSeconds != 10 || p.FailureThreshold != 37 {
		t.Fatalf("startup probe = %+v", p)
	}
	if p := c.LivenessProbe; p == nil || p.TCPSocket == nil || p.TCPSocket.Port.IntValue() != 25565 || p.PeriodSeconds != 20 || p.TimeoutSeconds != 5 || p.FailureThreshold != 6 {
		t.Fatalf("liveness probe = %+v", p)
	}

	srv.Spec.Startup.HealthHTTPPort = 8080
	srv.Spec.Startup.TimeoutSeconds = 0 // default 300s
	sts, err = buildStatefulSet(srv, 1, "felis:test")
	if err != nil {
		t.Fatal(err)
	}
	c = sts.Spec.Template.Spec.Containers[0]
	if p := c.LivenessProbe; p == nil || p.HTTPGet == nil || p.HTTPGet.Port.IntValue() != 8080 || p.HTTPGet.Path != "/healthz" {
		t.Fatalf("login-gate liveness = %+v", p)
	}
	if p := c.StartupProbe; p == nil || p.HTTPGet == nil || p.FailureThreshold != 37 {
		t.Fatalf("login-gate startup = %+v", p)
	}
}
