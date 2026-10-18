package backupjob

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type K8sJobs struct {
	c client.Client
}

func NewK8sJobs(c client.Client) *K8sJobs {
	return &K8sJobs{c: c}
}

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
