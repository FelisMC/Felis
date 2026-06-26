package reaper

import (
	"context"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorldPVCName returns the world PVC name for a server. The convention
// ("world-<name>-0") is owned by internal/naming because the operator, the
// reaper, and restore all depend on it; this is a thin alias kept so existing
// reaper call sites read naturally.
func WorldPVCName(server string) string {
	return naming.WorldPVCName(server)
}

// K8sCluster is the production Cluster backed by a controller-runtime client
// (spec §4, §18). It reads spec.reaperExempt, deletes the world PVC, and flips
// spec.desiredState to Stopped — nothing else. It is integration-tested against
// a live cluster, not the hermetic reaper_test.go suite.
type K8sCluster struct {
	c         client.Client
	namespace string
}

// NewK8sCluster builds a Cluster over c, scoped to namespace.
func NewK8sCluster(c client.Client, namespace string) *K8sCluster {
	return &K8sCluster{c: c, namespace: namespace}
}

func (k *K8sCluster) Inspect(ctx context.Context, name string) (ServerCRD, error) {
	var ms v1alpha1.MinecraftServer
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return ServerCRD{}, ErrNotFound
		}
		return ServerCRD{}, err
	}
	return ServerCRD{Exempt: ms.Spec.ReaperExempt, PVC: WorldPVCName(name)}, nil
}

// DeletePVC deletes the world PersistentVolumeClaim. A missing PVC is not an
// error: the reap is idempotent and a re-run after a partial failure must still
// converge.
func (k *K8sCluster) DeletePVC(ctx context.Context, pvc string) error {
	obj := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: k.namespace, Name: pvc},
	}
	if err := k.c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// Stop sets spec.desiredState=Stopped with a merge patch so a concurrent status
// write by the operator is never clobbered (spec §9.1).
func (k *K8sCluster) Stop(ctx context.Context, name string) error {
	var ms v1alpha1.MinecraftServer
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	patch := client.MergeFrom(ms.DeepCopy())
	ms.Spec.DesiredState = v1alpha1.DesiredStopped
	return k.c.Patch(ctx, &ms, patch)
}
