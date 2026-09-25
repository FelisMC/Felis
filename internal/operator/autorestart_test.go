package operator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/operator"
)

func gamePod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "survival-0", Namespace: "minecraft"}}
}

// A start that timed out is retried by recreating its pod after a doubling
// backoff (1m, 2m, 4m past the timeout), three times, then left Failed.
func TestTimedOutStartRecreatesThePodWithBackoff(t *testing.T) {
	srv := runningServer()
	srv.Spec.Startup.TimeoutSeconds = 30
	r, c := newReconciler(t, fakeProber{}, srv, rconSecret(), gamePod())
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }
	podExists := func() bool {
		err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-0"}, &corev1.Pod{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("get pod: %v", err)
		}
		return err == nil
	}
	at := func(offset time.Duration) *v1alpha1.MinecraftServer {
		clock = base.Add(offset)
		reconcile(t, r, "survival")
		return getServer(t, c, "survival")
	}

	at(0) // anchors the start at base
	// Each attempt: [anchor, the instant just before it is due, the due instant].
	attempts := [][3]time.Duration{
		{0, 89 * time.Second, 90 * time.Second},                   // 30s timeout + 1m
		{90 * time.Second, 239 * time.Second, 240 * time.Second},  // + 30s + 2m
		{240 * time.Second, 509 * time.Second, 510 * time.Second}, // + 30s + 4m
	}
	for i, a := range attempts {
		if s := at(a[1]); s.Status.Phase != v1alpha1.PhaseFailed || !podExists() || s.Status.AutoRestarts != int32(i) {
			t.Fatalf("attempt %d before due: phase=%s pod=%v restarts=%d", i+1, s.Status.Phase, podExists(), s.Status.AutoRestarts)
		}
		s := at(a[2])
		if podExists() {
			t.Fatalf("attempt %d: pod survived the due instant", i+1)
		}
		if s.Status.AutoRestarts != int32(i+1) || s.Status.Phase != v1alpha1.PhaseStarting {
			t.Fatalf("attempt %d: restarts=%d phase=%s", i+1, s.Status.AutoRestarts, s.Status.Phase)
		}
		if s.Status.LastAutoRestartAt == nil || !s.Status.LastAutoRestartAt.Time.Equal(base.Add(a[2])) {
			t.Fatalf("attempt %d: lastAutoRestartAt=%v", i+1, s.Status.LastAutoRestartAt)
		}
		if s.Status.StartRequestedAt == nil || !s.Status.StartRequestedAt.Time.Equal(base.Add(a[2])) {
			t.Fatalf("attempt %d: start not re-anchored: %v", i+1, s.Status.StartRequestedAt)
		}
		if err := c.Create(context.Background(), gamePod()); err != nil { // the StatefulSet's replacement
			t.Fatal(err)
		}
	}

	// Attempts spent: long after the next timeout the pod stays and so does Failed.
	s := at(time.Hour)
	if s.Status.Phase != v1alpha1.PhaseFailed || s.Status.AutoRestarts != 3 || !podExists() {
		t.Fatalf("after three attempts: phase=%s restarts=%d pod=%v", s.Status.Phase, s.Status.AutoRestarts, podExists())
	}
}

// An RCON channel that never answers on a TCP-ready pod gets the same retry.
func TestReadinessTimeoutRecreatesThePod(t *testing.T) {
	srv := runningServer()
	srv.Spec.Startup.ReadinessTimeoutSeconds = 30
	r, c := newReconciler(t, fakeProber{err: errors.New("connection refused")}, srv, rconSecret(), gamePod())
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	clock = base.Add(89 * time.Second)
	reconcile(t, r, "survival")
	if s := getServer(t, c, "survival"); s.Status.Phase != v1alpha1.PhaseFailed || s.Status.AutoRestarts != 0 {
		t.Fatalf("before due: phase=%s restarts=%d", s.Status.Phase, s.Status.AutoRestarts)
	}
	clock = base.Add(90 * time.Second)
	reconcile(t, r, "survival")
	s := getServer(t, c, "survival")
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-0"}, &corev1.Pod{})
	if !apierrors.IsNotFound(err) || s.Status.AutoRestarts != 1 || s.Status.Phase != v1alpha1.PhaseStarting {
		t.Fatalf("due: pod err=%v restarts=%d phase=%s", err, s.Status.AutoRestarts, s.Status.Phase)
	}
}

// switchProber fails while *down is true, so one server can miss and recover.
type switchProber struct{ down *bool }

func (p switchProber) Probe(context.Context, string, string) (operator.PlayerCount, error) {
	if *p.down {
		return operator.PlayerCount{}, errors.New("i/o timeout")
	}
	return operator.PlayerCount{Online: 1, Max: 20}, nil
}

func (switchProber) Save(context.Context, string, string) error { return nil }

// A Running server keeps its phase and endpoint through two missed probes,
// re-probing every 10s; the third consecutive miss degrades it to Starting,
// and any success in between starts the count over.
func TestRunningServerDegradesOnlyAfterThreeMisses(t *testing.T) {
	down := false
	r, c := newReconciler(t, switchProber{down: &down}, runningServer(), rconSecret())
	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	if res := reconcile(t, r, "survival"); res.RequeueAfter != time.Minute {
		t.Fatalf("healthy Running requeue = %v, want 1m", res.RequeueAfter)
	}
	stillRunning := func(label string) {
		t.Helper()
		s := getServer(t, c, "survival")
		if s.Status.Phase != v1alpha1.PhaseRunning || !s.Status.Ready || s.Status.Endpoint.Address != "10.43.0.42:25565" {
			t.Fatalf("%s: phase=%s ready=%v endpoint=%q", label, s.Status.Phase, s.Status.Ready, s.Status.Endpoint.Address)
		}
	}
	stillRunning("ready")

	down = true
	for i := 1; i <= 2; i++ {
		if res := reconcile(t, r, "survival"); res.RequeueAfter != 10*time.Second {
			t.Fatalf("miss %d requeue = %v, want 10s", i, res.RequeueAfter)
		}
		stillRunning("after a miss")
	}
	down = false
	reconcile(t, r, "survival") // a success clears the two misses
	down = true
	reconcile(t, r, "survival")
	reconcile(t, r, "survival")
	stillRunning("two misses after a recovery")

	reconcile(t, r, "survival") // third in a row
	if s := getServer(t, c, "survival"); s.Status.Phase != v1alpha1.PhaseStarting || s.Status.Ready {
		t.Fatalf("third miss: phase=%s ready=%v, want Starting not-ready", s.Status.Phase, s.Status.Ready)
	}
}

// A Failed server wakes when its next auto-restart falls due, and every 5m
// once the attempts are spent, instead of every 2s.
func TestFailedServerRequeuesWhenTheRetryIsDue(t *testing.T) {
	srv := runningServer()
	srv.Spec.Startup.TimeoutSeconds = 30
	r, c := newReconciler(t, fakeProber{}, srv, rconSecret(), gamePod())
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	if res := reconcile(t, r, "survival"); res.RequeueAfter != 2*time.Second {
		t.Fatalf("Starting requeue = %v, want 2s", res.RequeueAfter)
	}
	clock = base.Add(40 * time.Second) // timed out at 30s, first retry due at 90s
	if res := reconcile(t, r, "survival"); res.RequeueAfter != 50*time.Second {
		t.Fatalf("Failed requeue = %v, want 50s", res.RequeueAfter)
	}

	s := getServer(t, c, "survival")
	s.Status.AutoRestarts = 3
	if err := c.Status().Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	clock = base.Add(41 * time.Second)
	if res := reconcile(t, r, "survival"); res.RequeueAfter != 5*time.Minute {
		t.Fatalf("spent requeue = %v, want 5m", res.RequeueAfter)
	}
}

// noCachedSecrets stands in for the manager's cached client, which has no Secret
// informer: any Secret read through it fails.
type noCachedSecrets struct{ client.Client }

func (c noCachedSecrets) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		return errors.New("secrets are not cached")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// RCON Secrets are provisioned and read through Reconciler.Secrets alone, so the
// operator runs with secrets:get and no Secret informer.
func TestRconSecretsAreReadThroughTheUncachedReader(t *testing.T) {
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 2, Max: 20}}, runningServer())
	r.Client = noCachedSecrets{c}
	r.Secrets = c
	reconcile(t, r, "survival") // provisions survival-rcon
	var secret corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-rcon"}, &secret); err != nil {
		t.Fatalf("rcon secret not provisioned: %v", err)
	}
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")
	if s := getServer(t, c, "survival"); s.Status.Phase != v1alpha1.PhaseRunning || !s.Status.Ready {
		t.Fatalf("phase=%s ready=%v, want Running ready", s.Status.Phase, s.Status.Ready)
	}
}

// staleOnce answers the first Secret Get NotFound, like a reader that lost the
// race to a concurrent create, then reads through.
type staleOnce struct {
	client.Reader
	missed bool
}

func (s *staleOnce) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if !s.missed {
		s.missed = true
		return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
	}
	return s.Reader.Get(ctx, key, obj, opts...)
}

// Losing the create race re-reads the winner through the same uncached reader.
func TestRconSecretCreateRaceRereadsUncached(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret())
	r.Client = noCachedSecrets{c}
	r.Secrets = &staleOnce{Reader: c}
	reconcile(t, r, "survival")
	var secret corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-rcon"}, &secret); err != nil {
		t.Fatal(err)
	}
	if got := string(secret.Data["password"]); got != "hunter2" {
		t.Fatalf("password = %q, want the winner's hunter2", got)
	}
	// The pass went on to build the workload instead of parking on RconSecretUnavailable.
	getSTS(t, c, "survival")
}
