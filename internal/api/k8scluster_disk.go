package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/placement"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type DiskUsageTarget struct {
	Name      string
	ServerUID types.UID
	PVCName   string
	PVCUID    types.UID
	Node      string
}

type ServerDiskUsage struct {
	UsedBytes  int64     `json:"usedBytes"`
	LimitBytes int64     `json:"limitBytes"`
	SampledAt  time.Time `json:"sampledAt"`
	Stale      bool      `json:"stale"`
}

func serverDiskUsage(server *v1alpha1.MinecraftServer, now time.Time) (*ServerDiskUsage, bool) {
	sample, ok := v1alpha1.ServerDiskSample(server)
	limit := v1alpha1.DiskQuotaBytes(server)
	latched := v1alpha1.DiskQuotaReason(server, "", now) == v1alpha1.StopDiskFull
	if !ok || limit <= 0 {
		return nil, latched
	}
	fresh := v1alpha1.DiskSampleFresh(sample, now)
	view := &ServerDiskUsage{UsedBytes: sample.UsedBytes, LimitBytes: limit, SampledAt: sample.SampledAt, Stale: !fresh || (latched && sample.UsedBytes < limit)}
	return view, latched || (!sample.SampledAt.After(now) && sample.UsedBytes >= limit)
}

func (k *K8sCluster) DiskUsageTarget(ctx context.Context, name string) (DiskUsageTarget, error) {
	if err := k.CheckFileAccess(ctx, name); err != nil {
		return DiskUsageTarget{}, err
	}
	var server v1alpha1.MinecraftServer
	if err := k.getServer(ctx, name, &server); err != nil {
		return DiskUsageTarget{}, err
	}
	resolve := k.DiskWorldResolver
	if resolve == nil {
		resolve = placement.Resolve(k.c, k.namespace)
	}
	world, err := resolve(ctx, name)
	if err != nil {
		return DiskUsageTarget{}, err
	}
	if world.Claim != server.WorldPVC() || server.UID == "" {
		return DiskUsageTarget{}, ErrConflict
	}
	var claim corev1.PersistentVolumeClaim
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: world.Claim}, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return DiskUsageTarget{}, ErrNotFound
		}
		return DiskUsageTarget{}, err
	}
	if claim.Status.Phase != corev1.ClaimBound || claim.UID == "" || claim.DeletionTimestamp != nil {
		return DiskUsageTarget{}, ErrConflict
	}
	return DiskUsageTarget{Name: name, ServerUID: server.UID, PVCName: world.Claim, PVCUID: claim.UID, Node: world.Node}, nil
}

// PublishDiskUsage changes only the annotation, preserving the operator's exclusively owned status.
func (k *K8sCluster) PublishDiskUsage(ctx context.Context, target DiskUsageTarget, usedBytes int64, sampledAt time.Time) error {
	if usedBytes < 0 || sampledAt.IsZero() || sampledAt.After(k.clock()) {
		return errors.New("invalid disk usage sample")
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var server v1alpha1.MinecraftServer
		if err := k.getServer(ctx, target.Name, &server); err != nil {
			return err
		}
		current, err := k.DiskUsageTarget(ctx, target.Name)
		if err != nil {
			return err
		}
		if current != target {
			return ErrConflict
		}
		if server.UID != target.ServerUID || server.WorldPVC() != target.PVCName {
			return ErrConflict
		}
		if old, ok := v1alpha1.ParseDiskUsage(server.Annotations[v1alpha1.AnnotationDiskUsage]); ok &&
			old.ServerUID == target.ServerUID && old.PVCName == target.PVCName && old.PVCUID == target.PVCUID &&
			!old.SampledAt.After(k.clock()) && !sampledAt.After(old.SampledAt) {
			return ErrConflict
		}
		sample := v1alpha1.DiskUsageSample{ServerUID: target.ServerUID, PVCName: target.PVCName, PVCUID: target.PVCUID, UsedBytes: usedBytes, SampledAt: sampledAt.UTC()}
		raw, err := json.Marshal(sample)
		if err != nil {
			return err
		}
		patch := client.MergeFromWithOptions(server.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if server.Annotations == nil {
			server.Annotations = map[string]string{}
		}
		server.Annotations[v1alpha1.AnnotationDiskUsage] = string(raw)
		return k.c.Patch(ctx, &server, patch)
	})
}

func (k *K8sCluster) checkDiskQuota(ctx context.Context, server *v1alpha1.MinecraftServer) error {
	_, sampled := v1alpha1.ServerDiskSample(server)
	if !sampled && v1alpha1.DiskQuotaReason(server, "", k.clock()) != v1alpha1.StopDiskFull {
		return nil
	}
	var claim corev1.PersistentVolumeClaim
	err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: server.WorldPVC()}, &claim)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if claim.Status.Phase != corev1.ClaimBound || claim.DeletionTimestamp != nil {
		claim.UID = ""
	}
	if v1alpha1.DiskQuotaReason(server, claim.UID, k.clock()) == v1alpha1.StopDiskFull {
		return newError(http.StatusConflict, "disk_full", "the server's world volume reached its disk quota; remove files and wait for a successful usage sample before starting")
	}
	return nil
}
