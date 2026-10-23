package fileedit

import (
	"fmt"
	"strconv"
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
	LabelOpID      = "felis.lolicon.best/files-op"
	LabelMode      = "felis.lolicon.best/files-mode"
	LabelAsync     = "felis.lolicon.best/files-async"
	AnnotationPath = "felis.lolicon.best/files-path"

	managedByValue = "felis-files"
	componentValue = "world-files"

	worldVolume     = "world"
	felisBinaryPath = "/usr/local/bin/felis"
	containerName   = "files"
)

type JobParams struct {
	NodeName string
	Server   string
	OpID     string
	Op       string
	Path     string
	Query    string
	Paths    []string
	Content  []byte
	// Expect empty writes unconditionally; CreateOnly refuses existing paths.
	Expect     string
	CreateOnly bool
	To         string
	// UploadSize and UploadSHA256 must match fetched bytes; Overwrite permits replacing an existing file.
	SourceURL    string
	UploadToken  string
	UploadSize   int64
	UploadSHA256 string
	Overwrite    bool
	// Async marks uploads or unzips whose completion is read later through Ops.
	Async        bool
	WorldPVC     string
	BrowserURL   string
	BrowserToken string

	Namespace      string
	ServiceAccount string
	Image          string
	WorldsRoot     string
	Deadline       time.Duration
	CPULimit       string
	MemLimit       string
	RunAsUser      int64
	RunAsGroup     int64
	FSGroup        int64

	TTLAfterFinished time.Duration
}

// FilesJobName includes OpID to avoid collisions while completed Jobs await TTL cleanup; the API cannot delete them.
func FilesJobName(server, opID string) string { return "files-" + server + "-" + opID }

func filesLabels(p JobParams) map[string]string {
	l := map[string]string{
		LabelManagedBy: managedByValue,
		LabelComponent: componentValue,
		LabelServer:    p.Server,
		LabelOpID:      p.OpID,
		LabelMode:      p.Op,
	}
	if p.Async {
		l[LabelAsync] = "true"
	}
	return l
}

func FilesJob(p JobParams) (*batchv1.Job, error) {
	if p.BrowserURL != "" && (mutates(p.Op) || p.BrowserToken == "" || p.Async) {
		return nil, fmt.Errorf("fileedit: a browser must be read-only and have a token")
	}
	if p.Image == "" {
		return nil, fmt.Errorf("fileedit: image is empty")
	}
	if p.WorldPVC == "" {
		return nil, fmt.Errorf("fileedit: world PVC name is required")
	}
	if p.OpID == "" {
		return nil, fmt.Errorf("fileedit: op id is required")
	}
	if !validOp(p.Op) {
		return nil, fmt.Errorf("fileedit: unknown op %q", p.Op)
	}
	if len(p.Content) > MaxInlineWriteBytes {
		return nil, fmt.Errorf("fileedit: content is %d bytes, over the %d limit", len(p.Content), MaxInlineWriteBytes)
	}
	if (p.Op == OpPaperGlobalRead || p.Op == OpPaperGlobalPatch) && (p.Path != "" || p.To != "" || p.SourceURL != "" || len(p.Content) > MaxPaperGlobalPatchBytes) {
		return nil, fmt.Errorf("fileedit: invalid protected configuration operation")
	}
	if (p.Op == OpUpload || p.Op == OpWrite && p.SourceURL != "") && (p.SourceURL == "" || p.UploadToken == "") {
		return nil, fmt.Errorf("fileedit: an upload needs a source URL and a token")
	}
	if p.Async && p.Op != OpUpload && p.Op != OpUnzip && p.Op != OpCopy && p.Op != OpMove && p.Op != OpZip {
		return nil, fmt.Errorf("fileedit: this operation cannot run in the background, not %s", p.Op)
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

	// Mount read operations read-only so the kernel enforces their inability to alter the world.
	readOnlyWorld := !mutates(p.Op)

	args := []string{
		"--op", p.Op,
		"--path", p.Path,
		"--worlds-root", p.WorldsRoot,
	}
	// Expected hashes may travel in argv; file content uses the environment channel.
	switch p.Op {
	case OpWrite, OpPaperGlobalPatch:
		sum := digest(p.Content)
		if p.SourceURL != "" {
			sum = p.UploadSHA256
			args = append(args, "--source-url", p.SourceURL, "--size", strconv.FormatInt(p.UploadSize, 10))
		}
		args = append(args, "--sha256", sum)
		if p.Expect != "" {
			args = append(args, "--expect-sha256", p.Expect)
		}
		if p.CreateOnly {
			args = append(args, "--create-only")
		}
	case OpSearch:
		args = append(args, "--query", p.Query)
	case OpCopy, OpMove, OpZip:
		args = append(args, "--to", p.To)
		for _, name := range p.Paths {
			args = append(args, "--select", name)
		}
		if p.Overwrite {
			args = append(args, "--overwrite")
		}
	case OpRename:
		args = append(args, "--to", p.To)
	case OpUpload:
		// Only the upload token is secret and goes in the environment, unlike URL, size and digest.
		args = append(args, "--source-url", p.SourceURL,
			"--size", strconv.FormatInt(p.UploadSize, 10), "--sha256", p.UploadSHA256)
		if p.Overwrite {
			args = append(args, "--overwrite")
		}
	case OpUnzip:
		if p.Overwrite {
			args = append(args, "--overwrite")
		}
	}
	container := corev1.Container{
		Name:    containerName,
		Image:   p.Image,
		Command: []string{felisBinaryPath, "files"},
		Args:    args,
		VolumeMounts: []corev1.VolumeMount{
			{Name: worldVolume, MountPath: p.WorldsRoot, ReadOnly: readOnlyWorld},
		},
		Resources: corev1.ResourceRequirements{Limits: limits, Requests: limits},
		SecurityContext: &corev1.SecurityContext{
			Privileged:               boolPtr(false),
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  filesCapabilities(p.Op),
			},
		},
	}

	// Split base64 content across environment variables; write content and upload or browser tokens appear only in Jobs that need them.
	switch p.Op {
	case OpWrite, OpPaperGlobalPatch:
		if p.SourceURL != "" {
			container.Env = []corev1.EnvVar{{Name: UploadTokenEnv, Value: p.UploadToken}}
			break
		}
		parts := splitContent(p.Content)
		container.Env = []corev1.EnvVar{{Name: ContentPartsEnv, Value: strconv.Itoa(len(parts))}}
		for i, part := range parts {
			container.Env = append(container.Env, corev1.EnvVar{Name: contentPartEnv(i), Value: part})
		}
	case OpUpload:
		container.Env = []corev1.EnvVar{{Name: UploadTokenEnv, Value: p.UploadToken}}
	}
	if p.BrowserURL != "" {
		container.Args = []string{"--browse-url", p.BrowserURL, "--worlds-root", p.WorldsRoot}
		container.Env = []corev1.EnvVar{{Name: BrowserTokenEnv, Value: p.BrowserToken}}
		container.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")}
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        FilesJobName(p.Server, p.OpID),
			Namespace:   p.Namespace,
			Labels:      filesLabels(p),
			Annotations: filesAnnotations(p),
		},
		Spec: batchv1.JobSpec{
			// Disable retries so a failed write is not repeated behind the caller's request.
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   int64Ptr(deadline),
			TTLSecondsAfterFinished: int32Ptr(ttl),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: filesLabels(p)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					NodeSelector:                 jobNodeSelector(p.NodeName),
					ServiceAccountName:           p.ServiceAccount,
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext:              filesPodSecurityContext(p),
					Containers:                   []corev1.Container{container},
					Volumes: []corev1.Volume{{
						Name: worldVolume,
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: p.WorldPVC,
								ReadOnly:  readOnlyWorld,
							},
						},
					}},
				},
			},
		},
	}
	return job, nil
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
		return nil, fmt.Errorf("fileedit: invalid cpu limit %q: %w", cpu, err)
	}
	memQty, err := resource.ParseQuantity(mem)
	if err != nil {
		return nil, fmt.Errorf("fileedit: invalid memory limit %q: %w", mem, err)
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:    cpuQty,
		corev1.ResourceMemory: memQty,
	}, nil
}

func boolPtr(b bool) *bool    { return &b }
func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }

// filesCapabilities permits DAC_OVERRIDE for game-owned private files and CHOWN only for operations that create files.
func filesCapabilities(op string) []corev1.Capability {
	if op == OpWrite || op == OpPaperGlobalPatch || op == OpMkdir || op == OpUpload || op == OpUnzip || op == OpCopy || op == OpMove || op == OpZip {
		return []corev1.Capability{"CHOWN", "DAC_OVERRIDE"}
	}
	return []corev1.Capability{"DAC_OVERRIDE"}
}

// filesPodSecurityContext defaults to root for private game files and omits FSGroup unless configured to avoid changing volume ownership.
func filesPodSecurityContext(p JobParams) *corev1.PodSecurityContext {
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

func filesAnnotations(p JobParams) map[string]string {
	if !p.Async {
		return nil
	}
	return map[string]string{AnnotationPath: p.Path}
}

func jobNodeSelector(name string) map[string]string {
	if name == "" {
		return nil
	}
	return map[string]string{placement.LabelIdentity: name}
}
