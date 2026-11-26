package build

import (
	"context"
	"fmt"
	"strings"
	"time"

	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/registrygate"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	var secret *corev1.Secret
	if k.cfg.RegistryBuildToken != "" {
		repo, err := repoFromImageRef(p.ImageRef)
		if err != nil {
			return "", err
		}
		deadline := p.Deadline
		if deadline <= 0 {
			deadline = defaultDeadline
		}
		tok, err := registrygate.BuildPushToken(k.cfg.RegistryBuildToken, repo, time.Now().Add(deadline+15*time.Minute))
		if err != nil {
			return "", err
		}
		p.PushSecretName = BuildPushSecretName(p.BuildID)
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      p.PushSecretName,
				Namespace: p.Namespace,
				Labels:    buildLabels(p),
			},
			Data: map[string][]byte{
				naming.RegistryPushUsernameKey: []byte(registrygate.PrincipalBuild),
				naming.RegistryPushPasswordKey: []byte(tok),
			},
		}
		if err := k.c.Create(ctx, secret); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return "", err
			}
			if err := k.c.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
				return "", err
			}
			if err := k.c.Create(ctx, secret); err != nil {
				return "", err
			}
		}
	}
	job, err := BuildJob(p)
	if err != nil {
		if secret != nil {
			_ = k.c.Delete(ctx, secret)
		}
		return "", err
	}
	if err := k.c.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if secret != nil {
				var existing batchv1.Job
				if getErr := k.c.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: job.Name}, &existing); getErr == nil && existing.UID != "" {
					_ = k.ownPushSecret(ctx, secret, &existing)
				}
			}
			return job.Name, nil
		}
		if secret != nil {
			_ = k.c.Delete(ctx, secret)
		}
		return "", err
	}
	if secret != nil && job.UID != "" {
		if err := k.ownPushSecret(ctx, secret, job); err != nil {
			_ = k.CancelBuildJob(ctx, job.Name)
			_ = k.c.Delete(ctx, secret)
			return "", err
		}
	}
	return job.Name, nil
}

func (k *K8sJobs) ownPushSecret(ctx context.Context, secret *corev1.Secret, job *batchv1.Job) error {
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

func repoFromImageRef(ref string) (string, error) {
	_, rest, ok := splitRegistryHost(ref)
	if !ok {
		return "", fmt.Errorf("build: image reference %q must be host-qualified", ref)
	}
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		rest = rest[:i]
	}
	repo, _ := splitTag(rest)
	if repo == "" {
		return "", fmt.Errorf("build: image reference %q has no repository", ref)
	}
	return repo, nil
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
	if buildID, ok := strings.CutPrefix(jobName, "build-"); ok && buildID != "" {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: k.cfg.Namespace, Name: BuildPushSecretName(buildID)}}
		if err := k.c.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
