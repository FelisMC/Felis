package operator_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/hibernate"
	"felis.lolicon.best/internal/operator"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// hibernateRig is a running software server whose pod was started ready to hibernate, with a kubelet that grants resizes unless told not to.
type hibernateRig struct {
	t        *testing.T
	r        *operator.Reconciler
	c        client.Client
	clock    time.Time
	commands *[]string
	resizes  []string
	refuse   bool
}

func newHibernateRig(t *testing.T, prober fakeProber, podReady func(*corev1.Pod)) *hibernateRig {
	t.Helper()
	commands := []string{}
	prober.commands = &commands
	server := runningServer()
	server.Spec.Software = &v1alpha1.SoftwareSpec{Type: "fabric", MinecraftVersion: "1.21.4", Java: 21}
	server.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: 60}
	r, c := newReconciler(t, prober, server, rconSecret())
	rig := &hibernateRig{t: t, r: r, c: c, clock: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC), commands: &commands}
	r.Now = func() metav1.Time { return metav1.NewTime(rig.clock) }
	r.FelisImage = "registry.felis.svc:5000/felis/felis:test"
	r.HibernateNamespace = "felis"
	r.Resize = func(ctx context.Context, pod *corev1.Pod, res corev1.ResourceRequirements) error {
		limit := res.Limits[corev1.ResourceMemory]
		rig.resizes = append(rig.resizes, limit.String())
		var live corev1.Pod
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &live); err != nil {
			return err
		}
		if rig.refuse {
			live.Status.Conditions = append(live.Status.Conditions, corev1.PodCondition{Type: "PodResizePending", Status: corev1.ConditionTrue, Reason: "Infeasible", Message: "Node didn't have enough capacity: memory"})
		} else {
			live.Status.ContainerStatuses[0].Resources = &corev1.ResourceRequirements{Limits: res.Limits, Requests: res.Requests}
		}
		return c.Status().Update(ctx, &live)
	}

	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "survival-0", Namespace: "minecraft", UID: "pod-1"},
		Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{
			Name: "minecraft",
			Env:  []corev1.EnvVar{{Name: "FELIS_HIBERNATE", Value: "1"}},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourceCPU: resource.MustParse("500m")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
			},
		}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "minecraft", ContainerID: "containerd://c0ffee", Ready: true,
			State:     corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			Resources: &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}},
		}}},
	}
	if podReady != nil {
		podReady(pod)
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	return rig
}

func (g *hibernateRig) idle() {
	g.t.Helper()
	reconcile(g.t, g.r, "survival")
	g.clock = g.clock.Add(operator.ArrivalWindow)
	reconcile(g.t, g.r, "survival")
	g.clock = g.clock.Add(61 * time.Second)
	reconcile(g.t, g.r, "survival")
}

func (g *hibernateRig) jobs(action string) []batchv1.Job {
	g.t.Helper()
	var list batchv1.JobList
	if err := g.c.List(context.Background(), &list, client.InNamespace("felis"), client.MatchingLabels{operator.LabelHibernateJob: action}); err != nil {
		g.t.Fatal(err)
	}
	return list.Items
}

// finish marks the node Job done with the verdict the hibernate command writes as its termination message.
func (g *hibernateRig) finish(job batchv1.Job, result hibernate.Result) {
	g.t.Helper()
	if result.OK {
		job.Status.Succeeded = 1
	} else {
		job.Status.Failed = 1
	}
	if err := g.c.Status().Update(context.Background(), &job); err != nil {
		g.t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x", Namespace: "felis", Labels: map[string]string{"job-name": job.Name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "hibernate", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: string(raw)}},
		}}},
	}
	if err := g.c.Create(context.Background(), pod); err != nil {
		g.t.Fatal(err)
	}
}

func (g *hibernateRig) freeze() {
	g.t.Helper()
	g.idle()
	jobs := g.jobs("freeze")
	if len(jobs) != 1 {
		g.t.Fatalf("freeze jobs = %d, want 1", len(jobs))
	}
	g.finish(jobs[0], hibernate.Result{OK: true, SwappedBytes: 3 << 30})
	reconcile(g.t, g.r, "survival")
}

func (g *hibernateRig) setDesired(state v1alpha1.DesiredState) {
	g.t.Helper()
	server := getServer(g.t, g.c, "survival")
	server.Spec.DesiredState = state
	if err := g.c.Update(context.Background(), server); err != nil {
		g.t.Fatal(err)
	}
}

func TestIdleServerFreezesOnItsNodeAfterSavingTheWorld(t *testing.T) {
	var saves []string
	g := newHibernateRig(t, fakeProber{players: operator.PlayerCount{Known: true, Max: 20}, saves: &saves}, nil)
	g.idle()

	server := getServer(t, g.c, "survival")
	if server.Spec.DesiredState != v1alpha1.DesiredHibernated || server.Status.Phase != v1alpha1.PhaseHibernating {
		t.Fatalf("desired %s phase %s, want Hibernated and Hibernating", server.Spec.DesiredState, server.Status.Phase)
	}
	if strings.Join(*g.commands, ",") != "save-off" || len(saves) != 1 {
		t.Fatalf("commands %v saves %d; want saving off and one flush before the freeze", *g.commands, len(saves))
	}
	jobs := g.jobs("freeze")
	if len(jobs) != 1 {
		t.Fatalf("freeze jobs = %d, want 1", len(jobs))
	}
	spec := jobs[0].Spec.Template.Spec
	if spec.NodeName != "node-a" || !spec.HostPID || !strings.Contains(strings.Join(spec.Containers[0].Command, " "), "hibernate freeze --container c0ffee") {
		t.Fatalf("freeze job runs %v on %q hostPID=%v; want the game container frozen on its own node", spec.Containers[0].Command, spec.NodeName, spec.HostPID)
	}
}

func TestFrozenServerGivesItsMemoryBackAndWakesWhereItStopped(t *testing.T) {
	g := newHibernateRig(t, fakeProber{players: operator.PlayerCount{Known: true, Max: 20}}, nil)
	g.freeze()

	server := getServer(t, g.c, "survival")
	if server.Status.Phase != v1alpha1.PhaseHibernated || server.Status.Hibernation.SwappedBytes != 3<<30 {
		t.Fatalf("phase %s hibernation %+v, want Hibernated with 3 GiB in swap", server.Status.Phase, server.Status.Hibernation)
	}
	if strings.Join(g.resizes, ",") != "128Mi" {
		t.Fatalf("resizes %v, want the reservation shrunk to 128Mi", g.resizes)
	}

	g.setDesired(v1alpha1.DesiredRunning)
	reconcile(t, g.r, "survival")
	reconcile(t, g.r, "survival")
	if strings.Join(g.resizes, ",") != "128Mi,4Gi" {
		t.Fatalf("resizes %v, want the 4Gi reservation back before thawing", g.resizes)
	}
	thaws := g.jobs("thaw")
	if len(thaws) != 1 || !strings.Contains(strings.Join(thaws[0].Spec.Template.Spec.Containers[0].Command, " "), "--expect 3221225472") {
		t.Fatalf("thaw jobs %+v, want one that waits for the 3 GiB to come back", thaws)
	}
	g.finish(thaws[0], hibernate.Result{OK: true})
	*g.commands = nil
	reconcile(t, g.r, "survival")

	server = getServer(t, g.c, "survival")
	if server.Status.Hibernation != nil || server.Status.Phase != v1alpha1.PhaseStarting || strings.Join(*g.commands, ",") != "save-on" {
		t.Fatalf("after the thaw: hibernation %+v phase %s commands %v; want it resumed with saving back on", server.Status.Hibernation, server.Status.Phase, *g.commands)
	}
	var pod corev1.Pod
	if err := g.c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-0"}, &pod); err != nil {
		t.Fatalf("the woken pod is gone: %v", err)
	}
}

func TestWakeWaitsWhileTheNodeCannotGiveTheMemoryBack(t *testing.T) {
	g := newHibernateRig(t, fakeProber{players: operator.PlayerCount{Known: true, Max: 20}}, nil)
	g.freeze()
	g.refuse = true
	g.setDesired(v1alpha1.DesiredRunning)
	reconcile(t, g.r, "survival")
	reconcile(t, g.r, "survival")

	server := getServer(t, g.c, "survival")
	if len(g.jobs("thaw")) != 0 || server.Status.Hibernation == nil || server.Status.Hibernation.Stage != v1alpha1.HibernationGrowing {
		t.Fatalf("thaws %d hibernation %+v, want it frozen until the node has room", len(g.jobs("thaw")), server.Status.Hibernation)
	}
	if !strings.Contains(server.Status.Hibernation.Message, "memory") {
		t.Fatalf("message %q, want the reason the wake waits", server.Status.Hibernation.Message)
	}
}

func TestFreezeTheNodeRefusesStopsTheServerInstead(t *testing.T) {
	g := newHibernateRig(t, fakeProber{players: operator.PlayerCount{Known: true, Max: 20}}, nil)
	g.idle()
	g.finish(g.jobs("freeze")[0], hibernate.Result{Code: hibernate.CodeInsufficientSwap, Message: "needs 4160 MiB of swap, 900 MiB free"})
	*g.commands = nil
	reconcile(t, g.r, "survival")

	server := getServer(t, g.c, "survival")
	if server.Spec.DesiredState != v1alpha1.DesiredStopped || server.Status.Hibernation != nil || strings.Join(*g.commands, ",") != "save-on" {
		t.Fatalf("desired %s hibernation %+v commands %v; want saving back on and an ordinary stop", server.Spec.DesiredState, server.Status.Hibernation, *g.commands)
	}
}

func TestAPodStartedBeforeHibernationStopsWhenIdle(t *testing.T) {
	g := newHibernateRig(t, fakeProber{players: operator.PlayerCount{Known: true, Max: 20}}, func(pod *corev1.Pod) {
		pod.Spec.Containers[0].Env = nil
	})
	g.idle()
	if server := getServer(t, g.c, "survival"); server.Spec.DesiredState != v1alpha1.DesiredStopped || len(g.jobs("freeze")) != 0 {
		t.Fatalf("desired %s with %d freeze jobs; a pod whose watchdog is still on must stop, not freeze", server.Spec.DesiredState, len(g.jobs("freeze")))
	}
}

// killedFromTheNode checks that the frozen pod was deleted and a node Job sent to end its process, then finishes that Job.
func (g *hibernateRig) killedFromTheNode() {
	g.t.Helper()
	var pod corev1.Pod
	if err := g.c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-0"}, &pod); err == nil {
		g.t.Fatal("the frozen pod was not deleted")
	}
	kills := g.jobs("kill")
	if len(kills) != 1 || !strings.Contains(strings.Join(kills[0].Spec.Template.Spec.Containers[0].Command, " "), "hibernate kill --container c0ffee") {
		g.t.Fatalf("kill jobs %+v; a frozen process cannot act on SIGTERM and its preStop hook freezes too", kills)
	}
	if server := getServer(g.t, g.c, "survival"); server.Status.Hibernation == nil {
		g.t.Fatal("hibernation cleared before the frozen process was gone")
	}
	g.finish(kills[0], hibernate.Result{OK: true})
	reconcile(g.t, g.r, "survival")
}

func TestStoppingAFrozenServerEndsItFromTheNode(t *testing.T) {
	g := newHibernateRig(t, fakeProber{players: operator.PlayerCount{Known: true, Max: 20}}, nil)
	g.freeze()
	g.setDesired(v1alpha1.DesiredStopped)
	reconcile(t, g.r, "survival")
	g.killedFromTheNode()

	if server := getServer(t, g.c, "survival"); server.Status.Hibernation != nil || server.Status.Phase != v1alpha1.PhaseStopping {
		t.Fatalf("phase %s hibernation %+v after the kill, want Stopping with no hibernation", server.Status.Phase, server.Status.Hibernation)
	}
}

func TestAWakeThatCannotResumeStartsAfreshOnceTheProcessIsGone(t *testing.T) {
	g := newHibernateRig(t, fakeProber{players: operator.PlayerCount{Known: true, Max: 20}}, nil)
	g.freeze()
	g.setDesired(v1alpha1.DesiredRunning)
	reconcile(t, g.r, "survival")
	reconcile(t, g.r, "survival")
	g.finish(g.jobs("thaw")[0], hibernate.Result{Code: hibernate.CodeFailed, Message: "read memory of process 4242 back: no such process"})
	reconcile(t, g.r, "survival")
	g.killedFromTheNode()

	server := getServer(t, g.c, "survival")
	if server.Status.Hibernation != nil || server.Status.Phase != v1alpha1.PhaseStarting || server.Spec.DesiredState != v1alpha1.DesiredRunning {
		t.Fatalf("phase %s desired %s hibernation %+v, want a fresh start", server.Status.Phase, server.Spec.DesiredState, server.Status.Hibernation)
	}
}

func TestAServerWithoutAWatchdogIsRestartedWhenItHangs(t *testing.T) {
	g := newHibernateRig(t, fakeProber{players: operator.PlayerCount{Stalled: true}}, nil)
	reconcile(t, g.r, "survival")
	g.clock = g.clock.Add(30 * time.Second)
	reconcile(t, g.r, "survival")
	if server := getServer(t, g.c, "survival"); server.Status.Crashes != 0 {
		t.Fatalf("crashes = %d after 30s without an answer, want none yet", server.Status.Crashes)
	}
	g.clock = g.clock.Add(61 * time.Second)
	reconcile(t, g.r, "survival")

	server := getServer(t, g.c, "survival")
	if server.Status.Crashes != 1 || server.Status.LastCrash == nil || server.Status.LastCrash.Reason != "Unresponsive" {
		t.Fatalf("crashes %d last %+v, want one unresponsive crash", server.Status.Crashes, server.Status.LastCrash)
	}
	var pod corev1.Pod
	if err := g.c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival-0"}, &pod); err == nil {
		t.Fatal("the hung pod is still there, want it restarted")
	}
}
