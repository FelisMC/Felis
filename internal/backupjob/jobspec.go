package backupjob

import (
	"fmt"
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

	managedByValue = "felis-backup"
	componentValue = "world-backup"

	labelThenRestore          = "felis.lolicon.best/then-restore"
	thenRestorePending        = "pending"
	annotationRestoreRef      = "felis.lolicon.best/restore-ref"
	annotationRestoreBackupID = "felis.lolicon.best/restore-backup-id"

	ReasonPreRestore = "pre_restore"
	ReasonScheduled  = "scheduled"
	ReasonReplica    = "replica"
	LabelReason      = "felis.lolicon.best/backup-reason"
	// LabelLive lets recovery observe copy completion and restore game saving.
	LabelLive      = "felis.lolicon.best/live-backup"
	StageContainer = "stage"

	worldVolume     = "world"
	backupVolume    = "backup"
	configVolume    = "config"
	tmpVolume       = "tmp"
	stageVolume     = "stage"
	liveMountPath   = "/live"
	felisBinaryPath = "/usr/local/bin/felis"
)

type JobParams struct {
	Server string
	// JobName empty uses the deterministic base; Backuper normally supplies a fresh suffix to avoid completed-Job collisions.
	JobName        string
	FormerOwner    string
	WorldPVC       string
	BackupPVC      string
	Namespace      string
	ServiceAccount string
	Image          string
	ConfigSecret   string
	ConfigMount    string
	BackupRoot     string
	WorldsRoot     string
	Deadline       time.Duration
	CPULimit       string
	MemLimit       string
	RunAsUser      int64
	RunAsGroup     int64
	FSGroup        int64

	// RestoreRef and RestoreBackupID mark a pending safety-snapshot chain and protect the archive needed by its follow-up restore.
	RestoreRef      string
	RestoreBackupID string
	// Live copies a read-only world to scratch before archival; StageLimit empty leaves scratch capacity uncapped.
	Live       bool
	StageLimit string

	// Scheduled uses independent retention and cannot carry a restore.
	Scheduled bool
	// Replica cannot carry a restore.
	Replica bool

	TTLAfterFinished time.Duration
}

func BackupJobName(server string) string { return "backup-" + server }

func PodSelector() map[string]string {
	return map[string]string{
		LabelManagedBy: managedByValue,
		LabelComponent: componentValue,
	}
}

func backupLabels(p JobParams) map[string]string {
	labels := PodSelector()
	labels[LabelServer] = p.Server
	return labels
}

// Backup Jobs mount database config to record archives; restore Jobs remain credential-blind because they extract untrusted archives.
func BackupJob(p JobParams) (*batchv1.Job, error) {
	if p.Image == "" {
		return nil, fmt.Errorf("backup: image is empty")
	}
	if p.WorldPVC == "" || p.BackupPVC == "" {
		return nil, fmt.Errorf("backup: world and backup PVC names are required")
	}
	if p.ConfigSecret == "" {
		return nil, fmt.Errorf("backup: config secret name is required")
	}
	if p.RestoreRef != "" && p.RestoreBackupID == "" {
		return nil, fmt.Errorf("backup: a chained restore needs the backup id")
	}
	if p.RestoreRef != "" && p.Scheduled {
		return nil, fmt.Errorf("backup: a scheduled backup carries no restore")
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

	args := []string{
		"--config", p.ConfigMount + "/felis.toml",
		"--server", p.Server,
		"--worlds-root", p.WorldsRoot,
	}
	if p.FormerOwner != "" {
		args = append(args, "--former-owner", p.FormerOwner)
	}
	if p.RestoreRef != "" {
		args = append(args, "--reason", ReasonPreRestore, "--protect", p.RestoreBackupID)
	}
	if p.Scheduled {
		args = append(args, "--reason", ReasonScheduled)
	}
	if p.Replica {
		args = append(args, "--reason", ReasonReplica)
	}

	container := corev1.Container{
		Name:    "backup",
		Image:   p.Image,
		Command: []string{felisBinaryPath, "backup"},
		Args:    args,
		VolumeMounts: []corev1.VolumeMount{
			// Read-only world mounts prevent backup processes from modifying the source.
			{Name: worldVolume, MountPath: p.WorldsRoot, ReadOnly: true},
			{Name: backupVolume, MountPath: p.BackupRoot},
			{Name: configVolume, MountPath: p.ConfigMount, ReadOnly: true},
			{Name: tmpVolume, MountPath: "/tmp"},
		},
		Resources: corev1.ResourceRequirements{Limits: limits, Requests: jobRequests(limits)},
		SecurityContext: &corev1.SecurityContext{
			Privileged:               boolPtr(false),
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			// DAC_OVERRIDE permits reading mode-0600 world files regardless of the game UID.
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"DAC_OVERRIDE"},
			},
		},
	}

	container.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError

	name := p.JobName
	if name == "" {
		name = BackupJobName(p.Server)
	}
	meta := metav1.ObjectMeta{
		Name:      name,
		Namespace: p.Namespace,
		Labels:    backupLabels(p),
	}
	if p.RestoreRef != "" {
		// Only the Job needs the chain label because the API patches it after settlement.
		meta.Labels[labelThenRestore] = thenRestorePending
		meta.Annotations = map[string]string{
			annotationRestoreRef:      p.RestoreRef,
			annotationRestoreBackupID: p.RestoreBackupID,
		}
	}
	if p.Scheduled {
		meta.Labels[LabelReason] = ReasonScheduled
	}
	podLabels := backupLabels(p)
	volumes := []corev1.Volume{
		{
			Name: worldVolume,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: p.WorldPVC,
					ReadOnly:  true,
				},
			},
		},
		{
			Name: backupVolume,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: p.BackupPVC,
				},
			},
		},
		{
			Name: configVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: p.ConfigSecret},
			},
		},
		{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	job := &batchv1.Job{
		ObjectMeta: meta,
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   int64Ptr(deadline),
			TTLSecondsAfterFinished: int32Ptr(ttl),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           p.ServiceAccount,
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext:              backupPodSecurityContext(p),
					Containers:                   []corev1.Container{container},
					Volumes:                      volumes,
				},
			},
		},
	}
	if p.Live {
		if err := MakeLive(job, worldVolume, p.WorldsRoot, p.Image, p.StageLimit); err != nil {
			return nil, err
		}
	}
	return job, nil
}

// jobRequests reserves what a world copy needs, so the job schedules beside a server on a small host; the limits still bound it.
func jobRequests(limits corev1.ResourceList) corev1.ResourceList {
	requests := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}
	for name, limit := range limits {
		if limit.Cmp(requests[name]) < 0 {
			requests[name] = limit
		}
	}
	return requests
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
		return nil, fmt.Errorf("backup: invalid cpu limit %q: %w", cpu, err)
	}
	memQty, err := resource.ParseQuantity(mem)
	if err != nil {
		return nil, fmt.Errorf("backup: invalid memory limit %q: %w", mem, err)
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:    cpuQty,
		corev1.ResourceMemory: memQty,
	}, nil
}

func boolPtr(b bool) *bool    { return &b }
func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }

// backupPodSecurityContext defaults to root and omits FSGroup unless configured to avoid a volume-wide ownership change.
func backupPodSecurityContext(p JobParams) *corev1.PodSecurityContext {
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

// MakeLive replaces archival input with a staged copy and marks the Job so recovery knows when game saving can resume.
func MakeLive(job *batchv1.Job, worldVol, worldsRoot, image, stageLimit string) error {
	spec := &job.Spec.Template.Spec
	if len(spec.Containers) == 0 {
		return fmt.Errorf("backup: a live backup needs the archive container")
	}
	archive := &spec.Containers[0]
	found := false
	for i, m := range archive.VolumeMounts {
		if m.Name == worldVol {
			archive.VolumeMounts[i] = corev1.VolumeMount{Name: stageVolume, MountPath: worldsRoot, ReadOnly: true}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("backup: the archive container does not mount %s", worldVol)
	}
	scratch := &corev1.EmptyDirVolumeSource{}
	if stageLimit != "" {
		q, err := resource.ParseQuantity(stageLimit)
		if err != nil {
			return fmt.Errorf("backup: stage limit %q: %w", stageLimit, err)
		}
		scratch.SizeLimit = &q
	}
	spec.InitContainers = append(spec.InitContainers, corev1.Container{
		Name:    StageContainer,
		Image:   image,
		Command: []string{felisBinaryPath, "stage-world"},
		Args:    []string{"--from", liveMountPath, "--to", worldsRoot},
		VolumeMounts: []corev1.VolumeMount{
			{Name: worldVol, MountPath: liveMountPath, ReadOnly: true},
			{Name: stageVolume, MountPath: worldsRoot},
		},
		Resources:                archive.Resources,
		SecurityContext:          archive.SecurityContext,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	})
	spec.Volumes = append(spec.Volumes, corev1.Volume{Name: stageVolume, VolumeSource: corev1.VolumeSource{EmptyDir: scratch}})
	if job.Labels == nil {
		job.Labels = map[string]string{}
	}
	if job.Spec.Template.Labels == nil {
		job.Spec.Template.Labels = map[string]string{}
	}
	job.Labels[LabelLive] = "true"
	job.Spec.Template.Labels[LabelLive] = "true"
	return nil
}
