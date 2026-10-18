package reaper

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/naming"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func WorldPVCName(server string) string {
	return naming.WorldPVCName(server)
}

// gamePodComponent must match internal/operator.ComponentValue.
const gamePodComponent = "server"

type K8sCluster struct {
	c         client.Client
	namespace string
	now       func() time.Time
	// beat defaults to maintenance.Grace/4.
	beat        time.Duration
	distributed bool
}

func NewK8sCluster(c client.Client, namespace string) *K8sCluster {
	return &K8sCluster{c: c, namespace: namespace}
}

func (k *K8sCluster) WithDistributed(enabled bool) *K8sCluster { k.distributed = enabled; return k }
func (k *K8sCluster) RetainedWorlds(ctx context.Context, name string) (bool, error) {
	if !k.distributed {
		return false, nil
	}
	var pvcs corev1.PersistentVolumeClaimList
	if err := k.c.List(ctx, &pvcs, client.InNamespace(k.namespace), client.MatchingLabels{maintenance.LabelServer: name}); err != nil {
		return false, err
	}
	return len(pvcs.Items) > 0, nil
}

func (k *K8sCluster) clock() time.Time {
	if k.now != nil {
		return k.now()
	}
	return time.Now()
}

func (k *K8sCluster) Inspect(ctx context.Context, name string) (ServerCRD, error) {
	var ms v1alpha1.MinecraftServer
	if err := k.get(ctx, name, &ms); err != nil {
		return ServerCRD{}, err
	}
	return ServerCRD{Exempt: ms.Spec.ReaperExempt, PVC: ms.WorldPVC(), UID: string(ms.UID)}, nil
}

// HoldWorld renews the lock every beat and cancels its context after Grace/2 without a successful renewal, before lock expiry.
func (k *K8sCluster) HoldWorld(ctx context.Context, name string) (context.Context, func(), error) {
	if err := k.lock(ctx, name); err != nil {
		return nil, nil, err
	}
	held, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go k.keep(held, cancel, name, done)
	release := func() {
		cancel(nil)
		<-done
		rctx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		_ = k.unlock(rctx, name)
	}
	return held, release, nil
}

func (k *K8sCluster) lock(ctx context.Context, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.get(ctx, name, &ms); err != nil {
			return err
		}
		if ms.Spec.DesiredState != "" && ms.Spec.DesiredState != v1alpha1.DesiredStopped {
			patch := client.MergeFromWithOptions(ms.DeepCopy(), client.MergeFromWithOptimisticLock{})
			ms.Spec.DesiredState = v1alpha1.DesiredStopped
			if err := k.c.Patch(ctx, &ms, patch); err != nil {
				return err
			}
			return fmt.Errorf("%w: it was still up and has been told to stop", ErrNotQuiet)
		}
		if ms.Status.Ready || (ms.Status.Phase != "" && ms.Status.Phase != v1alpha1.PhaseStopped) {
			return fmt.Errorf("%w: it is still stopping (phase %s)", ErrNotQuiet, ms.Status.Phase)
		}
		var pods corev1.PodList
		if err := k.c.List(ctx, &pods, client.InNamespace(k.namespace), client.MatchingLabels{
			v1alpha1.LabelServer: name, v1alpha1.LabelComponent: gamePodComponent,
		}); err != nil {
			return err
		}
		if len(pods.Items) > 0 {
			return fmt.Errorf("%w: its game pod is still shutting down", ErrNotQuiet)
		}
		var jobs batchv1.JobList
		if err := k.c.List(ctx, &jobs, client.InNamespace(k.namespace),
			client.MatchingLabels{maintenance.LabelServer: name}); err != nil {
			return err
		}
		if kind, held := maintenance.Holder(name, ms.Annotations, jobs.Items, k.clock()); held {
			return fmt.Errorf("%w: a %s holds its world", ErrNotQuiet, kind)
		}
		patch := client.MergeFromWithOptions(ms.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if ms.Annotations == nil {
			ms.Annotations = map[string]string{}
		}
		ms.Annotations[maintenance.Annotation] = maintenance.LockValue(maintenance.KindReap, k.clock())
		return k.c.Patch(ctx, &ms, patch)
	})
}

var errLockLost = errors.New("the reap lock is gone from the server")

func (k *K8sCluster) keep(held context.Context, cancel context.CancelCauseFunc, name string, done chan<- struct{}) {
	defer close(done)
	beat := k.beat
	if beat <= 0 {
		beat = maintenance.Grace / 4
	}
	t := time.NewTicker(beat)
	defer t.Stop()
	last := k.clock()
	for {
		select {
		case <-held.Done():
			return
		case <-t.C:
		}
		err := k.refresh(held, name)
		if err == nil {
			last = k.clock()
			continue
		}
		if held.Err() != nil {
			return
		}
		if errors.Is(err, errLockLost) || k.clock().Sub(last) >= maintenance.Grace/2 {
			cancel(fmt.Errorf("reaper: lost the world lock on %s: %w", name, err))
			return
		}
	}
}

func (k *K8sCluster) refresh(ctx context.Context, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.get(ctx, name, &ms); err != nil {
			return err
		}
		if !reapLock(ms.Annotations) {
			return errLockLost
		}
		patch := client.MergeFromWithOptions(ms.DeepCopy(), client.MergeFromWithOptimisticLock{})
		ms.Annotations[maintenance.Annotation] = maintenance.LockValue(maintenance.KindReap, k.clock())
		return k.c.Patch(ctx, &ms, patch)
	})
}

func (k *K8sCluster) unlock(ctx context.Context, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.get(ctx, name, &ms); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		if !reapLock(ms.Annotations) {
			return nil
		}
		patch := client.MergeFromWithOptions(ms.DeepCopy(), client.MergeFromWithOptimisticLock{})
		delete(ms.Annotations, maintenance.Annotation)
		return k.c.Patch(ctx, &ms, patch)
	})
}

func reapLock(annotations map[string]string) bool {
	return strings.HasPrefix(annotations[maintenance.Annotation], maintenance.KindReap+"@")
}

func (k *K8sCluster) WorldExists(ctx context.Context, pvc string) (bool, error) {
	var claim corev1.PersistentVolumeClaim
	err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: pvc}, &claim)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// DeletePVC treats missing volumes as success so partial reaps are idempotent.
func (k *K8sCluster) DeletePVC(ctx context.Context, pvc string) error {
	obj := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: k.namespace, Name: pvc},
	}
	if err := k.c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// DeleteServer carries the checked resourceVersion and UID so a recreated same-name object cannot be deleted.
func (k *K8sCluster) DeleteServer(ctx context.Context, name, uid string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		switch err := k.get(ctx, name, &ms); {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		}
		if string(ms.UID) != uid {
			return fmt.Errorf("MinecraftServer %s is another server now (uid %s, inspected %s)", name, ms.UID, uid)
		}
		rv, u := ms.ResourceVersion, ms.UID
		err := k.c.Delete(ctx, &ms, client.Preconditions{UID: &u, ResourceVersion: &rv})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	})
}

func (k *K8sCluster) get(ctx context.Context, name string, ms *v1alpha1.MinecraftServer) error {
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, ms); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}
