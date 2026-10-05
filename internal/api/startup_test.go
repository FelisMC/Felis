package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestStartupDiagnostics(t *testing.T) {
	started := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	ms := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft"}, Status: v1alpha1.MinecraftServerStatus{Phase: v1alpha1.PhaseStarting, StartRequestedAt: &started}}
	cases := []struct {
		name, stage, reason string
		status              corev1.PodStatus
		logs                bool
	}{
		{name: "scheduling memory", stage: "scheduling", reason: "Unschedulable", status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "Insufficient memory"}}}},
		{name: "image failure", stage: "failed", reason: "ImagePullBackOff", status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: serverLogContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "image unavailable"}}}}}},
		{name: "process crash", stage: "failed", reason: "OOMKilled", status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: serverLogContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}}}}},
		{name: "evicted", stage: "failed", reason: "Evicted", status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "node memory pressure"}},
		{name: "init failure", stage: "failed", reason: "CrashLoopBackOff", status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: "prepare", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "preparation failed"}}}}, ContainerStatuses: []corev1.ContainerStatus{{Name: serverLogContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}}},
		{name: "node lost", stage: "failed", reason: "NodeNotReady", status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionUnknown, Reason: "NodeNotReady", Message: "node heartbeat lost"}}}},
		{name: "booting", stage: "booting", logs: true, status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: serverLogContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}},
	}
	scheme := runtime.NewScheme()
	v1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "survival-0", Namespace: "minecraft", Labels: map[string]string{v1alpha1.LabelServer: "survival", v1alpha1.LabelComponent: gamePodComponent}}, Status: tc.status}
			worker := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "file-worker", Namespace: "minecraft", CreationTimestamp: started, Labels: map[string]string{v1alpha1.LabelServer: "survival", v1alpha1.LabelComponent: "file-op"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ms, pod, worker).Build()
			info, err := NewK8sCluster(c, "minecraft").GetServer(context.Background(), "survival")
			if err != nil {
				t.Fatal(err)
			}
			s := info.Startup
			if s == nil || s.Stage != tc.stage || s.Reason != tc.reason || s.LogsAvailable != tc.logs || s.StartedAt != started.UTC().Format(time.RFC3339) {
				t.Fatalf("diagnostics=%+v", s)
			}
			if tc.reason == "OOMKilled" && !strings.Contains(s.Message, "137") {
				t.Fatal("missing exit code", s.Message)
			}
			if tc.stage == "failed" {
				msRunning := ms.DeepCopy()
				msRunning.Status.Phase = v1alpha1.PhaseRunning
				cRunning := fake.NewClientBuilder().WithScheme(scheme).WithObjects(msRunning, pod).Build()
				actual, err := NewK8sCluster(cRunning, "minecraft").GetServer(context.Background(), "survival")
				if err != nil || actual.Phase != "Failed" || actual.Ready || actual.Startup == nil {
					t.Fatal("stale operator Running concealed runtime failure", actual, err)
				}
			}
			if publicServerInfo(info).Startup != nil {
				t.Fatal("public status leaked diagnostics")
			}
		})
	}
	running := ms.DeepCopy()
	running.Status.Phase = v1alpha1.PhaseRunning
	if s := startupStatus(running, nil); s.Stage != "failed" || s.Reason != "GamePodMissing" {
		t.Fatal("missing game runtime concealed", s)
	}
	s := startupStatus(ms, nil)
	if s.Stage != "creating" || s.LogsAvailable {
		t.Fatalf("missing pod=%+v", s)
	}
	deleted := corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &started}, Status: cases[0].status}
	if s := startupStatus(ms, []corev1.Pod{deleted}); s.Stage != "creating" {
		t.Fatalf("terminating pod reused=%+v", s)
	}
}
