package operator_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/metrics"
	"felis.lolicon.best/internal/operator"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeProber struct {
	err     error
	players operator.PlayerCount
}

func (f fakeProber) Probe(context.Context, string, string) (operator.PlayerCount, error) {
	return f.players, f.err
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("clientgo scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("v1alpha1 scheme: %v", err)
	}
	return scheme
}

// fixedNow yields a deterministic timestamp so ReadySignalAt is assertable.
func fixedNow() metav1.Time {
	return metav1.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
}

func runningServer() *v1alpha1.MinecraftServer {
	return &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft", Generation: 1},
		Spec: v1alpha1.MinecraftServerSpec{
			Subdomain:      "survival",
			DesiredState:   v1alpha1.DesiredRunning,
			Image:          "registry.internal/felis/paper:latest",
			JavaMemory:     "4G",
			Storage:        v1alpha1.StorageSpec{Size: "10Gi"},
			FallbackServer: "lobby",
			Motd:           v1alpha1.MotdSpec{Running: "up", Stopped: "down", Starting: "booting"},
			Lifecycle:      v1alpha1.LifecycleSpec{PreStopSaveAndStop: true},
			Rcon: v1alpha1.RconSpec{
				Enabled:   true,
				Port:      25575,
				SecretRef: v1alpha1.SecretKeyRef{Name: "survival-rcon", Key: "password"},
			},
		},
	}
}

func rconSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "survival-rcon", Namespace: "minecraft"},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
}

func newReconciler(t *testing.T, prober operator.Prober, objs ...client.Object) (*operator.Reconciler, client.Client) {
	t.Helper()
	scheme := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.MinecraftServer{}, &appsv1.StatefulSet{}).
		Build()
	return &operator.Reconciler{Client: c, Scheme: scheme, Prober: prober, Now: fixedNow}, c
}

func reconcile(t *testing.T, r *operator.Reconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "minecraft", Name: name},
	})
	if err != nil {
		t.Fatalf("Reconcile(%s): %v", name, err)
	}
	return res
}

func getServer(t *testing.T, c client.Client, name string) *v1alpha1.MinecraftServer {
	t.Helper()
	var s v1alpha1.MinecraftServer
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: name}, &s); err != nil {
		t.Fatalf("get server: %v", err)
	}
	return &s
}

func getSTS(t *testing.T, c client.Client, name string) *appsv1.StatefulSet {
	t.Helper()
	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: name}, &sts); err != nil {
		t.Fatalf("get statefulset: %v", err)
	}
	return &sts
}

// markPodReady simulates the kubelet flipping the StatefulSet to ready.
func markPodReady(t *testing.T, c client.Client, name string) {
	t.Helper()
	sts := getSTS(t, c, name)
	sts.Status.Replicas = 1
	sts.Status.ReadyReplicas = 1
	if err := c.Status().Update(context.Background(), sts); err != nil {
		t.Fatalf("update sts status: %v", err)
	}
}

// markPodTerminated simulates the StatefulSet's pods finishing termination after
// a scale-to-zero, which the fake client does not do on its own.
func markPodTerminated(t *testing.T, c client.Client, name string) {
	t.Helper()
	sts := getSTS(t, c, name)
	sts.Status.Replicas = 0
	sts.Status.ReadyReplicas = 0
	if err := c.Status().Update(context.Background(), sts); err != nil {
		t.Fatalf("update sts status: %v", err)
	}
}

func TestReconcileRunning_CreatesWorkloadAndInjectsGracefulShutdown(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret())

	res := reconcile(t, r, "survival")
	if res.RequeueAfter == 0 {
		t.Errorf("expected a requeue while Starting, got %+v", res)
	}

	// Services exist.
	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &svc); err != nil {
		t.Fatalf("client service not created: %v", err)
	}
	var hl corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-hl"}, &hl); err != nil {
		t.Fatalf("headless service not created: %v", err)
	}
	if hl.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("headless service ClusterIP = %q, want None", hl.Spec.ClusterIP)
	}

	// StatefulSet exists with graceful-shutdown injection.
	sts := getSTS(t, c, "survival")
	if got := sts.Spec.Template.Spec.TerminationGracePeriodSeconds; got == nil || *got != 300 {
		t.Errorf("terminationGracePeriodSeconds = %v, want 300", got)
	}
	container := sts.Spec.Template.Spec.Containers[0]
	if container.Lifecycle == nil || container.Lifecycle.PreStop == nil || container.Lifecycle.PreStop.Exec == nil {
		t.Fatal("expected preStop exec hook to be injected")
	}
	preStop := strings.Join(container.Lifecycle.PreStop.Exec.Command, " ")
	if !strings.Contains(preStop, "save-all flush") || !strings.Contains(preStop, "stop") {
		t.Errorf("preStop hook missing save/stop sequence: %q", preStop)
	}
	// RCON password is sourced from the Secret, never inlined.
	var sawRconPassword bool
	for _, e := range container.Env {
		if e.Name == "RCON_PASSWORD" {
			sawRconPassword = true
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				t.Error("RCON_PASSWORD must come from a SecretKeyRef")
			}
			if e.Value != "" {
				t.Error("RCON_PASSWORD must not be inlined as a literal value")
			}
		}
	}
	if !sawRconPassword {
		t.Error("RCON_PASSWORD env not injected")
	}

	// Owner reference points back at the MinecraftServer.
	if len(sts.OwnerReferences) != 1 || sts.OwnerReferences[0].Name != "survival" {
		t.Errorf("statefulset owner refs = %+v, want one ref to survival", sts.OwnerReferences)
	}

	// The server pod must NOT mount a ServiceAccount token (spec §21): it runs
	// untrusted user worlds/plugins and has no business reaching the K8s API.
	if amt := sts.Spec.Template.Spec.AutomountServiceAccountToken; amt == nil || *amt {
		t.Error("server pod must set AutomountServiceAccountToken=false — untrusted workloads must not reach the API")
	}

	// Status is Starting (pod not yet ready).
	server := getServer(t, c, "survival")
	if server.Status.Phase != v1alpha1.PhaseStarting || server.Status.Ready {
		t.Errorf("status = %s ready=%v, want Starting not-ready", server.Status.Phase, server.Status.Ready)
	}
	if server.Status.Endpoint.Mode != v1alpha1.EndpointFallback || server.Status.Endpoint.Address != "lobby" {
		t.Errorf("starting endpoint = %+v, want fallback->lobby", server.Status.Endpoint)
	}
}

func TestReconcileRunning_RconProbeGatesReadiness(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret())

	reconcile(t, r, "survival") // creates workload, Starting
	markPodReady(t, c, "survival")

	res := reconcile(t, r, "survival") // pod ready + probe OK -> Running
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue once Running, got %+v", res)
	}

	server := getServer(t, c, "survival")
	if server.Status.Phase != v1alpha1.PhaseRunning || !server.Status.Ready {
		t.Fatalf("status = %s ready=%v, want Running ready", server.Status.Phase, server.Status.Ready)
	}
	if server.Status.ReadySignalAt == nil || !server.Status.ReadySignalAt.Equal(ptrTime(fixedNow())) {
		t.Errorf("readySignalAt = %v, want %v", server.Status.ReadySignalAt, fixedNow())
	}
	if server.Status.Endpoint.Mode != v1alpha1.EndpointDirect {
		t.Errorf("endpoint mode = %s, want direct", server.Status.Endpoint.Mode)
	}
	if !isConditionTrue(server, v1alpha1.ConditionReady) {
		t.Error("Ready condition should be True")
	}
	if !isConditionTrue(server, v1alpha1.ConditionRconReached) {
		t.Error("RconReached condition should be True")
	}
}

func TestReconcileRunning_PopulatesPlayerTally(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 3, Max: 20}}, runningServer(), rconSecret())

	reconcile(t, r, "survival") // creates workload, Starting
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival") // pod ready + probe OK -> Running

	server := getServer(t, c, "survival")
	if server.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("phase = %s, want Running", server.Status.Phase)
	}
	// The probe's tally must land on Status.Players so the panel stops reporting 0/0.
	if server.Status.Players.Online != 3 || server.Status.Players.Max != 20 {
		t.Errorf("status players = %d/%d, want 3/20", server.Status.Players.Online, server.Status.Players.Max)
	}
}

func TestReconcileRunning_RconProbeFailureStaysStarting(t *testing.T) {
	r, c := newReconciler(t, fakeProber{err: errors.New("connection refused")}, runningServer(), rconSecret())

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")

	res := reconcile(t, r, "survival") // pod ready but probe fails
	if res.RequeueAfter == 0 {
		t.Error("expected requeue while RCON not reachable")
	}
	server := getServer(t, c, "survival")
	if server.Status.Phase != v1alpha1.PhaseStarting || server.Status.Ready {
		t.Errorf("status = %s ready=%v, want Starting not-ready", server.Status.Phase, server.Status.Ready)
	}
	if isConditionTrue(server, v1alpha1.ConditionReady) {
		t.Error("Ready condition must not be True when the probe fails")
	}
}

func TestReconcileStopped_NoWorkloadIsStopped(t *testing.T) {
	s := runningServer()
	s.Spec.DesiredState = v1alpha1.DesiredStopped
	r, c := newReconciler(t, fakeProber{}, s)

	reconcile(t, r, "survival")

	if _, err := getSTSErr(c, "survival"); !apierrors.IsNotFound(err) {
		t.Errorf("stopped server should not create a StatefulSet, got err=%v", err)
	}
	server := getServer(t, c, "survival")
	if server.Status.Phase != v1alpha1.PhaseStopped || server.Status.Ready {
		t.Errorf("status = %s ready=%v, want Stopped not-ready", server.Status.Phase, server.Status.Ready)
	}
}

func TestReconcileStopped_ScalesRunningWorkloadDown(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret())

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival") // now Running with replicas=1

	// Flip desiredState to Stopped.
	server := getServer(t, c, "survival")
	server.Spec.DesiredState = v1alpha1.DesiredStopped
	if err := c.Update(context.Background(), server); err != nil {
		t.Fatalf("flip desiredState: %v", err)
	}

	reconcile(t, r, "survival")
	sts := getSTS(t, c, "survival")
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		t.Errorf("replicas = %v, want 0 after stop", sts.Spec.Replicas)
	}
}

// --- helpers ---------------------------------------------------------------

func getSTSErr(c client.Client, name string) (*appsv1.StatefulSet, error) {
	var sts appsv1.StatefulSet
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: name}, &sts)
	return &sts, err
}

func isConditionTrue(server *v1alpha1.MinecraftServer, condType string) bool {
	for _, cond := range server.Status.Conditions {
		if cond.Type == condType {
			return cond.Status == metav1.ConditionTrue
		}
	}
	return false
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }

// startDurationState reads the global felis_start_duration_seconds histogram's
// accumulated sample count and sum directly (Histogram implements Metric.Write),
// so assertions can be expressed as deltas and never depend on observations
// other tests made into the same process-wide collector.
func startDurationState(t *testing.T) (count uint64, sum float64) {
	t.Helper()
	var m dto.Metric
	if err := metrics.StartDurationSeconds.Write(&m); err != nil {
		t.Fatalf("read start_duration_seconds histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

// TestReconcileRunning_ObservesStartDuration proves the felis_start_duration_seconds
// wiring (spec §23) end-to-end across reconcile passes: the Starting pass anchors
// status.startRequestedAt, the value survives the patchStatus round-trip, and the
// first Running pass observes ReadySignalAt-StartRequestedAt into the histogram.
// A step-advancing clock (90s between the two passes) makes the observed duration
// a non-zero, exact value rather than the 0 a constant clock would yield.
func TestReconcileRunning_ObservesStartDuration(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret())

	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	clock := base
	// Override the constant fixedNow with a clock the test advances between passes.
	// now() is stable within a single reconcile; only the explicit bump moves it.
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	beforeCount, beforeSum := startDurationState(t)

	reconcile(t, r, "survival") // Starting: anchors startRequestedAt = base
	starting := getServer(t, c, "survival")
	if starting.Status.StartRequestedAt == nil || !starting.Status.StartRequestedAt.Equal(ptrTime(metav1.NewTime(base))) {
		t.Fatalf("startRequestedAt = %v, want %v", starting.Status.StartRequestedAt, base)
	}

	clock = base.Add(90 * time.Second) // 90s elapse before the pod reports ready
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival") // Running: observes 90s and sets readySignalAt

	ready := getServer(t, c, "survival")
	if ready.Status.Phase != v1alpha1.PhaseRunning || !ready.Status.Ready {
		t.Fatalf("status = %s ready=%v, want Running ready", ready.Status.Phase, ready.Status.Ready)
	}

	afterCount, afterSum := startDurationState(t)
	if got := afterCount - beforeCount; got != 1 {
		t.Fatalf("histogram sample count delta = %d, want exactly 1 observation", got)
	}
	if got := afterSum - beforeSum; got != 90 {
		t.Errorf("observed start duration = %vs, want 90s", got)
	}
}

// TestReconcileRunning_StartDurationObservedOnce guards against a re-observation
// bug: once a server is Running, further reconciles must not re-Observe the
// histogram (the once-only readySignalAt guard owns the Observe), and a Stop must
// clear startRequestedAt so a subsequent start re-anchors instead of measuring
// from the original boot.
func TestReconcileRunning_StartDurationObservedOnce(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret())

	reconcile(t, r, "survival") // Starting
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival") // Running -> one observation

	afterFirst, _ := startDurationState(t)

	reconcile(t, r, "survival") // still Running -> must NOT observe again
	afterSecond, _ := startDurationState(t)
	if afterSecond != afterFirst {
		t.Errorf("re-reconcile of a Running server observed again: %d -> %d", afterFirst, afterSecond)
	}

	// Stopping must clear the anchor so the next start measures afresh.
	stopped := getServer(t, c, "survival")
	stopped.Spec.DesiredState = v1alpha1.DesiredStopped
	if err := c.Update(context.Background(), stopped); err != nil {
		t.Fatalf("set desiredState=Stopped: %v", err)
	}
	reconcile(t, r, "survival")            // scales spec to 0; pods still terminating
	markPodTerminated(t, c, "survival")    // pods finish draining
	reconcile(t, r, "survival")            // reaches Stopped, clears startRequestedAt
	if s := getServer(t, c, "survival"); s.Status.StartRequestedAt != nil {
		t.Errorf("startRequestedAt = %v after Stop, want nil so the next start re-anchors", s.Status.StartRequestedAt)
	}
}
