package operator

import (
	"context"
	"fmt"
	"os"
	"strings"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type BootIDFunc func(ctx context.Context, node string) (string, error)

func NodeBootID(nodes client.Reader) BootIDFunc {
	return func(ctx context.Context, node string) (string, error) {
		var n corev1.Node
		if err := nodes.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
			return "", err
		}
		return n.Status.NodeInfo.BootID, nil
	}
}

// LocalBootID serves the operator host's boot ID, for single-node installs.
func LocalBootID() (BootIDFunc, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(b))
	return func(context.Context, string) (string, error) { return id, nil }, nil
}

func (r *Reconciler) gamePod(ctx context.Context, server *v1alpha1.MinecraftServer) (*corev1.Pod, error) {
	var pod corev1.Pod
	err := r.podReader().Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: server.Name + "-0"}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pod, nil
}

// observeRun reports stopping when a crash or host restart stops the server.
func (r *Reconciler) observeRun(ctx context.Context, server *v1alpha1.MinecraftServer, pod *corev1.Pod) (stopping bool, err error) {
	if pod == nil || pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil {
		return false, nil
	}
	var game *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == containerName {
			game = &pod.Status.ContainerStatuses[i]
		}
	}
	prev := server.Status.Run
	run := v1alpha1.RunStatus{PodUID: string(pod.UID), Node: pod.Spec.NodeName}
	if game != nil {
		run.Restarts = game.RestartCount
	}
	var exits int32
	switch {
	case prev.PodUID == run.PodUID:
		exits = run.Restarts - prev.Restarts
	case prev.PodUID != "":
		exits = run.Restarts
	}
	// Host restarts always change the container, so unchanged pods skip the boot ID read.
	if run.PodUID == prev.PodUID && run.Restarts == prev.Restarts && run.Node == prev.Node && prev.BootID != "" {
		return false, nil
	}
	if r.BootID != nil {
		id, err := r.BootID(ctx, run.Node)
		if err != nil {
			log.FromContext(ctx).Info("could not read the node's boot ID", "node", run.Node, "error", err.Error())
		}
		run.BootID = id
	}
	if run.BootID == "" && run.Node == prev.Node {
		run.BootID = prev.BootID
	}
	if run == prev {
		return false, nil
	}
	server.Status.Run = run
	system := server.Labels[v1alpha1.LabelSystemRole] != ""

	if prev.BootID != "" && prev.Node == run.Node && run.BootID != prev.BootID {
		resume := system || server.Spec.Recovery.ResumesAfterHostRestart()
		r.addEvent(server, v1alpha1.ServerEvent{Type: v1alpha1.EventHostRestarted, Stopping: !resume})
		if !resume {
			r.event(server, corev1.EventTypeWarning, v1alpha1.EventHostRestarted, "the host restarted; the server is set not to resume, stopping it")
			return true, r.stopFor(ctx, server, v1alpha1.StopHostRestart)
		}
		r.event(server, corev1.EventTypeNormal, v1alpha1.EventHostRestarted, "the host restarted; resuming the server")
		return false, r.patchStatus(ctx, server)
	}
	if exits <= 0 {
		return false, nil
	}

	crash := v1alpha1.CrashRecord{At: r.now()}
	if game != nil && game.LastTerminationState.Terminated != nil {
		t := game.LastTerminationState.Terminated
		crash.Reason, crash.ExitCode = t.Reason, t.ExitCode
		if !t.FinishedAt.IsZero() {
			crash.At = t.FinishedAt
		}
	}
	if last := server.Status.LastCrash; last == nil || crash.At.Sub(last.At.Time) > v1alpha1.CrashWindow {
		server.Status.Crashes = 0
	}
	server.Status.Crashes += exits
	server.Status.LastCrash = &crash
	limit := server.Spec.Recovery.CrashRestartLimit()
	if system {
		limit = 0
	}
	stop := !system && server.Status.Crashes > limit
	r.addEvent(server, v1alpha1.ServerEvent{
		Type: v1alpha1.EventCrashed, Reason: crash.Reason, ExitCode: crash.ExitCode,
		Crashes: server.Status.Crashes, Limit: limit, Stopping: stop,
	})
	msg := fmt.Sprintf("the server process exited (%s, exit code %d); crash %d in a row", crash.Reason, crash.ExitCode, server.Status.Crashes)
	if !stop {
		r.event(server, corev1.EventTypeWarning, v1alpha1.EventCrashed, msg+", restarted")
		return false, r.patchStatus(ctx, server)
	}
	reason := v1alpha1.StopCrashLimit
	if limit == 0 {
		reason = v1alpha1.StopCrashed
	}
	r.event(server, corev1.EventTypeWarning, v1alpha1.EventCrashed, msg+", stopping the server")
	return true, r.stopFor(ctx, server, reason)
}

// stopFor records AutoStop first so a lost desiredState patch is retried.
func (r *Reconciler) stopFor(ctx context.Context, server *v1alpha1.MinecraftServer, reason string) error {
	server.Status.AutoStop = &v1alpha1.AutoStop{Reason: reason, At: r.now(), Generation: server.Generation + 1}
	if err := r.patchStatus(ctx, server); err != nil {
		return err
	}
	return r.applyAutoStop(ctx, server)
}

func (r *Reconciler) applyAutoStop(ctx context.Context, server *v1alpha1.MinecraftServer) error {
	patch := client.MergeFrom(server.DeepCopy())
	server.Spec.DesiredState = v1alpha1.DesiredStopped
	return r.Patch(ctx, server, patch)
}

func (r *Reconciler) addEvent(server *v1alpha1.MinecraftServer, ev v1alpha1.ServerEvent) {
	ev.At = r.now()
	ev.Seq = 1
	if n := len(server.Status.Events); n > 0 {
		ev.Seq = server.Status.Events[n-1].Seq + 1
	}
	events := append(server.Status.Events, ev)
	if extra := len(events) - v1alpha1.MaxServerEvents; extra > 0 {
		events = append([]v1alpha1.ServerEvent(nil), events[extra:]...)
	}
	server.Status.Events = events
}

// noteLifecycle reports whether a phase change added a lifecycle event.
func (r *Reconciler) noteLifecycle(server *v1alpha1.MinecraftServer, prevPhase v1alpha1.Phase, prevGaveUp bool) bool {
	phase := server.Status.Phase
	if phase == prevPhase {
		return false
	}
	last := ""
	if n := len(server.Status.Events); n > 0 {
		last = server.Status.Events[n-1].Type
	}
	switch {
	case phase == v1alpha1.PhaseHibernated && last != v1alpha1.EventHibernated:
		r.addEvent(server, v1alpha1.ServerEvent{Type: v1alpha1.EventHibernated})
	case phase == v1alpha1.PhaseRunning && last != v1alpha1.EventStarted:
		ev := v1alpha1.ServerEvent{Type: v1alpha1.EventStarted}
		if last == v1alpha1.EventHibernated {
			ev.Reason = v1alpha1.ReasonWoke
		}
		r.addEvent(server, ev)
	case phase == v1alpha1.PhaseStopped && prevPhase != "" && last != v1alpha1.EventStopped:
		ev := v1alpha1.ServerEvent{Type: v1alpha1.EventStopped}
		if a := server.Status.AutoStop; a != nil {
			ev.Reason = a.Reason
		}
		r.addEvent(server, ev)
	case phase == v1alpha1.PhaseFailed && !prevGaveUp && v1alpha1.StartGaveUp(&server.Status):
		ev := v1alpha1.ServerEvent{Type: v1alpha1.EventStartFailed}
		if c := meta.FindStatusCondition(server.Status.Conditions, v1alpha1.ConditionReady); c != nil {
			ev.Reason = c.Reason
		}
		r.addEvent(server, ev)
	default:
		return false
	}
	return true
}
