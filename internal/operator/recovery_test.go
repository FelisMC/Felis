package operator_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/operator"
)

// runHost drives a running server whose pod the test restarts by hand, as the kubelet would.
type runHost struct {
	t      *testing.T
	r      *operator.Reconciler
	c      client.Client
	clock  time.Time
	bootID string
}

func newRunHost(t *testing.T, edit func(*v1alpha1.MinecraftServer)) *runHost {
	t.Helper()
	srv := runningServer()
	if edit != nil {
		edit(srv)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "survival-0", Namespace: "minecraft", UID: "pod-1"},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "minecraft"}}},
	}
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Max: 20, Known: true}}, srv, rconSecret(), pod)
	h := &runHost{t: t, r: r, c: c, clock: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), bootID: "boot-1"}
	r.Now = func() metav1.Time { return metav1.NewTime(h.clock) }
	r.BootID = func(context.Context, string) (string, error) { return h.bootID, nil }
	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")
	if s := getServer(t, c, "survival"); s.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("setup: phase %s, want Running", s.Status.Phase)
	}
	return h
}

// exit makes the kubelet restart the game container after it ended with code.
func (h *runHost) exit(reason string, code int32) *v1alpha1.MinecraftServer {
	h.t.Helper()
	var pod corev1.Pod
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-0"}, &pod); err != nil {
		h.t.Fatal(err)
	}
	cs := &pod.Status.ContainerStatuses[0]
	cs.RestartCount++
	cs.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Reason: reason, ExitCode: code, FinishedAt: metav1.NewTime(h.clock),
	}}
	if err := h.c.Status().Update(context.Background(), &pod); err != nil {
		h.t.Fatal(err)
	}
	return h.pass()
}

func (h *runHost) pass() *v1alpha1.MinecraftServer {
	h.t.Helper()
	reconcile(h.t, h.r, "survival")
	return getServer(h.t, h.c, "survival")
}

// start does what the panel's Start does, including the generation bump of the API server.
func (h *runHost) start() {
	h.t.Helper()
	s := getServer(h.t, h.c, "survival")
	s.Spec.DesiredState = v1alpha1.DesiredRunning
	s.Generation++
	if err := h.c.Update(context.Background(), s); err != nil {
		h.t.Fatal(err)
	}
}

func lastEvent(t *testing.T, s *v1alpha1.MinecraftServer) v1alpha1.ServerEvent {
	t.Helper()
	if len(s.Status.Events) == 0 {
		t.Fatal("no lifecycle event recorded")
	}
	return s.Status.Events[len(s.Status.Events)-1]
}

func TestCrashesAreRestartedUpToTheLimitThenTheServerStops(t *testing.T) {
	h := newRunHost(t, nil)
	for n := int32(1); n <= v1alpha1.DefaultMaxCrashRestarts; n++ {
		h.clock = h.clock.Add(time.Minute)
		s := h.exit("Error", 1)
		ev := lastEvent(t, s)
		if s.Spec.DesiredState != v1alpha1.DesiredRunning || s.Status.Crashes != n || s.Status.AutoStop != nil {
			t.Fatalf("crash %d: desired=%s crashes=%d autoStop=%v, want it restarted", n, s.Spec.DesiredState, s.Status.Crashes, s.Status.AutoStop)
		}
		if ev.Type != v1alpha1.EventCrashed || ev.Crashes != n || ev.Limit != 3 || ev.Stopping || ev.ExitCode != 1 || ev.Reason != "Error" {
			t.Fatalf("crash %d event = %+v", n, ev)
		}
	}
	h.clock = h.clock.Add(time.Minute)
	s := h.exit("OOMKilled", 137)
	if s.Spec.DesiredState != v1alpha1.DesiredStopped || s.Status.AutoStop == nil || s.Status.AutoStop.Reason != v1alpha1.StopCrashLimit {
		t.Fatalf("crash 4 of a limit of 3: desired=%s autoStop=%+v, want stopped for the limit", s.Spec.DesiredState, s.Status.AutoStop)
	}
	if ev := lastEvent(t, s); !ev.Stopping || ev.Crashes != 4 {
		t.Fatalf("the crash that hit the limit: %+v", ev)
	}
	if c := s.Status.LastCrash; c == nil || c.Reason != "OOMKilled" || c.ExitCode != 137 {
		t.Fatalf("lastCrash = %+v", c)
	}
	if !v1alpha1.CrashStopped(&s.Status) {
		t.Fatal("a server stopped for crashing must not be woken by a join")
	}
}

func TestACrashWithRestartOffStopsTheServer(t *testing.T) {
	off := false
	h := newRunHost(t, func(s *v1alpha1.MinecraftServer) { s.Spec.Recovery.RestartOnCrash = &off })
	s := h.exit("Error", 1)
	if s.Spec.DesiredState != v1alpha1.DesiredStopped || s.Status.AutoStop == nil || s.Status.AutoStop.Reason != v1alpha1.StopCrashed {
		t.Fatalf("desired=%s autoStop=%+v, want stopped on the first crash", s.Spec.DesiredState, s.Status.AutoStop)
	}
	if ev := lastEvent(t, s); ev.Limit != 0 || !ev.Stopping {
		t.Fatalf("event = %+v", ev)
	}
}

func TestTheOwnersLimitReplacesTheDefault(t *testing.T) {
	h := newRunHost(t, func(s *v1alpha1.MinecraftServer) { s.Spec.Recovery.MaxCrashRestarts = 1 })
	if s := h.exit("Error", 1); s.Spec.DesiredState != v1alpha1.DesiredRunning {
		t.Fatal("the first crash under a limit of 1 must be restarted")
	}
	if s := h.exit("Error", 1); s.Spec.DesiredState != v1alpha1.DesiredStopped {
		t.Fatal("the second crash under a limit of 1 must stop the server")
	}
}

func TestTheCrashCountStartsOverAfterAnHourWithoutOne(t *testing.T) {
	h := newRunHost(t, nil)
	h.exit("Error", 1)
	h.clock = h.clock.Add(50 * time.Minute)
	if s := h.exit("Error", 1); s.Status.Crashes != 2 {
		t.Fatalf("crashes = %d, want 2 within the hour", s.Status.Crashes)
	}
	h.clock = h.clock.Add(61 * time.Minute)
	if s := h.exit("Error", 1); s.Status.Crashes != 1 {
		t.Fatalf("crashes = %d, want the count started over", s.Status.Crashes)
	}
}

func TestStartingTheServerAgainClearsTheCrashStop(t *testing.T) {
	off := false
	h := newRunHost(t, func(s *v1alpha1.MinecraftServer) { s.Spec.Recovery.RestartOnCrash = &off })
	h.exit("Error", 1)
	h.pass()
	h.start()
	s := h.pass()
	if s.Spec.DesiredState != v1alpha1.DesiredRunning || s.Status.AutoStop != nil || s.Status.Crashes != 0 {
		t.Fatalf("after a start: desired=%s autoStop=%+v crashes=%d", s.Spec.DesiredState, s.Status.AutoStop, s.Status.Crashes)
	}
}

func TestALostStopPatchIsAppliedOnTheNextPass(t *testing.T) {
	h := newRunHost(t, nil)
	s := getServer(t, h.c, "survival")
	s.Status.AutoStop = &v1alpha1.AutoStop{Reason: v1alpha1.StopCrashLimit, At: metav1.NewTime(h.clock), Generation: s.Generation + 1}
	if err := h.c.Status().Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if s := h.pass(); s.Spec.DesiredState != v1alpha1.DesiredStopped {
		t.Fatalf("desired = %s, want the recorded stop applied", s.Spec.DesiredState)
	}
}

func TestRestartsFromBeforeTheUpgradeAreNotCrashes(t *testing.T) {
	h := newRunHost(t, nil)
	s := getServer(t, h.c, "survival")
	s.Status.Run = v1alpha1.RunStatus{}
	if err := h.c.Status().Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-0"}, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses[0].RestartCount = 7
	if err := h.c.Status().Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	if s := h.pass(); s.Status.Crashes != 0 || s.Status.Run.Restarts != 7 {
		t.Fatalf("crashes=%d run=%+v, want only a baseline", s.Status.Crashes, s.Status.Run)
	}
}

func TestRestartsOfAReplacementPodAreCrashes(t *testing.T) {
	h := newRunHost(t, nil)
	var pod corev1.Pod
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-0"}, &pod); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	fresh := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "survival-0", Namespace: "minecraft", UID: "pod-2"},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
	}
	if err := h.c.Create(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "minecraft", RestartCount: 1}}
	if err := h.c.Status().Update(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	if s := h.pass(); s.Status.Crashes != 1 {
		t.Fatalf("crashes = %d, want the new pod's restart counted", s.Status.Crashes)
	}
}

func TestAHostRestartResumesTheServerByDefault(t *testing.T) {
	h := newRunHost(t, nil)
	h.bootID = "boot-2"
	s := h.exit("Unknown", 255)
	if s.Spec.DesiredState != v1alpha1.DesiredRunning || s.Status.Crashes != 0 {
		t.Fatalf("desired=%s crashes=%d, want it resumed and no crash counted", s.Spec.DesiredState, s.Status.Crashes)
	}
	if ev := lastEvent(t, s); ev.Type != v1alpha1.EventHostRestarted || ev.Stopping {
		t.Fatalf("event = %+v", ev)
	}
}

func TestAHostRestartStopsAServerSetNotToResume(t *testing.T) {
	off := false
	h := newRunHost(t, func(s *v1alpha1.MinecraftServer) { s.Spec.Recovery.ResumeAfterHostRestart = &off })
	h.bootID = "boot-2"
	s := h.exit("Unknown", 255)
	if s.Spec.DesiredState != v1alpha1.DesiredStopped || s.Status.AutoStop == nil || s.Status.AutoStop.Reason != v1alpha1.StopHostRestart {
		t.Fatalf("desired=%s autoStop=%+v, want stopped after the host restart", s.Spec.DesiredState, s.Status.AutoStop)
	}
	if v1alpha1.CrashStopped(&s.Status) {
		t.Fatal("a server held back after a host restart may still be woken by a join")
	}
	if ev := lastEvent(t, s); ev.Type != v1alpha1.EventHostRestarted || !ev.Stopping {
		t.Fatalf("event = %+v", ev)
	}
}

func TestSystemServersAlwaysRestartAndResume(t *testing.T) {
	off := false
	h := newRunHost(t, func(s *v1alpha1.MinecraftServer) {
		s.Labels = map[string]string{v1alpha1.LabelSystemRole: "lobby"}
		s.Spec.Recovery = v1alpha1.RecoverySpec{RestartOnCrash: &off, ResumeAfterHostRestart: &off}
	})
	if s := h.exit("Error", 1); s.Spec.DesiredState != v1alpha1.DesiredRunning {
		t.Fatal("a system server stopped after a crash")
	}
	h.bootID = "boot-2"
	if s := h.exit("Unknown", 255); s.Spec.DesiredState != v1alpha1.DesiredRunning {
		t.Fatal("a system server stayed down after a host restart")
	}
}

func TestLifecycleEventsFollowStartsAndStops(t *testing.T) {
	h := newRunHost(t, nil)
	s := getServer(t, h.c, "survival")
	if ev := lastEvent(t, s); ev.Type != v1alpha1.EventStarted || ev.Seq != 1 {
		t.Fatalf("first event = %+v, want Started #1", ev)
	}
	s.Spec.DesiredState = v1alpha1.DesiredStopped
	if err := h.c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	h.pass()
	markPodTerminated(t, h.c, "survival")
	s = h.pass()
	if ev := lastEvent(t, s); ev.Type != v1alpha1.EventStopped || ev.Reason != "" || ev.Seq != 2 {
		t.Fatalf("after a stop: %+v, want a plain Stopped #2", ev)
	}
	if n := len(s.Status.Events); n != 2 {
		t.Fatalf("%d events, want 2", n)
	}
}

func TestEventsKeepTheLatestTwenty(t *testing.T) {
	h := newRunHost(t, func(s *v1alpha1.MinecraftServer) { s.Spec.Recovery.MaxCrashRestarts = v1alpha1.MaxCrashRestartsLimit })
	// Started, then 21 crashes, the last of which stops the server.
	var s *v1alpha1.MinecraftServer
	for range 21 {
		h.clock = h.clock.Add(time.Second)
		s = h.exit("Error", 1)
	}
	if n := len(s.Status.Events); n != v1alpha1.MaxServerEvents {
		t.Fatalf("%d events kept, want %d", n, v1alpha1.MaxServerEvents)
	}
	if first, last := s.Status.Events[0].Seq, lastEvent(t, s); first != 3 || last.Seq != 22 || !last.Stopping {
		t.Fatalf("kept seq %d..%d (stopping=%v), want 3..22 ending in the stop", first, last.Seq, last.Stopping)
	}
}
