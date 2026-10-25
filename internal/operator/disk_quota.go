package operator

import (
	"context"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *Reconciler) reconcileDiskQuota(ctx context.Context, server *v1alpha1.MinecraftServer) (bool, error) {
	_, sampled := v1alpha1.ServerDiskSample(server)
	now := r.now()
	if !sampled && v1alpha1.DiskQuotaReason(server, "", now.Time) == "" {
		return false, nil
	}
	reader := r.Volumes
	if reader == nil {
		reader = r.Client
	}
	var claim corev1.PersistentVolumeClaim
	if err := reader.Get(ctx, client.ObjectKey{Namespace: server.Namespace, Name: server.WorldPVC()}, &claim); err != nil && !apierrors.IsNotFound(err) {
		if server.Spec.DesiredState == v1alpha1.DesiredStopped {
			return false, nil
		}
		return false, err
	}
	if claim.Status.Phase != corev1.ClaimBound || claim.DeletionTimestamp != nil {
		claim.UID = ""
	}
	reason := v1alpha1.DiskQuotaReason(server, claim.UID, now.Time)
	if reason == "" {
		return false, nil
	}
	full := reason == v1alpha1.StopDiskFull
	status := metav1.ConditionTrue
	message := "the world volume is below its disk quota"
	if full {
		status = metav1.ConditionFalse
		message = "the world volume reached its disk quota; remove files before starting again"
	} else if reason == v1alpha1.DiskLow {
		message = "the world volume has used at least 90% of its disk quota"
	}
	previous := meta.FindStatusCondition(server.Status.Conditions, v1alpha1.ConditionDiskSpaceAvailable)
	changed := previous == nil || previous.Status != status || previous.Reason != reason
	if changed {
		meta.SetStatusCondition(&server.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionDiskSpaceAvailable,
			Status: status, Reason: reason, Message: message, ObservedGeneration: server.Generation, LastTransitionTime: now})
		if reason == v1alpha1.DiskLow {
			r.addEvent(server, v1alpha1.ServerEvent{Type: v1alpha1.EventDiskWarning, Reason: reason})
			r.event(server, corev1.EventTypeWarning, v1alpha1.EventDiskWarning, message)
		} else if full {
			r.addEvent(server, v1alpha1.ServerEvent{Type: v1alpha1.EventDiskFull, Reason: reason, Stopping: server.Spec.DesiredState == v1alpha1.DesiredRunning})
			r.event(server, corev1.EventTypeWarning, v1alpha1.EventDiskFull, message)
		}
	}
	if full && server.Spec.DesiredState == v1alpha1.DesiredRunning {
		return true, r.stopFor(ctx, server, v1alpha1.StopDiskFull)
	}
	if changed {
		return full, r.patchStatus(ctx, server)
	}
	return full, nil
}
