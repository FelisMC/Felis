package restore

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func sampleJobParams() JobParams {
	return JobParams{
		Server:           "survival",
		WorldPVC:         "world-survival-0",
		BackupPVC:        "felis-backups",
		BackupRef:        "/backups/survival/2026-06-25.tar.gz",
		ArchiveStore:     "tarLocal",
		Namespace:        defaultNamespace,
		ServiceAccount:   defaultServiceAccount,
		Image:            "registry.felis.svc:5000/felis:1.0",
		BackupRoot:       "/backups",
		WorldsRoot:       "/world",
		Deadline:         30 * time.Minute,
		CPULimit:         "1",
		MemLimit:         "1Gi",
		RunAsUser:        0,
		RunAsGroup:       0,
		FSGroup:          0,
		TTLAfterFinished: 10 * time.Minute,
	}
}

func TestRestoreJobRunsUnderWeakSA(t *testing.T) {
	job, err := RestoreJob(sampleJobParams())
	if err != nil {
		t.Fatalf("RestoreJob: %v", err)
	}
	sa := job.Spec.Template.Spec.ServiceAccountName
	if sa != defaultServiceAccount {
		t.Errorf("service account = %q, want %q", sa, defaultServiceAccount)
	}
	if sa == "felis-api" {
		t.Fatal("restore Pod must NOT run as the felis-api SA")
	}
	if amt := job.Spec.Template.Spec.AutomountServiceAccountToken; amt == nil || *amt {
		t.Error("AutomountServiceAccountToken must be explicitly false")
	}
}

func TestRestoreJobMountsOnlyTheTwoPVCsAndNoSecrets(t *testing.T) {
	job, err := RestoreJob(sampleJobParams())
	if err != nil {
		t.Fatalf("RestoreJob: %v", err)
	}
	vols := job.Spec.Template.Spec.Volumes
	if len(vols) != 2 {
		t.Fatalf("expected exactly 2 volumes (world + backup), got %d: %+v", len(vols), vols)
	}
	var world, backup *corev1.Volume
	for i := range vols {
		v := &vols[i]
		// The forbidden volume kinds: anything that could carry DB creds or
		// reach the API.
		if v.Secret != nil {
			t.Errorf("volume %q is a Secret — a restore Pod must never mount a Secret", v.Name)
		}
		if v.ConfigMap != nil {
			t.Errorf("volume %q is a ConfigMap — no config/credential injection allowed", v.Name)
		}
		if v.Projected != nil || v.DownwardAPI != nil {
			t.Errorf("volume %q is a projected/downward volume — could surface the SA token", v.Name)
		}
		if v.HostPath != nil {
			t.Errorf("volume %q is a hostPath — no node filesystem access allowed", v.Name)
		}
		if v.PersistentVolumeClaim == nil {
			t.Errorf("volume %q is not a PVC; only the world and backup PVCs are permitted", v.Name)
			continue
		}
		switch v.PersistentVolumeClaim.ClaimName {
		case "world-survival-0":
			world = v
		case "felis-backups":
			backup = v
		default:
			t.Errorf("unexpected PVC %q mounted", v.PersistentVolumeClaim.ClaimName)
		}
	}
	if world == nil {
		t.Fatal("world PVC not mounted")
	}
	if backup == nil {
		t.Fatal("backup PVC not mounted")
	}
	// The backup PVC must be read-only at the volume source: a restore must not
	// be able to mutate the archive store.
	if !backup.PersistentVolumeClaim.ReadOnly {
		t.Error("backup PVC volume source must be ReadOnly")
	}

	// And the container's mounts must agree: backup read-only, world writable.
	c := singleContainer(t, job)
	var backupMount, worldMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		m := &c.VolumeMounts[i]
		switch m.Name {
		case backupVolume:
			backupMount = m
		case worldVolume:
			worldMount = m
		}
	}
	if backupMount == nil || !backupMount.ReadOnly {
		t.Error("backup mount must be ReadOnly")
	}
	if worldMount == nil || worldMount.ReadOnly {
		t.Error("world mount must be writable (the archive extracts into it)")
	}
	if backupMount != nil && backupMount.MountPath != "/backups" {
		t.Errorf("backup mount path = %q, want /backups (absolute refs resolve here)", backupMount.MountPath)
	}

	// Require empty Env and EnvFrom: restore inputs come only from flags, without credentials.
	if len(c.Env) != 0 {
		t.Errorf("restore container must carry no env vars, got %+v", c.Env)
	}
	if len(c.EnvFrom) != 0 {
		t.Errorf("restore container must carry no envFrom sources (no Secret/ConfigMap injection), got %+v", c.EnvFrom)
	}
}

func TestRestoreJobIsBoundedOneShotAndSelfCleaning(t *testing.T) {
	job, err := RestoreJob(sampleJobParams())
	if err != nil {
		t.Fatalf("RestoreJob: %v", err)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("BackoffLimit must be 0 — a bad archive must not retry")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 1800 {
		t.Errorf("ActiveDeadlineSeconds must be 1800, got %v", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 600 {
		t.Errorf("TTLSecondsAfterFinished must be 600, got %v", job.Spec.TTLSecondsAfterFinished)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("RestartPolicy must be Never")
	}
}

func TestRestoreJobContainerIsHardened(t *testing.T) {
	job, err := RestoreJob(sampleJobParams())
	if err != nil {
		t.Fatalf("RestoreJob: %v", err)
	}
	pod := job.Spec.Template.Spec
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || *pod.SecurityContext.RunAsNonRoot {
		t.Error("pod must NOT require non-root: root is the owner-matching default for game-image worlds")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != 0 {
		t.Error("pod must run as uid 0 by default")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroup != nil {
		t.Error("fsGroup must stay unset when zero (a root executor must not chgrp the world volume)")
	}
	c := singleContainer(t, job)
	sc := c.SecurityContext
	if sc == nil {
		t.Fatal("container has no security context")
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Error("container must not be privileged")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("container must set allowPrivilegeEscalation=false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("container must set readOnlyRootFilesystem=true (writes go only to the world PVC)")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 || string(sc.Capabilities.Drop[0]) != "ALL" {
		t.Errorf("container must drop ALL capabilities, got %v", sc.Capabilities)
	}
	if len(sc.Capabilities.Add) != 1 || sc.Capabilities.Add[0] != "DAC_OVERRIDE" {
		t.Errorf("container must add exactly DAC_OVERRIDE, got %v", sc.Capabilities.Add)
	}
	if c.Resources.Limits.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
		t.Error("container must carry CPU+memory limits")
	}
}

func TestRestoreJobInvokesFelisRestoreWithParams(t *testing.T) {
	p := sampleJobParams()
	job, err := RestoreJob(p)
	if err != nil {
		t.Fatalf("RestoreJob: %v", err)
	}
	c := singleContainer(t, job)
	if len(c.Command) < 2 || c.Command[0] != felisBinaryPath || c.Command[1] != "restore" {
		t.Errorf("command = %v, want [%s restore ...]", c.Command, felisBinaryPath)
	}
	if !argPairPresent(c.Args, "--server", p.Server) {
		t.Errorf("args must carry --server %q, got %v", p.Server, c.Args)
	}
	if !argPairPresent(c.Args, "--ref", p.BackupRef) {
		t.Errorf("args must carry --ref %q, got %v", p.BackupRef, c.Args)
	}
	if got := job.Annotations[AnnotationBackupRef]; got != p.BackupRef {
		t.Errorf("backup-ref annotation = %q, want %q", got, p.BackupRef)
	}
	if !argPairPresent(c.Args, "--archive-store", p.ArchiveStore) {
		t.Errorf("args must carry --archive-store %q, got %v", p.ArchiveStore, c.Args)
	}
	if !argPairPresent(c.Args, "--backup-root", p.BackupRoot) {
		t.Errorf("args must carry --backup-root %q, got %v", p.BackupRoot, c.Args)
	}
	if !argPairPresent(c.Args, "--worlds-root", p.WorldsRoot) {
		t.Errorf("args must carry --worlds-root %q, got %v", p.WorldsRoot, c.Args)
	}
	if job.Name != "restore-survival" {
		t.Errorf("job name = %q, want restore-survival (deterministic for idempotency)", job.Name)
	}
}

func TestRestoreJobRequiresImage(t *testing.T) {
	p := sampleJobParams()
	p.Image = ""
	if _, err := RestoreJob(p); err == nil {
		t.Error("expected error for an empty image")
	}
}

func TestRestoreJobRejectsBadResourceLimit(t *testing.T) {
	p := sampleJobParams()
	p.MemLimit = "not-a-quantity"
	if _, err := RestoreJob(p); err == nil {
		t.Error("expected error for an unparseable memory limit")
	}
}

func TestRestoreServiceAccountIsBare(t *testing.T) {
	sa := RestoreServiceAccount(defaultNamespace, defaultServiceAccount)
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("SA must disable token automounting")
	}
	if len(sa.Secrets) != 0 {
		t.Errorf("SA must carry no secrets, got %d", len(sa.Secrets))
	}
	if len(sa.ImagePullSecrets) != 0 {
		t.Errorf("SA must carry no image-pull secrets, got %d", len(sa.ImagePullSecrets))
	}
}

func singleContainer(t *testing.T, job *batchv1.Job) corev1.Container {
	t.Helper()
	cs := job.Spec.Template.Spec.Containers
	if len(cs) != 1 {
		t.Fatalf("expected exactly one restore container, got %d", len(cs))
	}
	return cs[0]
}

func argPairPresent(args []string, flag, val string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}
