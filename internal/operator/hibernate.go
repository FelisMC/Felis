package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/hibernate"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// envHibernate tells `felis launch` to switch off the game's own watchdog, which a long freeze would trip.
	envHibernate = "FELIS_HIBERNATE"
	// LabelHibernateJob marks the node Jobs that freeze and thaw a server.
	LabelHibernateJob = v1alpha1.GroupName + "/hibernate"
	// frozenMemory is what a hibernated pod keeps reserved once its process sits in swap.
	frozenMemory = "128Mi"
	frozenCPU    = "10m"
	// stallLimit is how long RCON may go unanswered before a server whose own watchdog is off counts as hung.
	stallLimit           = 90 * time.Second
	requeueHibernateStep = 2 * time.Second
	requeueWakeBlocked   = 15 * time.Second
	requeueHibernated    = 5 * time.Minute
)

// hibernates reports whether the server is set up to hibernate when idle: managed software, RCON, not a system server.
func hibernates(server *v1alpha1.MinecraftServer) bool {
	return server.Spec.Idle.Hibernates() && server.Spec.Software != nil && server.Spec.Rcon.Enabled &&
		server.Labels[v1alpha1.LabelSystemRole] == ""
}

// podHibernates reports whether the running pod was started ready to freeze: watchdog off and no kubelet liveness probe.
func podHibernates(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != containerName {
			continue
		}
		if c.LivenessProbe != nil {
			return false
		}
		for _, e := range c.Env {
			if e.Name == envHibernate && e.Value == "1" {
				return true
			}
		}
	}
	return false
}

func gameContainerID(pod *corev1.Pod) string {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == containerName && s.State.Running != nil {
			if _, id, ok := strings.Cut(s.ContainerID, "://"); ok {
				return id
			}
		}
	}
	return ""
}

func (r *Reconciler) canHibernate(server *v1alpha1.MinecraftServer, pod *corev1.Pod) bool {
	return r.HibernateNamespace != "" && r.FelisImage != "" && hibernates(server) && podHibernates(pod) &&
		pod.Spec.NodeName != "" && gameContainerID(pod) != ""
}

// beginHibernate stops world saving, flushes the world and starts the freeze; any failure before the freeze stops the server instead.
func (r *Reconciler) beginHibernate(ctx context.Context, server *v1alpha1.MinecraftServer, pod *corev1.Pod) (ctrl.Result, error) {
	password, err := r.rconPassword(ctx, server)
	if err == nil {
		addr := rconAddress(server)
		if err = r.Prober.Commands(ctx, addr, password, []string{"save-off"}); err == nil {
			err = r.Prober.Save(ctx, addr, password)
		}
	}
	if err != nil {
		r.event(server, corev1.EventTypeWarning, "HibernateSkipped", "could not save the world before freezing; stopping instead: "+err.Error())
		return ctrl.Result{}, r.stopFor(ctx, server, v1alpha1.StopIdle)
	}
	var game corev1.Container
	for _, c := range pod.Spec.Containers {
		if c.Name == containerName {
			game = c
		}
	}
	h := &v1alpha1.HibernationStatus{
		Stage:         v1alpha1.HibernationFreezing,
		Since:         r.now(),
		Node:          pod.Spec.NodeName,
		Container:     gameContainerID(pod),
		MemoryRequest: quantityString(game.Resources.Requests, corev1.ResourceMemory),
		MemoryLimit:   quantityString(game.Resources.Limits, corev1.ResourceMemory),
		CPURequest:    quantityString(game.Resources.Requests, corev1.ResourceCPU),
	}
	job, err := r.createHibernateJob(ctx, server, h, "freeze", 0)
	if err != nil {
		r.thawWorldSaving(ctx, server)
		return ctrl.Result{}, err
	}
	h.Job = job
	server.Status.Hibernation = h
	r.markHibernating(server, "Freezing", "saving is off and the world is flushed; freezing the process")
	if err := r.patchStatus(ctx, server); err != nil {
		return ctrl.Result{}, err
	}
	patch := client.MergeFrom(server.DeepCopy())
	server.Spec.DesiredState = v1alpha1.DesiredHibernated
	if err := r.Patch(ctx, server, patch); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("idle hibernate", "emptySeconds", server.Spec.Idle.EmptySecondsBeforeStop, "job", job)
	return ctrl.Result{RequeueAfter: requeueHibernateStep}, nil
}

// reconcileHibernation drives a freeze, holds a frozen server and wakes it when desiredState goes back to Running.
func (r *Reconciler) reconcileHibernation(ctx context.Context, server *v1alpha1.MinecraftServer, desired v1alpha1.DesiredState) (ctrl.Result, error) {
	h := server.Status.Hibernation
	if h.Stage == v1alpha1.HibernationKilling {
		done, err := r.killFrozen(ctx, server, nil)
		if err != nil || !done {
			return ctrl.Result{RequeueAfter: time.Second}, err
		}
		server.Status.Hibernation = nil
		r.markStarting(server, "WakeFailed", "starting afresh from the world saved before the freeze")
		return ctrl.Result{RequeueAfter: requeueStarting}, r.patchStatus(ctx, server)
	}
	pod, err := r.gamePod(ctx, server)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod == nil || pod.DeletionTimestamp != nil || gameContainerID(pod) != h.Container {
		return r.hibernationLost(ctx, server, desired)
	}
	switch h.Stage {
	case v1alpha1.HibernationFreezing:
		done, result, err := r.hibernateJobResult(ctx, h.Job)
		if err != nil || !done {
			return ctrl.Result{RequeueAfter: requeueHibernateStep}, err
		}
		if !result.OK {
			// The node Job resumes the process before refusing, so the server runs on and stops the ordinary way.
			server.Status.Hibernation = nil
			r.thawWorldSaving(ctx, server)
			r.event(server, corev1.EventTypeWarning, "HibernateFailed", "could not hibernate ("+string(result.Code)+": "+result.Message+"); stopping instead")
			return ctrl.Result{}, r.stopFor(ctx, server, v1alpha1.StopIdle)
		}
		if err := r.resize(ctx, pod, frozenResources(h)); err != nil {
			return ctrl.Result{}, err
		}
		h.Stage, h.Job, h.SwappedBytes = v1alpha1.HibernationFrozen, "", result.SwappedBytes
		r.markHibernated(server)
		r.event(server, corev1.EventTypeNormal, "Hibernated", fmt.Sprintf("froze the process and moved %d MiB to swap", result.SwappedBytes>>20))
		return ctrl.Result{RequeueAfter: r.hibernatedRequeue(desired)}, r.patchStatus(ctx, server)

	case v1alpha1.HibernationFrozen:
		if desired != v1alpha1.DesiredRunning {
			return ctrl.Result{RequeueAfter: requeueHibernated}, nil
		}
		if err := r.resize(ctx, pod, originalResources(h)); err != nil {
			return ctrl.Result{}, err
		}
		h.Stage = v1alpha1.HibernationGrowing
		r.markHibernating(server, "Waking", "asking the node for the server's memory back")
		server.Status.Phase = v1alpha1.PhaseStarting
		return ctrl.Result{RequeueAfter: time.Second}, r.patchStatus(ctx, server)

	case v1alpha1.HibernationGrowing:
		if blocked, msg := resizeBlocked(pod); blocked {
			if h.Message != msg {
				h.Message = msg
				r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionFalse, "WakeWaitingForMemory", msg)
				if err := r.patchStatus(ctx, server); err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{RequeueAfter: requeueWakeBlocked}, nil
		}
		if !resized(pod, originalResources(h)) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		job, err := r.createHibernateJob(ctx, server, h, "thaw", h.SwappedBytes)
		if err != nil {
			return ctrl.Result{}, err
		}
		h.Stage, h.Job, h.Message = v1alpha1.HibernationThawing, job, ""
		return ctrl.Result{RequeueAfter: time.Second}, r.patchStatus(ctx, server)

	case v1alpha1.HibernationThawing:
		done, result, err := r.hibernateJobResult(ctx, h.Job)
		if err != nil || !done {
			return ctrl.Result{RequeueAfter: time.Second}, err
		}
		if !result.OK && result.Code == hibernate.CodeInsufficientMemory {
			h.Stage, h.Job, h.Message = v1alpha1.HibernationGrowing, "", result.Message
			r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionFalse, "WakeWaitingForMemory", result.Message)
			return ctrl.Result{RequeueAfter: requeueWakeBlocked}, r.patchStatus(ctx, server)
		}
		if !result.OK {
			// The world was flushed before the freeze, so a cold start loses nothing that was saved.
			r.event(server, corev1.EventTypeWarning, "WakeFailed", "could not resume the frozen process ("+result.Message+"); starting the server afresh")
			r.addEvent(server, v1alpha1.ServerEvent{Type: v1alpha1.EventStopped, Reason: v1alpha1.ReasonWakeFailed})
			_, err := r.killFrozen(ctx, server, pod)
			return ctrl.Result{RequeueAfter: time.Second}, err
		}
		server.Status.Hibernation = nil
		r.thawWorldSaving(ctx, server)
		r.event(server, corev1.EventTypeNormal, "Woke", "resumed the frozen process where it stopped")
		r.markStarting(server, "Woke", "resumed from hibernation")
		return ctrl.Result{RequeueAfter: time.Second}, r.patchStatus(ctx, server)
	}
	return ctrl.Result{}, nil
}

// killFrozen deletes a frozen pod and ends its process from the node, since the pod's preStop hook freezes too and would hold the deletion for its whole grace period; done reports the kill over, after which the deletion finishes on its own.
func (r *Reconciler) killFrozen(ctx context.Context, server *v1alpha1.MinecraftServer, pod *corev1.Pod) (bool, error) {
	h := server.Status.Hibernation
	if h.Stage != v1alpha1.HibernationKilling {
		// Deleting first leaves the pod terminating, so the kubelet does not restart the container the kill ends.
		if err := r.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
			return false, err
		}
		job, err := r.createHibernateJob(ctx, server, h, "kill", 0)
		if err != nil {
			return false, err
		}
		h.Stage, h.Job, h.Message = v1alpha1.HibernationKilling, job, ""
		return false, r.patchStatus(ctx, server)
	}
	done, result, err := r.hibernateJobResult(ctx, h.Job)
	if err != nil || !done {
		return false, err
	}
	if !result.OK && result.Code != hibernate.CodeNotFound {
		r.event(server, corev1.EventTypeWarning, "KillFailed", "could not end the frozen process ("+result.Message+"); its pod ends after the usual grace period")
	}
	return true, nil
}

func (r *Reconciler) hibernatedRequeue(desired v1alpha1.DesiredState) time.Duration {
	if desired == v1alpha1.DesiredRunning {
		return time.Second
	}
	return requeueHibernated
}

// hibernationLost handles a frozen pod that is gone, as after a node restart: the flushed world is what remains.
func (r *Reconciler) hibernationLost(ctx context.Context, server *v1alpha1.MinecraftServer, desired v1alpha1.DesiredState) (ctrl.Result, error) {
	server.Status.Hibernation = nil
	r.event(server, corev1.EventTypeWarning, "HibernationLost", "the frozen process is gone; the world saved before the freeze is kept")
	r.addEvent(server, v1alpha1.ServerEvent{Type: v1alpha1.EventStopped, Reason: v1alpha1.ReasonHibernationLost})
	if desired == v1alpha1.DesiredRunning {
		r.markStarting(server, "HibernationLost", "the frozen process is gone; starting afresh")
		return ctrl.Result{RequeueAfter: requeueStarting}, r.patchStatus(ctx, server)
	}
	return ctrl.Result{}, r.stopFor(ctx, server, v1alpha1.StopIdle)
}

// thawWorldSaving turns world saving back on after a freeze is called off or undone.
func (r *Reconciler) thawWorldSaving(ctx context.Context, server *v1alpha1.MinecraftServer) {
	password, err := r.rconPassword(ctx, server)
	if err == nil {
		err = r.Prober.Commands(ctx, rconAddress(server), password, []string{"save-on"})
	}
	if err != nil {
		log.FromContext(ctx).Info("could not turn world saving back on", "error", err.Error())
	}
}

func (r *Reconciler) markHibernating(server *v1alpha1.MinecraftServer, reason, msg string) {
	server.Status.Phase = v1alpha1.PhaseHibernating
	server.Status.Ready = false
	server.Status.ObservedGeneration = server.Generation
	server.Status.EmptySince = nil
	server.Status.Endpoint = v1alpha1.EndpointStatus{Mode: v1alpha1.EndpointFallback, Address: server.Spec.FallbackServer}
	server.Status.LiveMotd = server.Spec.Motd.Stopped
	r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionFalse, reason, msg)
}

func (r *Reconciler) markHibernated(server *v1alpha1.MinecraftServer) {
	r.markHibernating(server, "Hibernated", "the process is frozen and its memory is in swap")
	server.Status.Phase = v1alpha1.PhaseHibernated
	server.Status.ReadySignalAt = nil
	server.Status.StartRequestedAt = nil
	server.Status.Players = v1alpha1.PlayersStatus{}
}

// originalResources is what the pod ran with before the freeze shrank it.
func originalResources(h *v1alpha1.HibernationStatus) corev1.ResourceRequirements {
	res := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	set := func(list corev1.ResourceList, name corev1.ResourceName, value string) {
		if q, err := resource.ParseQuantity(value); err == nil && value != "" {
			list[name] = q
		}
	}
	set(res.Requests, corev1.ResourceMemory, h.MemoryRequest)
	set(res.Limits, corev1.ResourceMemory, h.MemoryLimit)
	set(res.Requests, corev1.ResourceCPU, h.CPURequest)
	return res
}

func quantityString(list corev1.ResourceList, name corev1.ResourceName) string {
	if q, ok := list[name]; ok {
		return q.String()
	}
	return ""
}

// frozenResources shrinks only what the pod set, since a request cannot be removed again in place.
func frozenResources(h *v1alpha1.HibernationStatus) corev1.ResourceRequirements {
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(frozenMemory)},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(frozenMemory)},
	}
	if h.CPURequest != "" {
		res.Requests[corev1.ResourceCPU] = resource.MustParse(frozenCPU)
	}
	return res
}

// resize changes the game container's resources in place, which frees or reclaims its share of the node.
func (r *Reconciler) resize(ctx context.Context, pod *corev1.Pod, res corev1.ResourceRequirements) error {
	if r.Resize != nil {
		return r.Resize(ctx, pod, res)
	}
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"containers": []map[string]any{{"name": containerName, "resources": res}}}})
	if err != nil {
		return err
	}
	return r.SubResource("resize").Patch(ctx, pod, client.RawPatch(types.StrategicMergePatchType, body))
}

// podResizePending is the condition Kubernetes 1.33 and later set on a resize the node has not granted; the vendored API predates its constant.
const podResizePending corev1.PodConditionType = "PodResizePending"

// resizeBlocked reports a resize the node cannot grant yet.
func resizeBlocked(pod *corev1.Pod) (bool, string) {
	for _, c := range pod.Status.Conditions {
		if c.Type == podResizePending && c.Status == corev1.ConditionTrue {
			return true, strings.TrimSpace("the node cannot grant the server's memory yet: " + c.Reason + " " + c.Message)
		}
	}
	return false, ""
}

// resized reports whether the container now runs with the requested memory limit.
func resized(pod *corev1.Pod, want corev1.ResourceRequirements) bool {
	limit, ok := want.Limits[corev1.ResourceMemory]
	if !ok {
		return true
	}
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == containerName && s.Resources != nil {
			got, ok := s.Resources.Limits[corev1.ResourceMemory]
			return ok && got.Cmp(limit) == 0
		}
	}
	return false
}

// createHibernateJob starts the node Job that freezes or thaws the server's container.
func (r *Reconciler) createHibernateJob(ctx context.Context, server *v1alpha1.MinecraftServer, h *v1alpha1.HibernationStatus, action string, expect int64) (string, error) {
	job := hibernateJob(r.HibernateNamespace, r.FelisImage, server.Name, h, action, expect, r.now().Time)
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", err
	}
	return job.Name, nil
}

func hibernateJob(namespace, image, server string, h *v1alpha1.HibernationStatus, action string, expect int64, now time.Time) *batchv1.Job {
	name := fmt.Sprintf("%s-%s-%s", action, server, strconv.FormatInt(now.Unix(), 36))
	if len(name) > 63 {
		name = name[:63]
	}
	zero, deadline, ttl := int32(0), int64(300), int32(600)
	root, no := int64(0), false
	labels := map[string]string{LabelHibernateJob: action, v1alpha1.LabelServer: server}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &zero,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeName:                     h.Node,
					HostPID:                      true,
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &no,
					Tolerations:                  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:    "hibernate",
						Image:   image,
						Command: []string{felisBinaryPath, "hibernate", action, "--container", h.Container, "--expect", strconv.FormatInt(expect, 10)},
						// Root writes the host's cgroup files; SYS_PTRACE and SYS_NICE let it advise the game's memory back in.
						SecurityContext: &corev1.SecurityContext{
							RunAsUser:                &root,
							RunAsNonRoot:             &no,
							AllowPrivilegeEscalation: &no,
							ReadOnlyRootFilesystem:   ptrBool(true),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
								Add:  []corev1.Capability{"SYS_PTRACE", "SYS_NICE"},
							},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
						},
						VolumeMounts:             []corev1.VolumeMount{{Name: "cgroup", MountPath: "/host/cgroup"}},
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
					}},
					Volumes: []corev1.Volume{{Name: "cgroup", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys/fs/cgroup"}}}},
				},
			},
		},
	}
}

func ptrBool(b bool) *bool { return &b }

// hibernateJobResult reads the node Job's verdict from its pod's termination message.
func (r *Reconciler) hibernateJobResult(ctx context.Context, name string) (bool, hibernate.Result, error) {
	var job batchv1.Job
	if err := r.jobReader().Get(ctx, types.NamespacedName{Namespace: r.HibernateNamespace, Name: name}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return true, hibernate.Result{Code: hibernate.CodeFailed, Message: "the node job is gone"}, nil
		}
		return false, hibernate.Result{}, err
	}
	if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
		return false, hibernate.Result{}, nil
	}
	var pods corev1.PodList
	if err := r.jobReader().List(ctx, &pods, client.InNamespace(r.HibernateNamespace), client.MatchingLabels{"job-name": name}); err != nil {
		return false, hibernate.Result{}, err
	}
	for _, p := range pods.Items {
		for _, s := range p.Status.ContainerStatuses {
			if t := s.State.Terminated; t != nil && t.Message != "" {
				var result hibernate.Result
				if err := json.Unmarshal([]byte(t.Message), &result); err == nil {
					return true, result, nil
				}
			}
		}
	}
	return true, hibernate.Result{Code: hibernate.CodeFailed, Message: "the node job ended without a result"}, nil
}

func (r *Reconciler) jobReader() client.Reader {
	if r.Jobs != nil {
		return r.Jobs
	}
	return r.Client
}

// noteStall reports how long the server's RCON list has gone unanswered, or zero once it answers.
func (r *Reconciler) noteStall(key types.NamespacedName, stalled bool) time.Duration {
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	if !stalled {
		delete(r.stalls, key)
		return 0
	}
	if r.stalls == nil {
		r.stalls = map[types.NamespacedName]time.Time{}
	}
	first, ok := r.stalls[key]
	if !ok {
		r.stalls[key] = r.now().Time
		return 0
	}
	return r.now().Time.Sub(first)
}

// restartHung counts a hung server as a crash, since its own watchdog is off, and restarts or stops it by the crash policy.
func (r *Reconciler) restartHung(ctx context.Context, server *v1alpha1.MinecraftServer, pod *corev1.Pod) error {
	r.noteStall(client.ObjectKeyFromObject(server), false)
	now := r.now()
	if last := server.Status.LastCrash; last == nil || now.Sub(last.At.Time) > v1alpha1.CrashWindow {
		server.Status.Crashes = 0
	}
	server.Status.Crashes++
	server.Status.LastCrash = &v1alpha1.CrashRecord{At: now, Reason: "Unresponsive"}
	limit := server.Spec.Recovery.CrashRestartLimit()
	stop := server.Status.Crashes > limit
	r.addEvent(server, v1alpha1.ServerEvent{Type: v1alpha1.EventCrashed, Reason: "Unresponsive", Crashes: server.Status.Crashes, Limit: limit, Stopping: stop})
	msg := fmt.Sprintf("the server answered nothing for %s; crash %d in a row", stallLimit, server.Status.Crashes)
	if stop {
		r.event(server, corev1.EventTypeWarning, v1alpha1.EventCrashed, msg+", stopping the server")
		reason := v1alpha1.StopCrashLimit
		if limit == 0 {
			reason = v1alpha1.StopCrashed
		}
		return r.stopFor(ctx, server, reason)
	}
	r.event(server, corev1.EventTypeWarning, v1alpha1.EventCrashed, msg+", restarted")
	// The usual grace period keeps the replacement from starting while the hung process still holds the world lock.
	if err := r.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
		return err
	}
	r.markStarting(server, "Unresponsive", msg)
	return r.patchStatus(ctx, server)
}

// stopFrozen ends a hibernated server without a shutdown save: a frozen process cannot act on SIGTERM, and its world was flushed before the freeze.
func (r *Reconciler) stopFrozen(ctx context.Context, server *v1alpha1.MinecraftServer, sts *appsv1.StatefulSet) (ctrl.Result, error) {
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		zero := int32(0)
		sts.Spec.Replicas = &zero
		if err := r.Update(ctx, sts); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.markStopping(server)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: server.Name + "-0", Namespace: server.Namespace}}
	done, err := r.killFrozen(ctx, server, pod)
	if err != nil || !done {
		return ctrl.Result{RequeueAfter: time.Second}, err
	}
	server.Status.Hibernation = nil
	return ctrl.Result{RequeueAfter: requeueStopping}, r.patchStatus(ctx, server)
}
