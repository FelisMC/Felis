package build

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// K8sJobs is the production Jobs backed by a controller-runtime client; it needs a live cluster to test.
type K8sJobs struct {
	c   client.Client
	cfg Config
}

func NewK8sJobs(c client.Client, cfg Config) *K8sJobs {
	return &K8sJobs{c: c, cfg: cfg.withDefaults()}
}

// CreateBuildJob applies the build Job and returns its name; an existing Job of the same name is treated as success.
func (k *K8sJobs) CreateBuildJob(ctx context.Context, p JobParams) (string, error) {
	job, err := BuildJob(p)
	if err != nil {
		return "", err
	}
	if err := k.c.Create(ctx, job); err != nil {
		// left by a start that stopped before recording the Job
		if apierrors.IsAlreadyExists(err) {
			return job.Name, nil
		}
		return "", err
	}
	return job.Name, nil
}

// JobPhase maps the Job status to a JobPhase; a missing Job is JobUnknown.
// Only a true Complete condition counts as succeeded, so a half-finished Job is never admitted.
func (k *K8sJobs) JobPhase(ctx context.Context, jobName string) (JobPhase, error) {
	var job batchv1.Job
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.cfg.Namespace, Name: jobName}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return JobUnknown, nil
		}
		return JobUnknown, err
	}
	for _, cond := range job.Status.Conditions {
		if cond.Status != "True" {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			return JobSucceeded, nil
		case batchv1.JobFailed:
			return JobFailed, nil
		}
	}
	if job.Status.Active > 0 {
		return JobRunning, nil
	}
	return JobPending, nil
}

// CancelBuildJob deletes the Job and, by background propagation, its pods; a missing Job is not an error.
func (k *K8sJobs) CancelBuildJob(ctx context.Context, jobName string) error {
	bg := metav1.DeletePropagationBackground
	obj := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: k.cfg.Namespace, Name: jobName},
	}
	if err := k.c.Delete(ctx, obj, &client.DeleteOptions{PropagationPolicy: &bg}); err != nil &&
		!apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
