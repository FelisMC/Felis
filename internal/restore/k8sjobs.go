package restore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type K8sJobs struct {
	c client.Client
}

func NewK8sJobs(c client.Client) *K8sJobs {
	return &K8sJobs{c: c}
}

// CreateRestoreJob compares archive references on name collisions and replaces terminal Jobs instead of absorbing a retry.
func (k *K8sJobs) CreateRestoreJob(ctx context.Context, p JobParams) error {
	var secret *corev1.Secret
	if p.SourceURL != "" {
		if p.Token == "" {
			return fmt.Errorf("restore: remote archive token is required")
		}
		if p.TokenSecretName == "" {
			p.TokenSecretName = randomRestoreTokenSecretName()
		}
	}
	job, err := RestoreJob(p)
	if err != nil {
		return err
	}
	if p.SourceURL != "" {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      p.TokenSecretName,
				Namespace: job.Namespace,
				Labels:    job.Labels,
			},
			Data: map[string][]byte{TokenSecretKey: []byte(p.Token)},
		}
		if err := k.c.Create(ctx, secret); err != nil {
			return err
		}
	}
	cleanupSecret := func() {
		if secret != nil {
			_ = k.c.Delete(ctx, secret)
		}
	}
	createErr := k.c.Create(ctx, job)
	if createErr == nil {
		if err := k.ownSecret(ctx, secret, job); err != nil {
			_ = k.c.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
			cleanupSecret()
			return err
		}
		return nil
	}
	if !apierrors.IsAlreadyExists(createErr) {
		cleanupSecret()
		return createErr
	}

	var existing batchv1.Job
	getErr := k.c.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, &existing)
	if apierrors.IsNotFound(getErr) {
		// TTL cleanup released the name during the lookup; retry creation once.
		return k.recreate(ctx, job, secret)
	}
	if getErr != nil {
		cleanupSecret()
		return getErr
	}
	if !restoreJobFinished(&existing) {
		cleanupSecret()
		if ref, ok := existing.Annotations[AnnotationBackupRef]; ok && ref != p.BackupRef {
			return ErrOtherRestoreRunning
		}
		return ErrAlreadyExists
	}
	// Use background propagation so deleting the Job also deletes its Pods instead of orphaning them.
	if deleteErr := k.c.Delete(ctx, &existing, client.PropagationPolicy(metav1.DeletePropagationBackground)); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
		cleanupSecret()
		return deleteErr
	}
	// Wait for the tracking finalizer to release the name before recreating, or the retry could collide and report a false enqueue.
	if err := k.waitForNameRelease(ctx, job.Namespace, job.Name); err != nil {
		cleanupSecret()
		return err
	}
	return k.recreate(ctx, job, secret)
}

func randomRestoreTokenSecretName() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "restore-tok-" + hex.EncodeToString(b[:])
}

func (k *K8sJobs) ownSecret(ctx context.Context, secret *corev1.Secret, job *batchv1.Job) error {
	if secret == nil || job.UID == "" {
		return nil
	}
	patch := client.MergeFrom(secret.DeepCopy())
	secret.OwnerReferences = []metav1.OwnerReference{{
		APIVersion:         "batch/v1",
		Kind:               "Job",
		Name:               job.Name,
		UID:                job.UID,
		Controller:         boolPtr(true),
		BlockOwnerDeletion: boolPtr(true),
	}}
	return k.c.Patch(ctx, secret, patch)
}

// waitForNameRelease is bounded so a stuck finalizer becomes an error rather than a successful no-op.
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

// recreate retries once and reapplies idempotency if another request wins the name.
func (k *K8sJobs) recreate(ctx context.Context, job *batchv1.Job, secret *corev1.Secret) error {
	if err := k.c.Create(ctx, job); err != nil {
		if secret != nil {
			_ = k.c.Delete(ctx, secret)
		}
		if apierrors.IsAlreadyExists(err) {
			return ErrAlreadyExists
		}
		return err
	}
	if err := k.ownSecret(ctx, secret, job); err != nil {
		_ = k.c.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
		if secret != nil {
			_ = k.c.Delete(ctx, secret)
		}
		return err
	}
	return nil
}

// restoreJobFinished trusts terminal conditions even while Pods exit; created but not started Jobs remain in flight.
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
