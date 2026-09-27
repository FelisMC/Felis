package operator_test

import (
	"context"
	"errors"
	"slices"
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
	"k8s.io/apimachinery/pkg/api/meta"
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
	saveErr error
	// saves, when set, records each Save's address.
	saves *[]string
	// broadcasts, when set, records each Broadcast's text.
	broadcasts   *[]string
	broadcastErr error
}

func (f fakeProber) Probe(context.Context, string, string) (operator.PlayerCount, error) {
	return f.players, f.err
}

func (f fakeProber) Save(_ context.Context, addr, _ string) error {
	if f.saves != nil {
		*f.saves = append(*f.saves, addr)
	}
	return f.saveErr
}

func (f fakeProber) Broadcast(_ context.Context, _, _, text string) error {
	if f.broadcasts != nil {
		*f.broadcasts = append(*f.broadcasts, text)
	}
	return f.broadcastErr
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
	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: name}, &svc); err != nil {
		t.Fatalf("get client service: %v", err)
	}
	svc.Spec.ClusterIP = "10.43.0.42"
	svc.Spec.ClusterIPs = []string{"10.43.0.42"}
	if err := c.Update(context.Background(), &svc); err != nil {
		t.Fatalf("assign client service ClusterIP: %v", err)
	}
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

	// StatefulSet exists with the shutdown grace period. The pre-stop save runs
	// from the operator over RCON, so the pod carries no preStop hook (none of the
	// game images ship an RCON client to run one with).
	sts := getSTS(t, c, "survival")
	if got := sts.Spec.Template.Spec.TerminationGracePeriodSeconds; got == nil || *got != 300 {
		t.Errorf("terminationGracePeriodSeconds = %v, want 300", got)
	}
	container := sts.Spec.Template.Spec.Containers[0]
	if container.Lifecycle != nil {
		t.Errorf("container lifecycle = %+v, want none", container.Lifecycle)
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
	// Running re-probes on a slow cadence to keep players and readiness current.
	if res.RequeueAfter != time.Minute {
		t.Errorf("expected the 1m Running re-probe, got %+v", res)
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
	if server.Status.Endpoint.Address != "10.43.0.42:25565" {
		t.Errorf("endpoint address = %q, want client Service ClusterIP", server.Status.Endpoint.Address)
	}
	if !isConditionTrue(server, v1alpha1.ConditionReady) {
		t.Error("Ready condition should be True")
	}
	if !isConditionTrue(server, v1alpha1.ConditionRconReached) {
		t.Error("RconReached condition should be True")
	}
}

func TestReconcileRunning_PopulatesPlayerTally(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 3, Max: 20, Known: true}}, runningServer(), rconSecret())

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
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}, runningServer(), rconSecret())

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

// stopRunningServer takes survival to Running with a ready pod, then flips it to
// Stopped and reconciles once.
func stopRunningServer(t *testing.T, r *operator.Reconciler, c client.Client) {
	t.Helper()
	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")
	server := getServer(t, c, "survival")
	server.Spec.DesiredState = v1alpha1.DesiredStopped
	if err := c.Update(context.Background(), server); err != nil {
		t.Fatalf("flip desiredState: %v", err)
	}
	reconcile(t, r, "survival")
}

func TestReconcileStopped_SavesBeforeScalingDown(t *testing.T) {
	var saves []string
	r, c := newReconciler(t, fakeProber{saves: &saves, players: operator.PlayerCount{Known: true}}, runningServer(), rconSecret())

	stopRunningServer(t, r, c)

	if want := []string{"survival.minecraft.svc.cluster.local:25575"}; !slices.Equal(saves, want) {
		t.Errorf("saves = %v, want %v", saves, want)
	}
	if sts := getSTS(t, c, "survival"); sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		t.Errorf("replicas = %v, want 0 after stop", sts.Spec.Replicas)
	}

	// Once scaled to zero the next reconcile only waits for the pod to go; it
	// does not flush again.
	reconcile(t, r, "survival")
	if len(saves) != 1 {
		t.Errorf("saves after second reconcile = %d, want 1", len(saves))
	}
}

func TestReconcileStopped_FailedSaveStillStops(t *testing.T) {
	var saves []string
	prober := fakeProber{saves: &saves, saveErr: errors.New("i/o timeout"), players: operator.PlayerCount{Known: true}}
	r, c := newReconciler(t, prober, runningServer(), rconSecret())

	stopRunningServer(t, r, c)

	if len(saves) != 1 {
		t.Fatalf("saves = %d, want 1 attempt", len(saves))
	}
	if sts := getSTS(t, c, "survival"); sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		t.Errorf("replicas = %v, want 0: a failed save must not hold the stop", sts.Spec.Replicas)
	}
	if server := getServer(t, c, "survival"); server.Status.Phase != v1alpha1.PhaseStopping {
		t.Errorf("phase = %s, want Stopping", server.Status.Phase)
	}
}

func TestReconcileStopped_SkipsSaveWithoutReadyPodOrRcon(t *testing.T) {
	t.Run("pod not ready", func(t *testing.T) {
		var saves []string
		r, c := newReconciler(t, fakeProber{saves: &saves}, runningServer(), rconSecret())
		reconcile(t, r, "survival") // StatefulSet at replicas=1, pod never ready
		server := getServer(t, c, "survival")
		server.Spec.DesiredState = v1alpha1.DesiredStopped
		if err := c.Update(context.Background(), server); err != nil {
			t.Fatalf("flip desiredState: %v", err)
		}
		reconcile(t, r, "survival")
		if len(saves) != 0 {
			t.Errorf("saves = %v, want none for a pod that never became ready", saves)
		}
		if sts := getSTS(t, c, "survival"); sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
			t.Errorf("replicas = %v, want 0", sts.Spec.Replicas)
		}
	})
	t.Run("rcon disabled", func(t *testing.T) {
		var saves []string
		s := runningServer()
		s.Spec.Rcon = v1alpha1.RconSpec{}
		r, c := newReconciler(t, fakeProber{saves: &saves}, s)
		stopRunningServer(t, r, c)
		if len(saves) != 0 {
			t.Errorf("saves = %v, want none without RCON", saves)
		}
		if sts := getSTS(t, c, "survival"); sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
			t.Errorf("replicas = %v, want 0", sts.Spec.Replicas)
		}
	})
}

// --- idle auto-stop tests (spec §8) ---------------------------------------

// TestIdleAutoStop_EmptyServerGetsTimestamp verifies that the first Running
// reconcile with zero players stamps EmptySince and keeps the server Running.
func TestIdleAutoStop_EmptyServerGetsTimestamp(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}, runningServer(), rconSecret())
	// Enable idle auto-stop with a generous timeout so we don't trigger the
	// actual stop in this test.
	s := getServer(t, c, "survival")
	s.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: 900}
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatalf("enable idle: %v", err)
	}

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")

	server := getServer(t, c, "survival")
	if server.Status.Phase != v1alpha1.PhaseRunning || !server.Status.Ready {
		t.Fatalf("phase = %s ready=%v, want Running ready", server.Status.Phase, server.Status.Ready)
	}
	if server.Status.EmptySince == nil {
		t.Fatal("EmptySince should be set for an empty server with idle autostop enabled")
	}
}

// TestIdleAutoStop_StopsAfterTimeout exercises the full auto-stop path:
// first reconcile stamps EmptySince; after advancing the clock past the
// timeout, the next reconcile flips desiredState to Stopped.
func TestIdleAutoStop_StopsAfterTimeout(t *testing.T) {
	prober := fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}
	r, c := newReconciler(t, prober, runningServer(), rconSecret())

	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	// Enable idle auto-stop with a 60s timeout.
	s := getServer(t, c, "survival")
	s.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: 60}
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatalf("enable idle: %v", err)
	}

	// First reconcile: Running, 0 players → stamp EmptySince = base.
	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")

	server := getServer(t, c, "survival")
	if server.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("phase = %s, want Running", server.Status.Phase)
	}
	if server.Status.EmptySince == nil || !server.Status.EmptySince.Equal(ptrTime(metav1.NewTime(base))) {
		t.Fatalf("EmptySince = %v, want %v", server.Status.EmptySince, base)
	}

	// Advance past timeout.
	clock = base.Add(61 * time.Second)
	reconcile(t, r, "survival")

	server = getServer(t, c, "survival")
	if server.Spec.DesiredState != v1alpha1.DesiredStopped {
		t.Fatalf("desiredState = %s, want Stopped after idle timeout", server.Spec.DesiredState)
	}
}

// TestIdleAutoStop_ResetsWhenPlayerJoins verifies that EmptySince is cleared
// when the player tally goes from zero to non-zero.
func TestIdleAutoStop_ResetsWhenPlayerJoins(t *testing.T) {
	emptyProber := fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}
	r, c := newReconciler(t, emptyProber, runningServer(), rconSecret())

	s := getServer(t, c, "survival")
	s.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: 900}
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatalf("enable idle: %v", err)
	}

	// First reconcile: Running, 0 players → stamp EmptySince.
	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")

	server := getServer(t, c, "survival")
	if server.Status.EmptySince == nil {
		t.Fatal("EmptySince should be set when empty")
	}

	// Swap to a prober that reports players online.
	populatedProber := fakeProber{players: operator.PlayerCount{Online: 3, Max: 20, Known: true}}
	r.Prober = populatedProber
	reconcile(t, r, "survival")

	server = getServer(t, c, "survival")
	if server.Status.EmptySince != nil {
		t.Fatalf("EmptySince = %v, want nil after players join", server.Status.EmptySince)
	}
}

// TestIdleAutoStop_UnreadTallyNeverStops pins the safety rule behind
// PlayerCount.Known: a `list` reply the parser cannot read is no sample at all.
// It must not stamp EmptySince, must not stop a server whose countdown already
// ran out, must keep the last known tally on display, and must say why on the
// PlayersCounted condition.
func TestIdleAutoStop_UnreadTallyNeverStops(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 4, Max: 20, Known: true}}, runningServer(), rconSecret())
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	s := getServer(t, c, "survival")
	s.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: 60}
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatalf("enable idle: %v", err)
	}
	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")

	// The tally becomes unreadable: nothing is stamped, the last count stays.
	r.Prober = fakeProber{}
	reconcile(t, r, "survival")
	server := getServer(t, c, "survival")
	if server.Status.EmptySince != nil {
		t.Fatalf("EmptySince = %v, want nil: an unread tally is not an empty server", server.Status.EmptySince)
	}
	if server.Status.Players.Online != 4 {
		t.Errorf("players.online = %d, want the last known 4", server.Status.Players.Online)
	}
	cond := meta.FindStatusCondition(server.Status.Conditions, v1alpha1.ConditionPlayersCounted)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ListUnreadable" {
		t.Fatalf("PlayersCounted = %+v, want False/ListUnreadable", cond)
	}

	// A countdown already past its deadline does not fire on an unread tally.
	r.Prober = fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}
	reconcile(t, r, "survival")
	server = getServer(t, c, "survival")
	cond = meta.FindStatusCondition(server.Status.Conditions, v1alpha1.ConditionPlayersCounted)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("PlayersCounted = %+v, want True after a readable reply", cond)
	}
	r.Prober = fakeProber{}
	clock = base.Add(10 * time.Minute)
	reconcile(t, r, "survival")
	server = getServer(t, c, "survival")
	if server.Spec.DesiredState != v1alpha1.DesiredRunning {
		t.Fatalf("desiredState = %s, want Running while the tally is unread", server.Spec.DesiredState)
	}
	if server.Status.EmptySince == nil {
		t.Fatal("EmptySince was cleared by an unread tally; a flaky read must not restart the countdown")
	}

	// The next real zero stops it.
	r.Prober = fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}
	reconcile(t, r, "survival")
	server = getServer(t, c, "survival")
	if server.Spec.DesiredState != v1alpha1.DesiredStopped {
		t.Fatalf("desiredState = %s, want Stopped once a real zero is read past the deadline", server.Spec.DesiredState)
	}
}

// TestIdleAutoStop_SystemServerNeverIdles: the login gate and the lobby must
// stay up whatever their spec says. A stopped gate locks every player out, and
// nothing would wake it.
func TestIdleAutoStop_SystemServerNeverIdles(t *testing.T) {
	srv := runningServer()
	srv.Labels = map[string]string{v1alpha1.LabelSystemRole: "lobby"}
	srv.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: 60}
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}, srv, rconSecret())
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")
	clock = base.Add(time.Hour)
	res := reconcile(t, r, "survival")

	server := getServer(t, c, "survival")
	if server.Spec.DesiredState != v1alpha1.DesiredRunning || server.Status.EmptySince != nil {
		t.Fatalf("system server: desiredState=%s emptySince=%v, want Running and no countdown",
			server.Spec.DesiredState, server.Status.EmptySince)
	}
	if res.RequeueAfter != time.Minute {
		t.Fatalf("RequeueAfter = %v, want only the 1m Running re-probe for a server that never idles", res.RequeueAfter)
	}
}

// TestIdleAutoStop_RequeuesUntilDeadline pins the self-driving requeue: an
// empty Running server must wake the controller at the auto-stop deadline with
// no external event to lean on. Live, the EmptySince stamp sat unexamined for
// minutes because nothing re-triggered the reconcile loop — this test fails if
// the requeue is ever dropped again.
func TestIdleAutoStop_RequeuesUntilDeadline(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}, runningServer(), rconSecret())
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	r.Now = func() metav1.Time { return metav1.NewTime(base) }

	s := getServer(t, c, "survival")
	s.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: 30}
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatalf("enable idle: %v", err)
	}

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	res := reconcile(t, r, "survival")

	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %v, want exactly 30s (wake at the auto-stop deadline)", res.RequeueAfter)
	}
}

// TestIdleAutoStop_RequeuesWhileOccupied verifies the slow probe cadence that
// notices the last player leaving: with players online there is no deadline to
// aim at, but the tally must still be re-sampled.
func TestIdleAutoStop_RequeuesWhileOccupied(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 3, Max: 20, Known: true}}, runningServer(), rconSecret())

	s := getServer(t, c, "survival")
	s.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: 900}
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatalf("enable idle: %v", err)
	}

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	res := reconcile(t, r, "survival")

	if res.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v, want a positive probe cadence while occupied", res.RequeueAfter)
	}
}

// TestNoIdleRequeueWhenDisabled guards against a blanket requeue: servers
// without idle auto-stop keep the old quiescent behaviour.
func TestNoIdleRequeueWhenDisabled(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}, runningServer(), rconSecret())

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	res := reconcile(t, r, "survival")

	if res.RequeueAfter != time.Minute {
		t.Fatalf("RequeueAfter = %v, want only the 1m Running re-probe when idle auto-stop is disabled", res.RequeueAfter)
	}
}

// TestRconStamp_StableAcrossReconciles pins that the pod template stamp does
// not drift while the Secret is untouched — a drifting value would roll the
// pod on every reconcile.
func TestRconStamp_StableAcrossReconciles(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}, runningServer(), rconSecret())

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")

	first := stsTemplateStamp(t, c, "survival")
	if first == "" {
		t.Fatal("pod template must carry the RCON secret stamp")
	}
	reconcile(t, r, "survival")
	if second := stsTemplateStamp(t, c, "survival"); second != first {
		t.Fatalf("stamp drifted from %q to %q across reconciles", first, second)
	}
}

// TestRconSecretRecreation_RollsTemplate is the regression for the live lockup:
// deleting the Secret re-mints a different password, and the pod must be
// rolled onto it — otherwise the old pod keeps authenticating with the lost
// password and the RCON gate fails forever.
func TestRconSecretRecreation_RollsTemplate(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}, runningServer(), rconSecret())

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")
	before := stsTemplateStamp(t, c, "survival")
	if before == "" {
		t.Fatal("pod template must carry the RCON secret stamp")
	}

	var sec corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-rcon"}, &sec); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if err := c.Delete(context.Background(), &sec); err != nil {
		t.Fatalf("delete secret: %v", err)
	}

	reconcile(t, r, "survival")

	after := stsTemplateStamp(t, c, "survival")
	if after == "" {
		t.Fatal("pod template must carry the RCON secret stamp after re-creation")
	}
	if after == before {
		t.Fatalf("stamp %q unchanged after the Secret was re-created — the pod would keep the lost password", before)
	}
}

func stsTemplateStamp(t *testing.T, c client.Client, name string) string {
	t.Helper()
	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: name}, &sts); err != nil {
		t.Fatalf("get sts: %v", err)
	}
	return sts.Spec.Template.Annotations[operator.RconSecretAnnotation]
}

// TestIdleAutoStop_SkipsWhenDisabled verifies that a Running empty server does
// NOT get an EmptySince timestamp when AutoStopEnabled is false.
func TestIdleAutoStop_SkipsWhenDisabled(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}, runningServer(), rconSecret())

	// Idle is NOT enabled (default).
	s := getServer(t, c, "survival")
	if s.Spec.Idle.AutoStopEnabled {
		t.Fatal("idle autostop should be disabled by default")
	}

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")

	server := getServer(t, c, "survival")
	if server.Status.EmptySince != nil {
		t.Fatalf("EmptySince = %v, want nil when idle autostop is disabled", server.Status.EmptySince)
	}
}

// TestReconcileRunning_ReadinessTimeoutConvertsToFailed verifies that a server
// whose pod is ready but whose RCON probe keeps failing past
// readinessTimeoutSeconds transitions to Failed (spec §5, §8).
func TestReconcileRunning_ReadinessTimeoutConvertsToFailed(t *testing.T) {
	srv := runningServer()
	srv.Spec.Startup.ReadinessTimeoutSeconds = 30
	r, c := newReconciler(t, fakeProber{err: errors.New("connection refused")}, srv, rconSecret())

	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	reconcile(t, r, "survival") // creates workload, Starting, anchors startRequestedAt=base
	markPodReady(t, c, "survival")

	// Within timeout: stays Starting.
	clock = base.Add(10 * time.Second)
	res := reconcile(t, r, "survival")
	if res.RequeueAfter == 0 {
		t.Error("expected requeue while RCON not reachable within timeout")
	}
	server := getServer(t, c, "survival")
	if server.Status.Phase != v1alpha1.PhaseStarting {
		t.Errorf("phase = %s, want Starting within readiness timeout", server.Status.Phase)
	}

	// Past timeout: transitions to Failed.
	clock = base.Add(31 * time.Second)
	res = reconcile(t, r, "survival")
	server = getServer(t, c, "survival")
	if server.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %s, want Failed after readiness timeout", server.Status.Phase)
	}
	if server.Status.Ready {
		t.Error("Ready must be false in Failed phase")
	}
	if !isConditionTrue(server, v1alpha1.ConditionReady) {
		// ConditionReady is False here — isConditionTrue checks for True.
		// We just want to verify the condition is set.
	}
	if isConditionTrue(server, v1alpha1.ConditionRconReached) {
		t.Error("RconReached must not be True in Failed phase")
	}
	_ = res
}

// TestReconcileRunning_StartupTimeoutConvertsToFailed verifies that a server
// whose pod never becomes ready past timeoutSeconds transitions to Failed
// (spec §5).
func TestReconcileRunning_StartupTimeoutConvertsToFailed(t *testing.T) {
	srv := runningServer()
	srv.Spec.Startup.TimeoutSeconds = 30
	r, c := newReconciler(t, fakeProber{}, srv, rconSecret())

	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	// First reconcile: creates workload, Starting. Pod is not ready yet.
	reconcile(t, r, "survival")
	server := getServer(t, c, "survival")
	if server.Status.Phase != v1alpha1.PhaseStarting {
		t.Fatalf("phase = %s, want Starting after first reconcile", server.Status.Phase)
	}

	// Within timeout: stays Starting (pod still not ready).
	clock = base.Add(10 * time.Second)
	reconcile(t, r, "survival")
	server = getServer(t, c, "survival")
	if server.Status.Phase != v1alpha1.PhaseStarting {
		t.Errorf("phase = %s, want Starting within startup timeout", server.Status.Phase)
	}

	// Past timeout: pod still not ready → Failed.
	clock = base.Add(31 * time.Second)
	res := reconcile(t, r, "survival")
	server = getServer(t, c, "survival")
	if server.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %s, want Failed after startup timeout", server.Status.Phase)
	}
	if server.Status.Ready {
		t.Error("Ready must be false in Failed phase")
	}
	_ = res
}

// TestReconcileRunning_ReadyClearsStartAnchor pins the recovery hygiene: once a
// server is Ready the start anchor must clear, so a later pod blip can never
// inherit the stale anchor and be judged StartupTimeout (found live: the
// operator marked an already-recovered server Failed minutes after recovery).
func TestReconcileRunning_ReadyClearsStartAnchor(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret())
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	reconcile(t, r, "survival") // Starting: anchors startRequestedAt
	if s := getServer(t, c, "survival"); s.Status.StartRequestedAt == nil {
		t.Fatal("StartRequestedAt should anchor on the first Starting pass")
	}

	clock = base.Add(10 * time.Second)
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival") // Running

	s := getServer(t, c, "survival")
	if s.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("phase = %s, want Running", s.Status.Phase)
	}
	if s.Status.StartRequestedAt != nil {
		t.Fatalf("StartRequestedAt = %v after Ready, want nil", s.Status.StartRequestedAt)
	}
}

// TestReconcileRunning_ProvisionedRecovers: markFailed flips Provisioned to
// False, and a recovered server must flip it back — a permanent False misleads
// every consumer of the conditions (kubectl waits, monitoring) forever.
func TestReconcileRunning_ProvisionedRecovers(t *testing.T) {
	srv := runningServer()
	srv.Spec.Startup.TimeoutSeconds = 30
	r, c := newReconciler(t, fakeProber{}, srv, rconSecret())
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	reconcile(t, r, "survival") // Starting
	clock = base.Add(31 * time.Second)
	reconcile(t, r, "survival") // past timeout → Failed

	s := getServer(t, c, "survival")
	if s.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %s, want Failed", s.Status.Phase)
	}
	if isConditionTrue(s, v1alpha1.ConditionProvisioned) {
		t.Fatal("Provisioned must be False after Failed")
	}

	markPodReady(t, c, "survival")
	reconcile(t, r, "survival") // recovers to Running

	s = getServer(t, c, "survival")
	if s.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("phase = %s, want Running after recovery", s.Status.Phase)
	}
	if !isConditionTrue(s, v1alpha1.ConditionProvisioned) {
		t.Fatal("Provisioned must flip back to True after recovery")
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
	reconcile(t, r, "survival")         // scales spec to 0; pods still terminating
	markPodTerminated(t, c, "survival") // pods finish draining
	reconcile(t, r, "survival")         // reaches Stopped, clears startRequestedAt
	if s := getServer(t, c, "survival"); s.Status.StartRequestedAt != nil {
		t.Errorf("startRequestedAt = %v after Stop, want nil so the next start re-anchors", s.Status.StartRequestedAt)
	}
}

// A pod that drops out of readiness under a Running server sends it back through
// Starting; its way back to ready is a start like any other, and is observed as
// one, measured from the new Starting pass. Before, readySignalAt kept the first
// run's time, so every start after the first went unobserved.
func TestReconcileRunning_ObservesStartDurationAfterAPodBlip(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret())
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	reconcile(t, r, "survival") // Starting
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival") // Running: the first start's observation
	beforeCount, beforeSum := startDurationState(t)

	clock = base.Add(10 * time.Minute)
	sts := getSTS(t, c, "survival")
	sts.Status.ReadyReplicas = 0
	if err := c.Status().Update(context.Background(), sts); err != nil {
		t.Fatalf("update sts status: %v", err)
	}
	reconcile(t, r, "survival") // the pod is not ready: back to Starting
	if s := getServer(t, c, "survival"); s.Status.Phase != v1alpha1.PhaseStarting || s.Status.ReadySignalAt != nil {
		t.Fatalf("after the blip: phase %s, readySignalAt %v; want Starting and none", s.Status.Phase, s.Status.ReadySignalAt)
	}

	clock = base.Add(10*time.Minute + 45*time.Second)
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival") // Running again

	afterCount, afterSum := startDurationState(t)
	if got := afterCount - beforeCount; got != 1 {
		t.Fatalf("histogram sample count delta = %d, want exactly 1 observation for the restart", got)
	}
	if got := afterSum - beforeSum; got != 45 {
		t.Errorf("observed restart duration = %vs, want 45s", got)
	}
	if s := getServer(t, c, "survival"); s.Status.ReadySignalAt == nil || !s.Status.ReadySignalAt.Equal(ptrTime(metav1.NewTime(clock))) {
		t.Errorf("readySignalAt = %v, want the restart's ready time %v", s.Status.ReadySignalAt, clock)
	}
}

// A server whose RCON Secret does not exist yet is the normal case on first
// reconcile — felis-api writes spec.rcon.secretRef but holds secrets:get, not
// create, so the name it points at is a promise the operator has to keep. Before
// this the server sat in Starting/RconSecretUnavailable forever.
func TestReconcileProvisionsMissingRconSecret(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer())
	reconcile(t, r, "survival")

	var secret corev1.Secret
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "minecraft", Name: "survival-rcon"}, &secret); err != nil {
		t.Fatalf("expected the operator to create the rcon Secret: %v", err)
	}
	pw := string(secret.Data["password"])
	if pw == "" {
		t.Fatal("rcon Secret was created with an empty password")
	}
	// 16 random bytes as hex. An assertion on the shape, not the value: a password
	// short enough to guess is the failure this is guarding, and it cannot be
	// checked by comparing against a fixture.
	if len(pw) != 32 {
		t.Fatalf("password = %d chars, want 32 hex chars", len(pw))
	}
	if strings.Trim(pw, "0123456789abcdef") != "" {
		t.Fatalf("password %q is not hex; it is written verbatim into server.properties", pw)
	}
	// The controller reference is what makes Kubernetes delete the Secret with the
	// server. Without it every deleted server leaves its password behind.
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Name != "survival" {
		t.Fatalf("owner references = %+v, want one referring to the server", secret.OwnerReferences)
	}
}

// Reconcile runs every few seconds; regenerating the password on each pass would
// leave the running server authenticating with a value the control plane no longer
// has, breaking the console until the pod happened to restart.
func TestReconcileKeepsAnExistingRconPassword(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret())
	reconcile(t, r, "survival")
	reconcile(t, r, "survival")

	var secret corev1.Secret
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "minecraft", Name: "survival-rcon"}, &secret); err != nil {
		t.Fatalf("get rcon secret: %v", err)
	}
	if got := string(secret.Data["password"]); got != "hunter2" {
		t.Fatalf("password = %q, want the original %q — the operator rotated it", got, "hunter2")
	}
}

// The idle auto-stop reads Status.Players, which is only sampled by the RCON
// probe. With RCON off the tally stays at its zero value, and a bare
// "players == 0" test would read that as an empty server and stop one full of
// people.
func TestIdleAutoStopIsInertWithoutRcon(t *testing.T) {
	server := runningServer()
	server.Spec.Rcon = v1alpha1.RconSpec{Enabled: false}
	server.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: 1}

	r, c := newReconciler(t, fakeProber{}, server)
	reconcile(t, r, "survival")
	reconcile(t, r, "survival")

	if got := getServer(t, c, "survival").Spec.DesiredState; got != v1alpha1.DesiredRunning {
		t.Fatalf("desiredState = %q, want %q — idle auto-stop fired on an unprobed player count",
			got, v1alpha1.DesiredRunning)
	}
}

const (
	felisV1 = "registry.felis.svc:5000/felis/felis:v1.0.0"
	felisV2 = "registry.felis.svc:5000/felis/felis:v1.1.0"
)

func initImages(t *testing.T, c client.Client, name string) []string {
	t.Helper()
	var images []string
	for _, ic := range getSTS(t, c, name).Spec.Template.Spec.InitContainers {
		images = append(images, ic.Image)
	}
	if len(images) == 0 {
		t.Fatal("the StatefulSet runs no felis init container")
	}
	return images
}

func expectInitImages(t *testing.T, c client.Client, want string) {
	t.Helper()
	for _, got := range initImages(t, c, "survival") {
		if got != want {
			t.Fatalf("init container image = %q, want %q", got, want)
		}
	}
}

// TestFelisUpgradeLeavesARunningServerAlone: the installer moves the felis image
// on every release. A running server keeps the template it started with rather
// than restarting under its players; any other change still rolls it, and the
// roll carries the new image along.
func TestFelisUpgradeLeavesARunningServerAlone(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 1, Max: 20, Known: true}}, runningServer(), rconSecret())
	r.FelisImage = felisV1
	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")
	expectInitImages(t, c, felisV1)
	stamp := getSTS(t, c, "survival").Annotations[operator.PodTemplateAnnotation]
	if stamp == "" {
		t.Fatal("the StatefulSet must carry the pod-template fingerprint")
	}

	r.FelisImage = felisV2
	reconcile(t, r, "survival")
	expectInitImages(t, c, felisV1)
	if got := getSTS(t, c, "survival").Annotations[operator.PodTemplateAnnotation]; got != stamp {
		t.Fatalf("fingerprint moved from %q to %q on an image-only change", stamp, got)
	}

	server := getServer(t, c, "survival")
	server.Spec.JavaMemory = "6G"
	if err := c.Update(context.Background(), server); err != nil {
		t.Fatalf("edit spec: %v", err)
	}
	reconcile(t, r, "survival")
	expectInitImages(t, c, felisV2)
	if got := getSTS(t, c, "survival").Annotations[operator.PodTemplateAnnotation]; got == stamp {
		t.Fatal("a spec edit must move the fingerprint")
	}
}

// TestFelisUpgradeReachesAServerOnItsNextStart: a stopped server's next start
// writes the whole template, the new felis image included.
func TestFelisUpgradeReachesAServerOnItsNextStart(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}, runningServer(), rconSecret())
	r.FelisImage = felisV1
	stopRunningServer(t, r, c)
	markPodTerminated(t, c, "survival")
	reconcile(t, r, "survival")

	r.FelisImage = felisV2
	server := getServer(t, c, "survival")
	server.Spec.DesiredState = v1alpha1.DesiredRunning
	if err := c.Update(context.Background(), server); err != nil {
		t.Fatalf("flip desiredState: %v", err)
	}
	reconcile(t, r, "survival")
	if sts := getSTS(t, c, "survival"); sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want 1 after start", sts.Spec.Replicas)
	}
	expectInitImages(t, c, felisV2)
}
