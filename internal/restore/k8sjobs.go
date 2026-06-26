package restore

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// K8sJobs is the production Jobs backed by a controller-runtime client (spec
// §7, §16). It creates the world-restore Job — nothing more: the restore Job is
// one-shot and self-cleaning (ttlSecondsAfterFinished), so there is no phase or
// cancel seam, and thus no config to hold (unlike build.K8sJobs, which needs the
// namespace to read and delete its Job). Every restore parameter arrives in the
// JobParams the Restorer builds from its own (defaulted) Config. The
// cluster-bootstrap objects (the weak felis-restore SA) are installed once by
// the deployment manifests (spec §21), not per restore, so this binding never
// creates them. It is integration-tested against a live cluster, not the
// hermetic restore_test.go suite.
type K8sJobs struct {
	c client.Client
}

// NewK8sJobs builds a Jobs over c. The restore Job's parameters all travel in
// JobParams, so there is no Config to retain here.
func NewK8sJobs(c client.Client) *K8sJobs {
	return &K8sJobs{c: c}
}

// CreateRestoreJob renders and applies the restore Job. Its name is a
// deterministic function of the server (RestoreJobName), so a concurrent restore
// of the same server collides on Create; that collision is mapped to
// ErrAlreadyExists, which the Restorer treats as success (idempotent enqueue).
func (k *K8sJobs) CreateRestoreJob(ctx context.Context, p JobParams) error {
	job, err := RestoreJob(p)
	if err != nil {
		return err
	}
	if err := k.c.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ErrAlreadyExists
		}
		return err
	}
	return nil
}
