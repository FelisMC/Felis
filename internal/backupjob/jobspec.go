package backupjob

import (
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Label keys applied to backup objects, mirroring internal/restore so the two
// executors are observable the same way.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelServer    = "felis.lolicon.best/server"

	managedByValue = "felis-backup"
	componentValue = "world-backup"

	worldVolume     = "world"
	backupVolume    = "backup"
	configVolume    = "config"
	tmpVolume       = "tmp"
	felisBinaryPath = "/usr/local/bin/felis"
)

// JobParams are the rendered inputs to a backup Job, derived from a server +
// former owner + Config by the Backuper. jobspec is a pure function of them so
// the security-critical Job shape is unit-tested without a cluster.
type JobParams struct {
	Server string
	// JobName is the unique Job object name for this invocation. The Backuper sets a
	// fresh per-call name (BackupJobName(server) + random suffix) so every on-demand
	// request produces its own archive — a deterministic name would collide with a
	// just-finished Job still inside its TTL window and silently no-op the retry.
	// Empty falls back to the deterministic BackupJobName (tests, and any direct call).
	JobName        string
	FormerOwner    string // recorded on the world_backups row so the owner can later restore it
	WorldPVC       string
	BackupPVC      string
	Namespace      string
	ServiceAccount string
	Image          string
	ConfigSecret   string // the felis config Secret mounted for the DB URL (self-recording)
	ConfigMount    string // where the config Secret is mounted (holds felis.toml)
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

// BackupJobName is the base Job name for a server's on-demand backup — the prefix
// the Backuper extends with a per-invocation random suffix (JobParams.JobName) so
// repeat backups do not collide. It is distinct from the restore Job name so a
// backup and a restore of the same server never collide.
func BackupJobName(server string) string { return "backup-" + server }

func backupLabels(p JobParams) map[string]string {
	return map[string]string{
		LabelManagedBy: managedByValue,
		LabelComponent: componentValue,
		LabelServer:    p.Server,
	}
}

// BackupJob renders the on-demand world-backup Job (spec §18/§19 WorldArchiver,
// run on demand rather than on the reaper's daily schedule). Its isolation mirrors
// the restore Job (weak SA, non-root, read-only root fs, drop ALL, one-shot with a
// deadline) with ONE deliberate, security-reviewed departure asserted by
// jobspec_test.go:
//
//   - It DOES mount the felis config Secret (read-only), because a backup must
//     self-record its world_backups row — archive-then-insert atomically, exactly
//     like the reaper, which is the only other component holding both a world mount
//     and the database. A restore Pod must never touch the DB because it processes a
//     potentially poisoned archive; a backup Pod only READS a world the operator
//     already owns and tars it (bytes, never executed), so the poisoned-input threat
//     that forbids restore's DB access does not apply. Its blast radius is the DB
//     plus the two PVCs, matching the reaper's trust for a strict subset of the
//     reaper's operations (it never deletes a PVC and never calls the K8s API — its
//     SA token stays un-mounted).
//   - The world PVC is mounted READ-ONLY (backup only reads it; the RWO volume must
//     be free, which the handler's stopped-gate guarantees), and the backup PVC
//     READ-WRITE (the archive is written into it) — the mirror image of restore.
//
// The container runs `/usr/local/bin/felis backup` (cmd/felis), which tars the
// world at WorldsRoot into BackupRoot and inserts the world_backups row. BackupRoot
// MUST equal the reaper's [archive] local_path so the recorded absolute ref
// resolves the same way a later restore Job mounts it.
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
	// FormerOwner is optional: an admin backing up an unowned (released) server
	// records an empty former_owner, exactly as the reaper does for an unowned reap.
	if p.FormerOwner != "" {
		args = append(args, "--former-owner", p.FormerOwner)
	}

	container := corev1.Container{
		Name:    "backup",
		Image:   p.Image,
		Command: []string{felisBinaryPath, "backup"},
		Args:    args,
		VolumeMounts: []corev1.VolumeMount{
			// The world is only read to tar it; mounting it read-only means the
			// backup process can never mutate the world it snapshots.
			{Name: worldVolume, MountPath: p.WorldsRoot, ReadOnly: true},
			{Name: backupVolume, MountPath: p.BackupRoot},
			{Name: configVolume, MountPath: p.ConfigMount, ReadOnly: true},
			{Name: tmpVolume, MountPath: "/tmp"},
		},
		Resources: corev1.ResourceRequirements{Limits: limits, Requests: limits},
		SecurityContext: &corev1.SecurityContext{
			Privileged:               boolPtr(false),
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			// DAC_OVERRIDE is granted on top of dropping ALL: the pod runs as root,
			// but the world may have been written by a game image whose UID is
			// neither root nor ours, and Paper's own files are mode 0600. It is the
			// minimal extra power that makes the archive read every world shape.
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"DAC_OVERRIDE"},
			},
		},
	}

	name := p.JobName
	if name == "" {
		name = BackupJobName(p.Server)
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.Namespace,
			Labels:    backupLabels(p),
		},
		Spec: batchv1.JobSpec{
			// One shot: a wedged archive must not loop. The TTL GCs the finished Job
			// so a later backup of the same server is not blocked forever by a stale
			// completed Job.
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   int64Ptr(deadline),
			TTLSecondsAfterFinished: int32Ptr(ttl),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: backupLabels(p)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           p.ServiceAccount,
					AutomountServiceAccountToken: boolPtr(false),
					// Root by default (see Config.RunAsUser): the world volume's
					// owner is the game uid, so only an owner-matching or
					// DAC-overriding uid can read it. FSGroup is omitted when unset
					// so a root pod never triggers a volume chgrp.
					SecurityContext: backupPodSecurityContext(p),
					Containers:      []corev1.Container{container},
					Volumes: []corev1.Volume{
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
					},
				},
			},
		},
	}
	return job, nil
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

// backupPodSecurityContext pins the Pod identity. RunAsNonRoot is false because
// the default identity is root: worlds are owned by the game uid (or root, for a
// world an older release wrote), and Paper writes mode-0600 files any other
// non-root reader cannot open. FSGroup stays unset unless configured — a root executor must not
// needlessly chgrp the world volume.
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
