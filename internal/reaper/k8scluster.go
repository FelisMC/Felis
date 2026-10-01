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

// WorldPVCName returns the world PVC name for a server. The convention
// ("world-<name>-0") is owned by internal/naming because the operator, the
// reaper, and restore all depend on it; this is a thin alias kept so existing
// reaper call sites read naturally.
func WorldPVCName(server string) string {
	return naming.WorldPVCName(server)
}

// gamePodComponent is the operator's component label on a game server's pod
// (internal/operator.ComponentValue; k8scluster_test pins the two).
const gamePodComponent = "server"

// K8sCluster is the production Cluster backed by a controller-runtime client
// (spec §4, §18). It reads spec.reaperExempt, stops a server and holds its world
// volume through the maintenance lock, deletes the world PVC, and removes the
// MinecraftServer of a server an admin deleted — nothing else.
type K8sCluster struct {
	c         client.Client
	namespace string
	now       func() time.Time
	// beat is how often a held lock is rewritten; maintenance.Grace/4 unless a
	// test shortens it.
	beat        time.Duration
	distributed bool
}

// NewK8sCluster builds a Cluster over c, scoped to namespace.
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

// HoldWorld implements Cluster. The lock is the same Annotation felis-api
// writes before a restore, backup or file-write Job (internal/maintenance): a
// wake through felis-api, the operator scaling the server up, and another
// world operation all refuse while it is fresh. With no Job behind it, it
// holds for maintenance.Grace after each write, so it is rewritten every beat;
// the held context is cancelled once a rewrite has failed for Grace/2, well
// before any reader could take the lock as lapsed.
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

// lock stops the server if it is still meant to run and takes the reap lock
// once it is fully down and nothing else holds its world.
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

// errLockLost reports that the lock on the object is no longer the reaper's.
var errLockLost = errors.New("the reap lock is gone from the server")

// keep rewrites the lock every beat until held ends, and ends held itself once
// it has not managed to for Grace/2.
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

// unlock drops the lock if it is still the reaper's; a lock left behind lapses
// after maintenance.Grace on its own.
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

// WorldExists reports whether the world PersistentVolumeClaim exists.
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

// DeleteServer implements Cluster. The read and the delete are one step (the
// delete carries the resourceVersion read), so the object removed is the one
// whose uid was checked: a server made again under the name is refused.
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
