package build

import (
	"context"
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Vars so tests can shrink them. The probe pod's image is the api's own, so it is
// already on the node; the timeout covers a slow pod start, and a runtime that
// cannot do user namespaces fails the pod well within it.
var (
	usernsProbeTimeout = 3 * time.Minute
	usernsProbePoll    = 2 * time.Second
)

// UsernsProbeJob renders the one-shot Job that asks the cluster whether a build
// pod can run with hostUsers: false. The pod has the parts of a build pod that
// need idmapped mounts and namespaced capabilities: root with kaniko's three
// capabilities, the RuntimeDefault seccomp profile and an emptyDir. It runs
// `felis version`, which touches nothing.
func UsernsProbeJob(namespace, serviceAccount, image, name string) *batchv1.Job {
	labels := map[string]string{LabelManagedBy: managedByValue, LabelComponent: "userns-probe"}
	small := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("100m"),
			corev1.ResourceMemory:           resource.MustParse("64Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("64Mi"),
		},
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("10m"),
			corev1.ResourceMemory:           resource.MustParse("16Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("16Mi"),
		},
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   int64Ptr(int64(usernsProbeTimeout / time.Second)),
			TTLSecondsAfterFinished: int32Ptr(300),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           serviceAccount,
					AutomountServiceAccountToken: boolPtr(false),
					HostUsers:                    boolPtr(false),
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:      "probe",
						Image:     image,
						Args:      []string{"version"},
						Resources: small,
						SecurityContext: &corev1.SecurityContext{
							Privileged:               boolPtr(false),
							AllowPrivilegeEscalation: boolPtr(false),
							RunAsUser:                int64Ptr(0),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
								Add:  []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER"},
							},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}},
					}},
					Volumes: []corev1.Volume{{
						Name:         "scratch",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: quantityPtr(resource.MustParse("16Mi"))}},
					}},
				},
			},
		},
	}
}

// ProbeUserNamespaces runs UsernsProbeJob and reports whether its pod succeeded.
// A pod that fails, or never starts before the timeout, answers false; err is set
// only when the Job could not be created or read. The Job is deleted afterwards.
func (k *K8sJobs) ProbeUserNamespaces(ctx context.Context, image string) (bool, error) {
	name := "userns-probe-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	job := UsernsProbeJob(k.cfg.Namespace, k.cfg.ServiceAccount, image, name)
	if err := k.c.Create(ctx, job); err != nil {
		return false, fmt.Errorf("create the probe job: %w", err)
	}
	defer func() {
		bg := metav1.DeletePropagationBackground
		del := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: k.cfg.Namespace, Name: name}}
		_ = k.c.Delete(context.WithoutCancel(ctx), del, &client.DeleteOptions{PropagationPolicy: &bg})
	}()
	deadline := time.Now().Add(usernsProbeTimeout)
	for {
		var got batchv1.Job
		err := k.c.Get(ctx, types.NamespacedName{Namespace: k.cfg.Namespace, Name: name}, &got)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("read the probe job: %w", err)
		}
		for _, cond := range got.Status.Conditions {
			if cond.Status != corev1.ConditionTrue {
				continue
			}
			switch cond.Type {
			case batchv1.JobComplete:
				return true, nil
			case batchv1.JobFailed:
				return false, nil
			}
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(usernsProbePoll):
		}
	}
}
