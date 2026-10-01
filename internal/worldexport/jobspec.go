package worldexport

import (
	"fmt"
	"time"

	"felis.lolicon.best/internal/placement"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Label keys applied to export objects, the same ones the other executors use
// (internal/maintenance and internal/api keep their own copies;
// maintenance_test pins them against these).
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelServer    = "felis.lolicon.best/server"
	// LabelMode is what the Job sends (ModeWorld, ModeBackup or ModeFiles). A
	// world or files export holds the world volume; a backup export does not.
	LabelMode = "felis.lolicon.best/export-mode"

	managedByValue = "felis-export"
	componentValue = "world-export"

	worldVolume     = "world"
	backupVolume    = "backup"
	containerName   = "export"
	felisBinaryPath = "/usr/local/bin/felis"
)

// What an export sends: the whole world as a tar.gz, one stored backup as a
// tar.gz, or one file or folder of the world (a folder as a zip).
const (
	ModeWorld  = "world"
	ModeBackup = "backup"
	ModeFiles  = "files"
)

// TokenEnv carries the one-time upload token into the Pod. It is the only
// secret the Pod holds, so it rides the environment and not argv, where a
// process listing on the node would show it.
const TokenEnv = "FELIS_EXPORT_TOKEN"

// The Job's PUT goes chunked, so that it can end with a trailer: DigestTrailer
// carries the SHA-256 of every byte it sent (sha-256=:<base64>:, RFC 9530),
// which felis-api checks before the last of them reaches the browser.
// LengthHeader declares the length up front when the Job knows it (one file),
// which chunked encoding cannot carry, so the browser still gets a
// Content-Length.
const (
	DigestTrailer = "Content-Digest"
	LengthHeader  = "X-Felis-Export-Length"
)

// JobParams are the rendered inputs to an export Job. ExportJob is a pure
// function of them, so the Job shape is unit-tested without a cluster.
type JobParams struct {
	NodeName string
	Server   string
	// ID names this export: it is the tail of the Job name and of the internal
	// upload path, so two exports of one server never collide.
	ID   string
	Mode string
	// WorldPVC is mounted for ModeWorld and ModeFiles, BackupPVC and BackupRef
	// for ModeBackup.
	WorldPVC  string
	BackupPVC string
	BackupRef string
	// BackupSHA256 is what the stored archive must hash to. The Job checks it
	// itself, because what it sends is the archive re-written without its
	// secrets and no longer hashes to anything felis-api knows.
	BackupSHA256 string
	// Path is the file or folder a ModeFiles export sends, and Dir whether the
	// caller saw a folder there.
	Path string
	Dir  bool
	// TargetURL is where the Pod PUTs the archive (felis-api's internal face),
	// and Token the one-time bearer token that opens it.
	TargetURL string
	Token     string

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

// JobName is the Job an export runs as.
func JobName(server, id string) string { return "export-" + server + "-" + id }

func exportLabels(p JobParams) map[string]string {
	return map[string]string{
		LabelManagedBy: managedByValue,
		LabelComponent: componentValue,
		LabelServer:    p.Server,
		LabelMode:      p.Mode,
	}
}

// ExportJob renders the export Job. Every isolation guarantee lives here and is
// asserted by jobspec_test.go:
//
//   - the weak felis-restore SA with its token auto-mount disabled, so the Pod
//     cannot reach the K8s API;
//   - EXACTLY one volume, read-only in the claim and in the mount: the world PVC
//     for a world export, the backup PVC for a backup export. No Secret or
//     ConfigMap: the one credential in the Pod is the upload token, which opens
//     this one export and nothing else;
//   - root with DAC_OVERRIDE and nothing more: the world is the game uid's
//     mode-0600 files, and Pod Security baseline, which the minecraft namespace
//     enforces, admits DAC_OVERRIDE but not the narrower DAC_READ_SEARCH. No
//     privilege or escalation, a read-only root filesystem;
//   - backoffLimit 0 (the token is spent by the first attempt, a retry could
//     only fail) and activeDeadlineSeconds, so a download left hanging cannot
//     hold the world forever; ttlSecondsAfterFinished GCs the finished Job.
//
// The container runs `/usr/local/bin/felis export` (cmd/felis). The backup ref is
// an absolute path, so the backup PVC is mounted at BackupRoot, the path the
// archives were written under, as the restore Job mounts it.
func ExportJob(p JobParams) (*batchv1.Job, error) {
	if p.Image == "" {
		return nil, fmt.Errorf("worldexport: image is empty")
	}
	if p.ID == "" || p.TargetURL == "" || p.Token == "" {
		return nil, fmt.Errorf("worldexport: an export needs an id, a target URL and a token")
	}
	var (
		args   = []string{"--mode", p.Mode, "--server", p.Server, "--target-url", p.TargetURL}
		volume corev1.Volume
		mount  corev1.VolumeMount
	)
	switch p.Mode {
	case ModeWorld, ModeFiles:
		if p.WorldPVC == "" {
			return nil, fmt.Errorf("worldexport: world PVC name is required")
		}
		args = append(args, "--worlds-root", p.WorldsRoot)
		if p.Mode == ModeFiles {
			if p.Path == "" {
				return nil, fmt.Errorf("worldexport: a files export needs a path")
			}
			args = append(args, "--path", p.Path)
			if p.Dir {
				args = append(args, "--dir")
			}
		}
		volume = readOnlyClaim(worldVolume, p.WorldPVC)
		mount = corev1.VolumeMount{Name: worldVolume, MountPath: p.WorldsRoot, ReadOnly: true}
	case ModeBackup:
		if p.BackupPVC == "" || p.BackupRef == "" {
			return nil, fmt.Errorf("worldexport: backup PVC name and archive ref are required")
		}
		args = append(args, "--ref", p.BackupRef, "--backup-root", p.BackupRoot)
		if p.BackupSHA256 != "" {
			args = append(args, "--sha256", p.BackupSHA256)
		}
		volume = readOnlyClaim(backupVolume, p.BackupPVC)
		mount = corev1.VolumeMount{Name: backupVolume, MountPath: p.BackupRoot, ReadOnly: true}
	default:
		return nil, fmt.Errorf("worldexport: unknown mode %q", p.Mode)
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
		Name:         containerName,
		Image:        p.Image,
		Command:      []string{felisBinaryPath, "export"},
		Args:         args,
		Env:          []corev1.EnvVar{{Name: TokenEnv, Value: p.Token}},
		VolumeMounts: []corev1.VolumeMount{mount},
		Resources:    corev1.ResourceRequirements{Limits: limits, Requests: limits},
		SecurityContext: &corev1.SecurityContext{
			Privileged:               boolPtr(false),
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"DAC_OVERRIDE"},
			},
		},
		// The exit error reaches the export's status and GET
		// /servers/{name}/jobs through the terminated state.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}

	sc := &corev1.PodSecurityContext{
		RunAsNonRoot: boolPtr(false),
		RunAsUser:    int64Ptr(p.RunAsUser),
		RunAsGroup:   int64Ptr(p.RunAsGroup),
	}
	if p.FSGroup > 0 {
		sc.FSGroup = int64Ptr(p.FSGroup)
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      JobName(p.Server, p.ID),
			Namespace: p.Namespace,
			Labels:    exportLabels(p),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   int64Ptr(deadline),
			TTLSecondsAfterFinished: int32Ptr(ttl),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: exportLabels(p)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					NodeSelector:                 jobNodeSelector(p.NodeName),
					ServiceAccountName:           p.ServiceAccount,
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext:              sc,
					Containers:                   []corev1.Container{container},
					Volumes:                      []corev1.Volume{volume},
				},
			},
		},
	}, nil
}

func readOnlyClaim(name, claim string) corev1.Volume {
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim, ReadOnly: true},
		},
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
		return nil, fmt.Errorf("worldexport: invalid cpu limit %q: %w", cpu, err)
	}
	memQty, err := resource.ParseQuantity(mem)
	if err != nil {
		return nil, fmt.Errorf("worldexport: invalid memory limit %q: %w", mem, err)
	}
	return corev1.ResourceList{corev1.ResourceCPU: cpuQty, corev1.ResourceMemory: memQty}, nil
}

func boolPtr(b bool) *bool    { return &b }
func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }

func jobNodeSelector(name string) map[string]string {
	if name == "" {
		return nil
	}
	return map[string]string{placement.LabelIdentity: name}
}
