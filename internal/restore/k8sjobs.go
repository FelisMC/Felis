package restore

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// K8sJobs is the production Jobs backed by a controller-runtime client (spec
// §7, §16). It creates the world-restore Job and, when a previous run's finished
// Job still holds the deterministic name, replaces it (see CreateRestoreJob).
// There is no other phase or cancel seam: the Job is one-shot and self-cleaning
// (ttlSecondsAfterFinished), so unlike build.K8sJobs there is no namespace to
// hold — every restore parameter arrives in the JobParams the Restorer builds
// from its own (defaulted) Config. The
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
// of the same server collides on Create. The collision is answered by the state
// of the Job already holding the name:
//
//   - still running (or not yet started) on the same archive: ErrAlreadyExists,
//     which the Restorer treats as success — the idempotent coalesce.
//   - still running on another archive: ErrOtherRestoreRunning. A Job from
//     before the ref annotation cannot be compared and coalesces as before.
//   - finished (succeeded OR failed): the finished Job is deleted and replaced,
//     so the caller's retry enqueues for real. Without this, the deterministic
//     name plus the ten-minute TTL would swallow the retry — most importantly
//     the retry after a FAILED restore, which must not have to wait out the TTL
//     (an E2E audit found exactly that: a retry answered 202 "restoring" while
//     nothing ran).
func (k *K8sJobs) CreateRestoreJob(ctx context.Context, p JobParams) error {
	job, err := RestoreJob(p)
	if err != nil {
		return err
	}
	createErr := k.c.Create(ctx, job)
	if createErr == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(createErr) {
		return createErr
	}

	var existing batchv1.Job
	getErr := k.c.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, &existing)
	if apierrors.IsNotFound(getErr) {
		// The name freed itself (TTL cleanup raced us); one retry.
		return k.recreate(ctx, job)
	}
	if getErr != nil {
		return getErr
	}
	if !restoreJobFinished(&existing) {
		if ref, ok := existing.Annotations[AnnotationBackupRef]; ok && ref != p.BackupRef {
			return ErrOtherRestoreRunning
		}
		return ErrAlreadyExists
	}
	// Background propagation: a Job deleted with the API's default policy
	// orphans its pods, which then outlive it for good.
	if deleteErr := k.c.Delete(ctx, &existing, client.PropagationPolicy(metav1.DeletePropagationBackground)); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
		return deleteErr
	}
	// The API server keeps the object until its job-tracking finalizer has run,
	// so an immediate re-Create would collide again and swallow the retry a second
	// time (found live: the E2E retry still answered 202 while nothing ran). Wait
	// for the name to actually free, bounded, then replace.
	if err := k.waitForNameRelease(ctx, job.Namespace, job.Name); err != nil {
		return err
	}
	return k.recreate(ctx, job)
}

// waitForNameRelease polls until the named Job is gone or the wait budget is
// spent. The job controller releases the tracking finalizer within a second or
// two of the delete, so this normally returns on the first or second probe; the
// bound exists so a stuck finalizer surfaces as an error ("retry shortly")
// instead of another silent success.
func (k *K8sJobs) waitForNameRelease(ctx context.Context, namespace, name string) error {
	const (
		probeInterval = 500 * time.Millisecond
		maxProbes     = 20
	)
	for probe := 0; probe < maxProbes; probe++ {
		var probeJob batchv1.Job
		err := k.c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &probeJob)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(probeInterval):
		}
	}
	return fmt.Errorf("restore: job %s/%s is still terminating after deletion; retry shortly", namespace, name)
}

// recreate retries Create once after a finished Job released the name. A
// collision that survives means a concurrent restore re-created first, so the
// idempotent answer applies again.
func (k *K8sJobs) recreate(ctx context.Context, job *batchv1.Job) error {
	if err := k.c.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ErrAlreadyExists
		}
		return err
	}
	return nil
}

// restoreJobFinished reports whether the Job has reached a terminal state. A Job
// that is merely created-but-not-started (no active pods yet, no completions)
// counts as in flight, not finished, so a duplicate enqueue during startup still
// coalesces.
//
// A terminal condition wins over pods still shutting down: the world-volume lock
// (internal/maintenance.JobFinished) already lets the next restore in at that
// point, and answering it with the coalesce would be a 202 for a restore that
// never runs.
func restoreJobFinished(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete, batchv1.JobFailed, batchv1.JobSuccessCriteriaMet, batchv1.JobFailureTarget:
			return true
		}
	}
	if job.Status.Active > 0 {
		return false
	}
	if job.Status.CompletionTime != nil {
		return true
	}
	return job.Status.Succeeded > 0 || job.Status.Failed > 0
}
