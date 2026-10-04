package operator_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

// requestStartRetry is felis-api's RetryStart as the operator sees it.
func requestStartRetry(t *testing.T, c client.Client) {
	t.Helper()
	s := getServer(t, c, "survival")
	patch := client.MergeFrom(s.DeepCopy())
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations[v1alpha1.AnnotationStartRetry] = "2026-07-01T12:00:00Z"
	if err := c.Patch(context.Background(), s, patch); err != nil {
		t.Fatalf("annotate: %v", err)
	}
}

func podPresent(t *testing.T, c client.Client) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-0"}, &corev1.Pod{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get pod: %v", err)
	}
	return err == nil
}

func TestExplicitRestartIsConsumedOnce(t *testing.T) {
	srv := runningServer()
	srv.Status.Phase = v1alpha1.PhaseRunning
	srv.Annotations = map[string]string{v1alpha1.AnnotationRestart: "2026-07-01T12:00:00Z"}
	r, c := newReconciler(t, fakeProber{}, srv, rconSecret(), gamePod())
	reconcile(t, r, "survival")
	s := getServer(t, c, "survival")
	if s.Annotations[v1alpha1.AnnotationRestart] != "" || s.Spec.DesiredState != v1alpha1.DesiredRunning || s.Status.Phase != v1alpha1.PhaseStarting || podPresent(t, c) {
		t.Fatalf("restart: desired=%s phase=%s request=%s pod=%v", s.Spec.DesiredState, s.Status.Phase, s.Annotations[v1alpha1.AnnotationRestart], podPresent(t, c))
	}
	if err := c.Create(context.Background(), gamePod()); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "survival")
	if !podPresent(t, c) {
		t.Fatal("consumed request deleted the replacement pod")
	}
}

func TestStopSupersedesExplicitRestart(t *testing.T) {
	srv := runningServer()
	srv.Spec.DesiredState = v1alpha1.DesiredStopped
	srv.Status.Phase = v1alpha1.PhaseRunning
	srv.Annotations = map[string]string{v1alpha1.AnnotationRestart: "2026-07-01T12:00:00Z"}
	r, c := newReconciler(t, fakeProber{}, srv, rconSecret(), gamePod())
	reconcile(t, r, "survival")
	s := getServer(t, c, "survival")
	if s.Annotations[v1alpha1.AnnotationRestart] != "" || s.Spec.DesiredState != v1alpha1.DesiredStopped || s.Status.Phase == v1alpha1.PhaseStarting {
		t.Fatalf("restart overrode stop: desired=%s phase=%s annotations=%v", s.Spec.DesiredState, s.Status.Phase, s.Annotations)
	}
}

// Once the automatic restarts are spent, a retry request starts the server over
// with the whole budget back: the pod is recreated, the start re-anchored, and
// the next timeout is retried automatically again.
func TestStartRetryRevivesAServerThatGaveUp(t *testing.T) {
	srv := runningServer()
	srv.Spec.Startup.TimeoutSeconds = 30
	srv.Spec.Motd.Failed = "broken"
	r, c := newReconciler(t, fakeProber{}, srv, rconSecret(), gamePod())
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }
	at := func(offset time.Duration) *v1alpha1.MinecraftServer {
		clock = base.Add(offset)
		reconcile(t, r, "survival")
		return getServer(t, c, "survival")
	}
	recreatePod := func() {
		if err := c.Create(context.Background(), gamePod()); err != nil {
			t.Fatal(err)
		}
	}

	// Spend the three automatic restarts (autorestart_test.go pins the schedule).
	at(0)
	for _, due := range []time.Duration{90 * time.Second, 240 * time.Second, 510 * time.Second} {
		at(due)
		recreatePod()
	}
	s := at(time.Hour)
	if !v1alpha1.StartGaveUp(&s.Status) {
		t.Fatalf("setup: not given up: phase=%s restarts=%d", s.Status.Phase, s.Status.AutoRestarts)
	}
	if s.Status.LiveMotd != "broken" {
		t.Fatalf("Failed advertises liveMotd %q, want the failed MOTD", s.Status.LiveMotd)
	}

	requestStartRetry(t, c)
	s = at(2 * time.Hour)
	if _, left := s.Annotations[v1alpha1.AnnotationStartRetry]; left {
		t.Fatal("the retry request was left on the server")
	}
	if podPresent(t, c) {
		t.Fatal("the retry kept the failed pod")
	}
	if s.Status.Phase != v1alpha1.PhaseStarting || s.Status.AutoRestarts != 0 || v1alpha1.StartGaveUp(&s.Status) {
		t.Fatalf("after retry: phase=%s restarts=%d gaveUp=%v", s.Status.Phase, s.Status.AutoRestarts, v1alpha1.StartGaveUp(&s.Status))
	}
	if s.Status.StartRequestedAt == nil || !s.Status.StartRequestedAt.Time.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("start not re-anchored at the retry: %v", s.Status.StartRequestedAt)
	}
	recreatePod()

	// The budget is really back: this start times out and is retried on its own.
	if s := at(2*time.Hour + 89*time.Second); s.Status.Phase != v1alpha1.PhaseFailed || v1alpha1.StartGaveUp(&s.Status) {
		t.Fatalf("retried start timing out: phase=%s gaveUp=%v, want Failed with retries left", s.Status.Phase, v1alpha1.StartGaveUp(&s.Status))
	}
	if s := at(2*time.Hour + 90*time.Second); s.Status.AutoRestarts != 1 || podPresent(t, c) {
		t.Fatalf("retried start: restarts=%d pod=%v, want the first automatic restart", s.Status.AutoRestarts, podPresent(t, c))
	}
}

// A request that finds the server anywhere but Failed is stale (the start
// already recovered, or someone stopped it): it is removed and nothing else.
func TestStaleStartRetryIsOnlyRemoved(t *testing.T) {
	srv := runningServer()
	srv.Annotations = map[string]string{v1alpha1.AnnotationStartRetry: "2026-07-01T12:00:00Z"}
	anchor := metav1.NewTime(fixedNow().Add(-10 * time.Second))
	srv.Status = v1alpha1.MinecraftServerStatus{Phase: v1alpha1.PhaseStarting, AutoRestarts: 2, StartRequestedAt: &anchor}
	r, c := newReconciler(t, fakeProber{}, srv, rconSecret(), gamePod())

	reconcile(t, r, "survival")
	s := getServer(t, c, "survival")
	if _, left := s.Annotations[v1alpha1.AnnotationStartRetry]; left {
		t.Fatal("the stale retry request was left on the server")
	}
	if !podPresent(t, c) || s.Status.AutoRestarts != 2 || !s.Status.StartRequestedAt.Equal(&anchor) {
		t.Fatalf("a stale request touched the start: pod=%v restarts=%d anchor=%v",
			podPresent(t, c), s.Status.AutoRestarts, s.Status.StartRequestedAt)
	}
}
