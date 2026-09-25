package build

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// K8sJobs is the production Jobs backed by a controller-runtime client (spec
// §16). It creates the Kaniko+Trivy build Job, reads its phase for the scan-gate
// translation, and deletes it on cancel — nothing more. The cluster-bootstrap
// objects (the felis-build namespace, the weak SA, and the egress
// NetworkPolicy) are installed once by the deployment manifests (spec §21), not
// per build, so this binding never needs to create them. It is integration-
// tested against a live cluster, not the hermetic build_test.go suite.
type K8sJobs struct {
	c   client.Client
	cfg Config
}

// NewK8sJobs builds a Jobs over c using cfg for the namespace and image refs.
func NewK8sJobs(c client.Client, cfg Config) *K8sJobs {
	return &K8sJobs{c: c, cfg: cfg.withDefaults()}
}

// CreateBuildJob renders and applies the build Job, returning its name. The Job
// is the security-critical object; its shape is fixed by BuildJob (jobspec.go)
// and asserted by jobspec_test.go.
func (k *K8sJobs) CreateBuildJob(ctx context.Context, p JobParams) (string, error) {
	job, err := BuildJob(p)
	if err != nil {
		return "", err
	}
	if err := k.c.Create(ctx, job); err != nil {
		// The name is the build's; an existing Job is this build's own, created
		// by a start that stopped before recording it.
		if apierrors.IsAlreadyExists(err) {
			return job.Name, nil
		}
		return "", err
	}
	return job.Name, nil
}

// JobPhase reads the Job and maps its status to a JobPhase. A missing Job
// (GC'd, never created) is JobUnknown, which Sync treats as failed. The mapping
// is deliberately conservative: a Job is Succeeded only when the Complete
// condition is true, so a half-finished Job is never admitted.
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
			// Covers a scan the policy blocked (scan-gate exits 1), a failed
			// build, scan or push step, and DeadlineExceeded — all are a
			// rejected build.
			return JobFailed, nil
		}
	}
	if job.Status.Active > 0 {
		return JobRunning, nil
	}
	return JobPending, nil
}

// CancelBuildJob deletes the Job and, via background propagation, its pods. A
// missing Job is not an error: cancellation is idempotent.
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
