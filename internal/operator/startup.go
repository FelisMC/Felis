package operator

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/software"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (r *Reconciler) desiredStatefulSet(server *v1alpha1.MinecraftServer, passwordStamp string) (*appsv1.StatefulSet, error) {
	placed := server.DeepCopy()
	if placed.Spec.NodeName == "" {
		placed.Spec.NodeName = r.ControllerNode
	}
	desired, err := buildStatefulSetWith(placed, 1, podOptions{FelisImage: r.FelisImage, GateProbe: r.EgressProbe, ArtifactCache: r.ArtifactCache})
	if err != nil {
		return nil, err
	}
	if r.floats(server) {
		desired.Spec.Template.Spec.NodeSelector = map[string]string{placement.LabelRole: placement.RoleController}
	}
	if passwordStamp != "" {
		if desired.Spec.Template.Annotations == nil {
			desired.Spec.Template.Annotations = map[string]string{}
		}
		desired.Spec.Template.Annotations[RconSecretAnnotation] = passwordStamp
	}
	return desired, nil
}

func deferStartupTemplate(existing, desired *appsv1.StatefulSet, felisImage string) (bool, error) {
	stamp := existing.Annotations[PodTemplateAnnotation]
	if stamp == "" {
		return false, nil
	}
	old := slices.IndexFunc(existing.Spec.Template.Spec.Containers, func(c corev1.Container) bool { return c.Name == containerName })
	template := desired.Spec.Template.DeepCopy()
	next := slices.IndexFunc(template.Spec.Containers, func(c corev1.Container) bool { return c.Name == containerName })
	if old < 0 || next < 0 {
		return false, nil
	}
	previous, current := &existing.Spec.Template.Spec.Containers[old], &template.Spec.Containers[next]
	if !reflect.DeepEqual(platformStartupEnv(previous.Env), platformStartupEnv(current.Env)) {
		return false, nil
	}
	current.Args, current.Env = previous.Args, previous.Env
	if slices.Equal(current.Command, []string{felisBinCopy, "launch", "--data", dataMountPath, "--"}) &&
		slices.Equal(previous.Command, []string{felisBinCopy, "launch", "--data", dataMountPath}) {
		current.Command = previous.Command
	}
	projected, err := podTemplateStamp(template, felisImage)
	return err == nil && projected == stamp, err
}

func platformStartupEnv(env []corev1.EnvVar) []corev1.EnvVar {
	var out []corev1.EnvVar
	for _, entry := range env {
		if entry.Name != "JAVA_FLAGS" && software.IsReservedEnv(entry.Name) {
			out = append(out, entry)
		}
	}
	return out
}

type restartTarget struct {
	Request       string    `json:"request"`
	PodUID        types.UID `json:"pod_uid"`
	StopAttempted bool      `json:"stop_attempted,omitempty"`
}

func (r *Reconciler) takeRestartRequest(ctx context.Context, server *v1alpha1.MinecraftServer, desired v1alpha1.DesiredState, annotation string) (bool, error) {
	raw := server.Annotations[annotation]
	if desired != v1alpha1.DesiredRunning || raw == "" {
		return false, r.clearRestartRequest(ctx, server, annotation)
	}
	var target restartTarget
	resuming := strings.HasPrefix(raw, "{")
	if resuming && (len(raw) > 4096 || json.Unmarshal([]byte(raw), &target) != nil || target.Request == "" || len(target.Request) > 256 || len(target.PodUID) > 128) {
		return false, errors.New("invalid pinned restart request")
	}
	explicit := annotation == v1alpha1.AnnotationRestart
	if !resuming && server.Status.Phase != v1alpha1.PhaseFailed && (!explicit || server.Status.Phase != v1alpha1.PhaseRunning) {
		return false, r.clearRestartRequest(ctx, server, annotation)
	}
	if !resuming {
		if len(raw) > 256 {
			return false, errors.New("restart request is too large")
		}
		pod, err := r.gamePod(ctx, server)
		if err != nil {
			return false, err
		}
		target.Request = raw
		if pod != nil {
			target.PodUID = pod.UID
		}
		if err := r.persistRestartTarget(ctx, server, annotation, target); err != nil {
			return false, err
		}
	}
	passwordStamp, err := r.ensureRconSecret(ctx, server)
	if err != nil {
		return false, err
	}
	workload, err := r.desiredStatefulSet(server, passwordStamp)
	if err != nil {
		return false, err
	}
	if err := controllerutil.SetControllerReference(server, workload, r.Scheme); err != nil {
		return false, err
	}
	stamp, err := podTemplateStamp(&workload.Spec.Template, r.FelisImage)
	if err != nil {
		return false, err
	}
	var current appsv1.StatefulSet
	err = r.Get(ctx, client.ObjectKeyFromObject(server), &current)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if apierrors.IsNotFound(err) {
		workload.Annotations = map[string]string{PodTemplateAnnotation: stamp}
		if err := r.Create(ctx, workload); err != nil {
			return false, err
		}
		current = *workload
	} else {
		if !target.StopAttempted && explicit && len(server.Spec.Lifecycle.StopCommands) > 0 && target.PodUID != "" {
			pod, err := r.gamePod(ctx, server)
			if err != nil {
				return false, err
			}
			if pod != nil && pod.UID == target.PodUID && pod.DeletionTimestamp == nil {
				r.saveBeforeStop(ctx, server, &current)
				target.StopAttempted = true
				if err := r.persistRestartTarget(ctx, server, annotation, target); err != nil {
					return false, err
				}
			}
		}
		current.Labels = workload.Labels
		current.Spec.Replicas = workload.Spec.Replicas
		current.Spec.Template = workload.Spec.Template
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[PodTemplateAnnotation] = stamp
		if err := r.Update(ctx, &current); err != nil {
			return false, err
		}
	}
	if current.Status.ObservedGeneration < current.Generation {
		return true, nil
	}
	if err := r.deleteRestartTarget(ctx, server, target.PodUID); err != nil {
		return false, err
	}
	if server.Status.Phase != v1alpha1.PhaseStarting {
		server.Status.AutoRestarts = 0
		server.Status.StartRequestedAt = nil
		reason, message := "StartRetried", "start requested again after it failed; recreated the pod"
		if explicit {
			reason, message = "RestartRequested", "restart requested; recreated the pod"
		}
		r.markStarting(server, reason, message)
		if err := r.patchStatus(ctx, server); err != nil {
			return false, err
		}
		log.FromContext(ctx).Info(message)
		r.event(server, corev1.EventTypeNormal, reason, message)
	}
	if err := r.clearRestartRequest(ctx, server, annotation); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Reconciler) persistRestartTarget(ctx context.Context, server *v1alpha1.MinecraftServer, annotation string, target restartTarget) error {
	encoded, err := json.Marshal(target)
	if err != nil {
		return err
	}
	patch := client.MergeFromWithOptions(server.DeepCopy(), client.MergeFromWithOptimisticLock{})
	server.Annotations[annotation] = string(encoded)
	return r.Patch(ctx, server, patch)
}

func (r *Reconciler) clearRestartRequest(ctx context.Context, server *v1alpha1.MinecraftServer, annotation string) error {
	patch := client.MergeFromWithOptions(server.DeepCopy(), client.MergeFromWithOptimisticLock{})
	delete(server.Annotations, annotation)
	return r.Patch(ctx, server, patch)
}

func (r *Reconciler) deleteRestartTarget(ctx context.Context, server *v1alpha1.MinecraftServer, uid types.UID) error {
	if uid == "" {
		return nil
	}
	pod, err := r.gamePod(ctx, server)
	if err != nil || pod == nil || pod.UID != uid || pod.DeletionTimestamp != nil {
		return err
	}
	err = r.Delete(ctx, pod, client.Preconditions{UID: &uid})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if !apierrors.IsConflict(err) {
		return err
	}
	current, readErr := r.gamePod(ctx, server)
	if readErr != nil {
		return readErr
	}
	if current == nil || current.UID != uid {
		return nil
	}
	return err
}
