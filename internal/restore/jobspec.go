package restore

import (
	"felis.lolicon.best/internal/archivetransfer"
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelServer    = "felis.lolicon.best/server"

	managedByValue = "felis-restore"
	componentValue = "world-restore"

	// AnnotationBackupRef distinguishes duplicate requests from another archive using the same deterministic Job name.
	AnnotationBackupRef = "felis.lolicon.best/backup-ref"

	worldVolume     = "world"
	backupVolume    = "backup"
	felisBinaryPath = "/usr/local/bin/felis"
)

type JobParams struct {
	SourceURL, Token, SHA256 string
	MaxBytes                 int64
	Server                   string
	WorldPVC                 string
	BackupPVC                string
	BackupRef                string
	ArchiveStore             string
	Namespace                string
	ServiceAccount           string
	Image                    string
	BackupRoot               string
	WorldsRoot               string
	Deadline                 time.Duration
	CPULimit                 string
	MemLimit                 string
	RunAsUser                int64
	RunAsGroup               int64
	FSGroup                  int64

	TTLAfterFinished time.Duration
}

func RestoreJobName(server string) string { return "restore-" + server }

func restoreLabels(p JobParams) map[string]string {
	return map[string]string{
		LabelManagedBy: managedByValue,
		LabelComponent: componentValue,
		LabelServer:    p.Server,
	}
}

// RestoreJob mounts only the world and read-only backup volumes, with no Kubernetes token or database credentials.
func RestoreJob(p JobParams) (*batchv1.Job, error) {
	if p.Image == "" {
		return nil, fmt.Errorf("restore: image is empty")
	}
	if p.WorldPVC == "" || (p.BackupPVC == "" && p.SourceURL == "") {
		return nil, fmt.Errorf("restore: world and backup PVC names are required")
	}
	limits, err := resourceLimits(p.CPULimit, p.MemLimit)
	if err != nil {
		return nil, err
	}
	deadline := int64(p.Deadline / time.Second)
	if deadline <= 0 {
		deadline = int64(defaultDeadline / time.Second)
	}
	ttl := int32(p.TTLAfterFinished / time.Second)
	if ttl <= 0 {
		ttl = int32(defaultTTL / time.Second)
	}

	container := corev1.Container{
		Name:    "restore",
		Image:   p.Image,
		Command: []string{felisBinaryPath, "restore"},
		Args: []string{
			"--server", p.Server,
			"--ref", p.BackupRef,
			"--archive-store", p.ArchiveStore,
			"--backup-root", p.BackupRoot,
			"--worlds-root", p.WorldsRoot,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: worldVolume, MountPath: p.WorldsRoot},
			// Read-only backup mounts prevent a compromised extractor from changing other archives.
			{Name: backupVolume, MountPath: p.BackupRoot, ReadOnly: true},
		},
		Resources: corev1.ResourceRequirements{Limits: limits, Requests: limits},
		SecurityContext: &corev1.SecurityContext{
			Privileged:               boolPtr(false),
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"DAC_OVERRIDE"},
			},
		},
	}

	container.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        RestoreJobName(p.Server),
			Namespace:   p.Namespace,
			Labels:      restoreLabels(p),
			Annotations: map[string]string{AnnotationBackupRef: p.BackupRef},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   int64Ptr(deadline),
			TTLSecondsAfterFinished: int32Ptr(ttl),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: restoreLabels(p)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           p.ServiceAccount,
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext:              restorePodSecurityContext(p),
					Containers:                   []corev1.Container{container},
					Volumes: []corev1.Volume{
						{
							Name: worldVolume,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: p.WorldPVC,
								},
							},
						},
						{
							Name: backupVolume,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: p.BackupPVC,
									ReadOnly:  true,
								},
							},
						},
					},
				},
			},
		},
	}
	if p.SourceURL != "" {
		if p.Token == "" || p.SHA256 == "" || p.MaxBytes <= 0 {
			return nil, fmt.Errorf("restore: remote archive credentials and bounds required")
		}
		c := &job.Spec.Template.Spec.Containers[0]
		c.Args = append(c.Args, "--source-url", p.SourceURL, "--sha256", p.SHA256, "--max-bytes", strconv.FormatInt(p.MaxBytes, 10))
		c.Env = []corev1.EnvVar{{Name: archivetransfer.TokenEnv, Value: p.Token}}
		c.VolumeMounts[1] = corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"}
		job.Spec.Template.Spec.Volumes[1] = corev1.Volume{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	}
	return job, nil
}

// RestoreServiceAccount must remain unbound with token automount disabled; its work requires only mounted filesystem access.
func RestoreServiceAccount(namespace, name string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy: managedByValue,
				LabelComponent: componentValue,
			},
		},
		AutomountServiceAccountToken: boolPtr(false),
	}
}

func resourceLimits(cpu, mem string) (corev1.ResourceList, error) {
	if cpu == "" {
		cpu = defaultCPULimit
	}
	if mem == "" {
		mem = defaultMemLimit
	}
	cpuQty, err := resource.ParseQuantity(cpu)
	if err != nil {
		return nil, fmt.Errorf("restore: invalid cpu limit %q: %w", cpu, err)
	}
	memQty, err := resource.ParseQuantity(mem)
	if err != nil {
		return nil, fmt.Errorf("restore: invalid memory limit %q: %w", mem, err)
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:    cpuQty,
		corev1.ResourceMemory: memQty,
	}, nil
}

func boolPtr(b bool) *bool { return &b }

// restorePodSecurityContext defaults to root for private game files and omits FSGroup unless explicitly configured.
func restorePodSecurityContext(p JobParams) *corev1.PodSecurityContext {
	sc := &corev1.PodSecurityContext{
		RunAsNonRoot: boolPtr(false),
		RunAsUser:    int64Ptr(p.RunAsUser),
		RunAsGroup:   int64Ptr(p.RunAsGroup),
	}
	if p.FSGroup > 0 {
		sc.FSGroup = int64Ptr(p.FSGroup)
	}
	return sc
}
func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }
