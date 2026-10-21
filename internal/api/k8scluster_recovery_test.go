package api

import (
	"context"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func recoveryCluster(t *testing.T, objs ...client.Object) (*K8sCluster, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return NewK8sCluster(c, "minecraft"), c
}

func plainServer(name string) *v1alpha1.MinecraftServer {
	return &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "minecraft"},
		Spec: v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredStopped}}
}

func TestRecoverySettingsDefaultOnAndRoundTrip(t *testing.T) {
	k, c := recoveryCluster(t, plainServer("survival"))
	ctx := context.Background()
	info, err := k.GetServer(ctx, "survival")
	if err != nil {
		t.Fatal(err)
	}
	if !info.ResumeAfterHostRestart || !info.RestartOnCrash || info.MaxCrashRestarts != 3 || info.Notify != (ServerNotify{Problems: true}) {
		t.Fatalf("a server with nothing set: resume %v restart %v max %d notify %+v; want both on, 3 and problem mail only",
			info.ResumeAfterHostRestart, info.RestartOnCrash, info.MaxCrashRestarts, info.Notify)
	}

	off, on := false, true
	if err := k.PatchServerSpec(ctx, "survival", ServerSpecPatch{
		ResumeAfterHostRestart: &off, RestartOnCrash: &off, NotifyProblems: &off, NotifyStarted: &on,
	}); err != nil {
		t.Fatal(err)
	}
	var ms v1alpha1.MinecraftServer
	if err := c.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &ms); err != nil {
		t.Fatal(err)
	}
	if ms.Spec.Recovery.CrashRestartLimit() != 0 || ms.Spec.Recovery.ResumesAfterHostRestart() || ms.Spec.Notify.ProblemsOn() || !ms.Spec.Notify.Started {
		t.Fatalf("stored recovery %+v notify %+v", ms.Spec.Recovery, ms.Spec.Notify)
	}
	info, _ = k.GetServer(ctx, "survival")
	if info.RestartOnCrash || info.MaxCrashRestarts != 3 {
		t.Fatalf("restart %v max %d; want off with the limit kept for turning it back on", info.RestartOnCrash, info.MaxCrashRestarts)
	}

	seven := int32(7)
	if err := k.PatchServerSpec(ctx, "survival", ServerSpecPatch{RestartOnCrash: &on, MaxCrashRestarts: &seven}); err != nil {
		t.Fatal(err)
	}
	if info, _ = k.GetServer(ctx, "survival"); !info.RestartOnCrash || info.MaxCrashRestarts != 7 || info.ResumeAfterHostRestart {
		t.Fatalf("restart %v max %d resume %v", info.RestartOnCrash, info.MaxCrashRestarts, info.ResumeAfterHostRestart)
	}
}

func TestCrashStateReachesTheView(t *testing.T) {
	s := plainServer("survival")
	s.Status = v1alpha1.MinecraftServerStatus{
		Phase: v1alpha1.PhaseStopped, Crashes: 4,
		LastCrash: &v1alpha1.CrashRecord{At: metav1.NewTime(time.Now().Add(-10 * time.Minute)), Reason: "Error", ExitCode: 1},
		AutoStop:  &v1alpha1.AutoStop{Reason: v1alpha1.StopCrashLimit, At: metav1.Now(), Generation: 2},
		Events:    []v1alpha1.ServerEvent{{Seq: 1, Type: v1alpha1.EventCrashed, Crashes: 4, Limit: 3, Stopping: true}},
	}
	old := plainServer("old")
	old.Status.Crashes = 2
	old.Status.LastCrash = &v1alpha1.CrashRecord{At: metav1.NewTime(time.Now().Add(-2 * time.Hour))}
	k, _ := recoveryCluster(t, s, old)
	ctx := context.Background()
	info, err := k.GetServer(ctx, "survival")
	if err != nil {
		t.Fatal(err)
	}
	if info.RecentCrashes != 4 || !info.CrashStopped || info.AutoStop == nil || info.AutoStop.Reason != v1alpha1.StopCrashLimit ||
		info.LastCrash == nil || info.LastCrash.ExitCode != 1 || len(info.Events) != 1 || !info.Events[0].Stopping {
		t.Fatalf("view = %+v", info)
	}
	if info, _ := k.GetServer(ctx, "old"); info.RecentCrashes != 0 {
		t.Fatalf("crashes from two hours ago show as recent: %d", info.RecentCrashes)
	}
	list, err := k.ListServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if len(s.Events) != 0 {
			t.Fatalf("%s carries its events in the fleet list", s.Name)
		}
	}
}

func TestMailedEventsAreRememberedOnTheServer(t *testing.T) {
	busy := plainServer("survival")
	busy.Spec.DisplayName = "生存服"
	busy.Status.Events = []v1alpha1.ServerEvent{{Seq: 1, Type: v1alpha1.EventStarted}, {Seq: 2, Type: v1alpha1.EventStopped}}
	k, _ := recoveryCluster(t, busy, plainServer("quiet"))
	ctx := context.Background()
	feeds, err := k.ServerEventFeeds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 || feeds[0].Name != "survival" || feeds[0].DisplayName != "生存服" || feeds[0].Mailed != 0 || len(feeds[0].Events) != 2 {
		t.Fatalf("feeds = %+v", feeds)
	}
	if err := k.MarkEventsMailed(ctx, "survival", 2); err != nil {
		t.Fatal(err)
	}
	if feeds, _ = k.ServerEventFeeds(ctx); feeds[0].Mailed != 2 {
		t.Fatalf("mailed = %d after marking 2", feeds[0].Mailed)
	}
	if err := k.MarkEventsMailed(ctx, "gone", 1); err != ErrNotFound {
		t.Fatalf("marking a deleted server: %v, want ErrNotFound", err)
	}
}
