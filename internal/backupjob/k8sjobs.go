package backupjob

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// K8sJobs is the production Jobs backed by a controller-runtime client. It creates
// the on-demand world-backup Job — nothing more: the backup Job is one-shot and
// self-cleaning (ttlSecondsAfterFinished), so there is no phase or cancel seam, and
// thus no config to hold. Every backup parameter arrives in the JobParams the
// Backuper builds from its own (defaulted) Config. The cluster-bootstrap objects
// (the weak felis-restore SA it reuses) are installed once by the deployment
// manifests, not per backup, so this binding never creates them. It is
// integration-tested against a live cluster, not the hermetic unit suite.
type K8sJobs struct {
	c client.Client
}

// NewK8sJobs builds a Jobs over c.
func NewK8sJobs(c client.Client) *K8sJobs {
	return &K8sJobs{c: c}
}

// CreateBackupJob renders and applies the backup Job. Its name is a deterministic
// function of the server (BackupJobName), so a concurrent backup of the same server
// collides on Create; that collision is mapped to ErrAlreadyExists, which the
// Backuper treats as success (idempotent enqueue).
func (k *K8sJobs) CreateBackupJob(ctx context.Context, p JobParams) error {
	job, err := BackupJob(p)
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
