package operator_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/operator"
)

// drain returns the Events recorded since the last drain.
func drain(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func expectEvents(t *testing.T, rec *record.FakeRecorder, step string, want ...string) {
	t.Helper()
	if got := drain(rec); !slices.Equal(got, want) {
		t.Fatalf("%s: events = %q, want %q", step, got, want)
	}
}

// Each phase change lands one Event, and a pass that changes nothing lands none.
func TestReconcilerRecordsThePhaseTimeline(t *testing.T) {
	down := false
	r, c := newReconciler(t, switchProber{down: &down}, runningServer())
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec

	reconcile(t, r, "survival")
	expectEvents(t, rec, "first pass",
		"Normal RconSecretCreated created the RCON password Secret survival-rcon",
		"Normal PodNotReady New → Starting: waiting for pod TCP readiness")
	reconcile(t, r, "survival")
	expectEvents(t, rec, "still starting")

	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")
	expectEvents(t, rec, "ready", "Normal RconReached Starting → Running: server is accepting RCON")
	reconcile(t, r, "survival")
	expectEvents(t, rec, "steady")

	down = true
	for range 3 {
		reconcile(t, r, "survival")
	}
	expectEvents(t, rec, "degraded", "Warning RconNotReachable Running → Starting: i/o timeout")
}

// A timed-out start and the pod recreation that retries it are Warnings.
func TestTimeoutAndAutoRestartAreWarnings(t *testing.T) {
	srv := runningServer()
	srv.Spec.Startup.TimeoutSeconds = 30
	r, _ := newReconciler(t, fakeProber{}, srv, rconSecret(), gamePod())
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	reconcile(t, r, "survival")
	drain(rec)
	clock = base.Add(30 * time.Second)
	reconcile(t, r, "survival")
	expectEvents(t, rec, "timeout",
		"Warning StartupTimeout Starting → Failed: pod did not become ready within startup timeout")
	clock = base.Add(90 * time.Second)
	reconcile(t, r, "survival")
	expectEvents(t, rec, "retry",
		"Warning AutoRestart Failed → Starting: start timed out; recreated the pod (attempt 1 of 3)")
}

// Idle auto-stop says why it stopped the server.
func TestIdleStopRecordsAnEvent(t *testing.T) {
	srv := runningServer()
	srv.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: 60}
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}, srv, rconSecret())
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")
	clock = base.Add(operator.ArrivalWindow)
	reconcile(t, r, "survival")
	drain(rec)
	clock = base.Add(operator.ArrivalWindow + 61*time.Second)
	reconcile(t, r, "survival")
	expectEvents(t, rec, "idle", "Normal IdleStop no players online for 60s; set desiredState to Stopped")
}

// A pass that fails records nothing, even though it changed the phase in memory.
func TestFailedPassRecordsNoEvent(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret())
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec
	r.Client = failStatus{c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "minecraft", Name: "survival"}}
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("want the status write error")
	}
	expectEvents(t, rec, "failed write")
}

// failStatus refuses every status write.
type failStatus struct{ client.Client }

func (f failStatus) Status() client.SubResourceWriter { return failingWriter{f.Client.Status()} }

type failingWriter struct{ client.SubResourceWriter }

func (failingWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return errors.New("status write refused")
}
