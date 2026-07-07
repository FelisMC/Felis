package backupjob

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func sampleJobParams() JobParams {
	return JobParams{
		Server:           "survival",
		FormerOwner:      "usr-abc",
		WorldPVC:         "world-survival-0",
		BackupPVC:        "felis-backups",
		Namespace:        defaultNamespace,
		ServiceAccount:   defaultServiceAccount,
		Image:            "registry.felis.svc:5000/felis:1.0",
		ConfigSecret:     defaultConfigSecret,
		ConfigMount:      defaultConfigMount,
		BackupRoot:       "/backups",
		WorldsRoot:       "/world",
		Deadline:         30 * time.Minute,
		CPULimit:         "1",
		MemLimit:         "1Gi",
		RunAsUser:        1000,
		RunAsGroup:       1000,
		FSGroup:          1000,
		TTLAfterFinished: 10 * time.Minute,
	}
}

// The backup Pod runs under the weak felis-restore SA — never the felis-api
// identity — with its token un-mounted, so it cannot reach the K8s API. Its DB
// access comes from the mounted config Secret, not from any SA permission.
func TestBackupJobRunsUnderWeakSAWithNoAPIToken(t *testing.T) {
	job, err := BackupJob(sampleJobParams())
	if err != nil {
		t.Fatalf("BackupJob: %v", err)
	}
	sa := job.Spec.Template.Spec.ServiceAccountName
	if sa != defaultServiceAccount {
		t.Errorf("service account = %q, want %q", sa, defaultServiceAccount)
	}
	if sa == "felis-api" {
		t.Fatal("backup Pod must NOT run as the felis-api SA")
	}
	if amt := job.Spec.Template.Spec.AutomountServiceAccountToken; amt == nil || *amt {
		t.Error("AutomountServiceAccountToken must be explicitly false")
	}
}

// The deliberate departure from restore's zero-secret isolation: a backup Pod
// mounts EXACTLY the two PVCs (world read-only, backup read-write) PLUS the config
// Secret read-only (so it can self-record its world_backups row like the reaper) —
// and nothing else. This test freezes that exact volume set so a future edit that
// widens it (e.g. a second Secret, or a writable world mount) fails loudly.
func TestBackupJobMountsTwoPVCsPlusConfigSecretOnly(t *testing.T) {
	job, err := BackupJob(sampleJobParams())
	if err != nil {
		t.Fatalf("BackupJob: %v", err)
	}
	spec := job.Spec.Template.Spec

	var secretVols, pvcVols int
	for _, v := range spec.Volumes {
		switch {
		case v.Secret != nil:
			secretVols++
			if v.Secret.SecretName != defaultConfigSecret {
				t.Errorf("secret volume = %q, want the config secret %q", v.Secret.SecretName, defaultConfigSecret)
			}
		case v.PersistentVolumeClaim != nil:
			pvcVols++
		case v.EmptyDir != nil:
			// the /tmp scratch dir under the read-only root fs — allowed
		default:
			t.Errorf("unexpected volume %q: a backup Pod mounts only the two PVCs, the config Secret, and a /tmp emptyDir", v.Name)
		}
	}
	if secretVols != 1 {
		t.Errorf("secret volumes = %d, want exactly 1 (the config Secret)", secretVols)
	}
	if pvcVols != 2 {
		t.Errorf("PVC volumes = %d, want exactly 2 (world + backup)", pvcVols)
	}

	// World read-only (backup never mutates the world), backup read-write (the
	// archive is written into it) — the mirror image of the restore Job.
	world := mountByName(t, spec.Containers[0].VolumeMounts, worldVolume)
	if !world.ReadOnly {
		t.Error("world mount must be read-only — a backup only reads the world")
	}
	back := mountByName(t, spec.Containers[0].VolumeMounts, backupVolume)
	if back.ReadOnly {
		t.Error("backup mount must be read-write — the archive is written into it")
	}
	cfg := mountByName(t, spec.Containers[0].VolumeMounts, configVolume)
	if !cfg.ReadOnly {
		t.Error("config Secret mount must be read-only")
	}

	// The world PVC volume itself is also declared read-only so the RWO claim is
	// requested read-only (defense in depth beyond the mount flag).
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "world-survival-0" && !v.PersistentVolumeClaim.ReadOnly {
			t.Error("world PVC volume source must be read-only")
		}
	}
}

// The backup container is hardened exactly like the restore/build Job containers:
// no privilege, no escalation, read-only root fs, drop ALL capabilities.
func TestBackupJobContainerIsHardened(t *testing.T) {
	job, err := BackupJob(sampleJobParams())
	if err != nil {
		t.Fatalf("BackupJob: %v", err)
	}
	sc := job.Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("container SecurityContext is nil")
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Error("Privileged must be false")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Error("capabilities must drop ALL")
	}
}

// One-shot: a wedged archive must not loop, and a deadline caps it.
func TestBackupJobIsOneShotWithDeadline(t *testing.T) {
	job, err := BackupJob(sampleJobParams())
	if err != nil {
		t.Fatalf("BackupJob: %v", err)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("BackoffLimit must be 0 (no retry loop)")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds <= 0 {
		t.Error("ActiveDeadlineSeconds must be set")
	}
	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Error("TTLSecondsAfterFinished must be set so the finished Job is GC'd")
	}
}

// The command carries the former owner so the recorded backup can be restored by
// its owner; an empty former owner (admin backing up an unowned server) omits it.
func TestBackupJobArgsCarryServerAndOwner(t *testing.T) {
	job, err := BackupJob(sampleJobParams())
	if err != nil {
		t.Fatalf("BackupJob: %v", err)
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	if !argsContain(args, "--server", "survival") {
		t.Errorf("args missing --server survival: %v", args)
	}
	if !argsContain(args, "--former-owner", "usr-abc") {
		t.Errorf("args missing --former-owner usr-abc: %v", args)
	}

	p := sampleJobParams()
	p.FormerOwner = ""
	unowned, err := BackupJob(p)
	if err != nil {
		t.Fatalf("BackupJob(unowned): %v", err)
	}
	for _, a := range unowned.Spec.Template.Spec.Containers[0].Args {
		if a == "--former-owner" {
			t.Error("--former-owner must be omitted when there is no former owner")
		}
	}
}

func TestBackupJobRejectsMissingInputs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mut   func(*JobParams)
	}{
		{"no image", func(p *JobParams) { p.Image = "" }},
		{"no world pvc", func(p *JobParams) { p.WorldPVC = "" }},
		{"no backup pvc", func(p *JobParams) { p.BackupPVC = "" }},
		{"no config secret", func(p *JobParams) { p.ConfigSecret = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := sampleJobParams()
			tc.mut(&p)
			if _, err := BackupJob(p); err == nil {
				t.Errorf("BackupJob(%s) = nil error, want a validation error", tc.name)
			}
		})
	}
}

func mountByName(t *testing.T, mounts []corev1.VolumeMount, name string) corev1.VolumeMount {
	t.Helper()
	for _, m := range mounts {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("volume mount %q not found", name)
	return corev1.VolumeMount{}
}

func argsContain(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}
