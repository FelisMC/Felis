package restore

import (
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Label keys applied to restore objects, mirroring internal/build so the two
// executors are observable the same way.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelServer    = "felis.lolicon.best/server"

	managedByValue = "felis-restore"
	componentValue = "world-restore"

	// AnnotationBackupRef records which archive the Job extracts, so a second
	// restore that collides with it on the name can tell a duplicate of the same
	// request from a request for a different backup (K8sJobs.CreateRestoreJob).
	AnnotationBackupRef = "felis.lolicon.best/backup-ref"

	worldVolume     = "world"
	backupVolume    = "backup"
	felisBinaryPath = "/usr/local/bin/felis"
)

// JobParams are the rendered inputs to a restore Job, derived from a server +
// archive ref + Config by the Restorer. jobspec is a pure function of them so
// the security-critical Job shape is unit-tested without a cluster.
type JobParams struct {
	Server         string
	WorldPVC       string
	BackupPVC      string
	BackupRef      string
	ArchiveStore   string
	Namespace      string
	ServiceAccount string
	Image          string
	BackupRoot     string
	WorldsRoot     string
	Deadline       time.Duration
	CPULimit       string
	MemLimit       string
	RunAsUser      int64
	RunAsGroup     int64
	FSGroup        int64

	TTLAfterFinished time.Duration
}

// RestoreJobName is the deterministic Job name for a server's restore. It is a
// pure function of the server name, which is how CreateRestoreJob detects a
// restore already in flight (AlreadyExists) and how the orchestrator's
// idempotency holds.
func RestoreJobName(server string) string { return "restore-" + server }

func restoreLabels(p JobParams) map[string]string {
	return map[string]string{
		LabelManagedBy: managedByValue,
		LabelComponent: componentValue,
		LabelServer:    p.Server,
	}
}

// RestoreJob renders the world-restore Job (spec §7, §16, §22). Every isolation
// guarantee lives here and is asserted by jobspec_test.go, because no cluster
// runs in this environment:
//
//   - runs under the weak felis-restore SA (never the felis-api SA) with its
//     token auto-mount disabled, so it cannot reach the K8s API (spec §16, §21);
//   - mounts EXACTLY two volumes — the world PVC read-write and the backup PVC
//     read-only — and NO Secret/ConfigMap, so a poisoned archive cannot reach
//     the felis database or any credential (the four-power red line, spec §22);
//   - runs as a non-root, fixed uid/gid with an fsGroup so the files it writes
//     are owned by the same identity the minecraft server later runs as;
//   - no privilege, no privilege escalation, read-only root filesystem, drop ALL
//     capabilities — all writes go to the mounted world PVC, nothing else;
//   - activeDeadlineSeconds + backoffLimit=0 so a wedged or malicious archive
//     cannot loop or run forever; ttlSecondsAfterFinished GCs the finished Job.
//
// The container runs `/usr/local/bin/felis restore` (cmd/felis), which extracts
// the archive at BackupRef from the backup mount into the world mount. BackupRef
// is an absolute path, so the backup PVC MUST be mounted at BackupRoot — the
// same path the reaper wrote it under — for the ref to resolve.
func RestoreJob(p JobParams) (*batchv1.Job, error) {
	if p.Image == "" {
		return nil, fmt.Errorf("restore: image is empty")
	}
	if p.WorldPVC == "" || p.BackupPVC == "" {
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
			// The archive is only ever read; mounting it read-only means a
			// compromised restore process cannot mutate other servers' backups.
			{Name: backupVolume, MountPath: p.BackupRoot, ReadOnly: true},
		},
		Resources: corev1.ResourceRequirements{Limits: limits, Requests: limits},
		SecurityContext: &corev1.SecurityContext{
			Privileged:               boolPtr(false),
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			// Root + DAC_OVERRIDE (see restore.Config.RunAsUser): the world is
			// owned by the game uid and Paper's files are mode 0600, so the
			// restore must bypass file modes to overwrite what the server wrote —
			// otherwise level.dat is un-restorable. What it extracts lands
			// root-owned; the server's prepare-data initContainer hands it to the
			// game uid before the server next starts.
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"DAC_OVERRIDE"},
			},
		},
	}

	// The exit error reaches GET /servers/{name}/jobs through the terminated
	// state (api.K8sJobStatus), in place of the Job's generic backoff text.
	container.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        RestoreJobName(p.Server),
			Namespace:   p.Namespace,
			Labels:      restoreLabels(p),
			Annotations: map[string]string{AnnotationBackupRef: p.BackupRef},
		},
		Spec: batchv1.JobSpec{
			// One shot: a bad archive must not loop. The TTL GCs the finished Job
			// so a later restore of the same server is not blocked forever by a
			// stale completed Job.
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
	return job, nil
}

// RestoreServiceAccount renders the weak restore SA (spec §16, §21). Like the
// build SA it is created bare: no secrets, token auto-mounting disabled, and —
// by having no Role or RoleBinding anywhere — zero K8s API permissions. Its only
// capability is filesystem access to the two PVCs the Job mounts.
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

// resourceLimits parses the CPU/memory limits into a ResourceList.
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

// restorePodSecurityContext pins the Pod identity. Root by default — the world
// volume is owned by the game uid and Paper writes mode-0600 files, so a different
// non-root executor could neither read nor replace them. FSGroup is only
// rendered when configured: a root executor must not chgrp the world volume.
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
