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

const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelServer    = "felis.lolicon.best/server"
	// LabelMode distinguishes exports that hold the world volume from backup exports.
	LabelMode = "felis.lolicon.best/export-mode"

	managedByValue = "felis-export"
	componentValue = "world-export"

	worldVolume     = "world"
	backupVolume    = "backup"
	containerName   = "export"
	felisBinaryPath = "/usr/local/bin/felis"
)

const (
	ModeWorld  = "world"
	ModeBackup = "backup"
	ModeFiles  = "files"
)

// TokenEnv keeps the one-time upload token out of argv.
const TokenEnv = "FELIS_EXPORT_TOKEN"

// DigestTrailer carries the streamed SHA-256 (RFC 9530); LengthHeader supplies a known content length.
const (
	DigestTrailer = "Content-Digest"
	LengthHeader  = "X-Felis-Export-Length"
)

type JobParams struct {
	NodeName string
	Server   string
	ID       string
	Mode     string
	// WorldPVC serves world/files exports; BackupPVC and BackupRef serve backup exports.
	WorldPVC  string
	BackupPVC string
	BackupRef string
	// BackupSHA256 validates the source archive before secrets are removed for export.
	BackupSHA256 string
	Path         string
	Dir          bool
	TargetURL    string
	Token        string

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

func JobName(server, id string) string { return "export-" + server + "-" + id }

func exportLabels(p JobParams) map[string]string {
	return map[string]string{
		LabelManagedBy: managedByValue,
		LabelComponent: componentValue,
		LabelServer:    p.Server,
		LabelMode:      p.Mode,
	}
}

// ExportJob mounts one volume read-only, disables API credentials, and permits no retry of the single-use token.
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
		// The terminated state exposes failures through the export status and server jobs API.
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
