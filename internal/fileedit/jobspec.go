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

// Label keys applied to file-editor objects, mirroring internal/restore and
// internal/backupjob so all three world-touching executors are observable the same
// way. LabelOpID is the one addition: it is how felis-api finds THIS invocation's
// Pod among any others, which matters here in a way it does not for restore —
// file operations are interactive and repeated, so several may be in flight or
// lingering inside their TTL at once. LabelMode carries the operation so the
// world-volume lock (internal/maintenance) can tell a write, which holds the
// volume, from a read or listing, which does not.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelServer    = "felis.lolicon.best/server"
	LabelOpID      = "felis.lolicon.best/files-op"
	LabelMode      = "felis.lolicon.best/files-mode"
	// LabelAsync marks the Job of an upload or unzip felis-api started and does
	// not wait on (Editor.StartUpload, Editor.StartUnzip); Ops finds them by it.
	// AnnotationPath names the file such a Job works on, for Ops to show.
	LabelAsync     = "felis.lolicon.best/files-async"
	AnnotationPath = "felis.lolicon.best/files-path"

	managedByValue = "felis-files"
	componentValue = "world-files"

	worldVolume     = "world"
	felisBinaryPath = "/usr/local/bin/felis"
	containerName   = "files"
)

// JobParams are the rendered inputs to a file-editor Job, derived from a server +
// operation + Config by the Editor. jobspec is a pure function of them so the
// security-critical Job shape is unit-tested without a cluster.
type JobParams struct {
	NodeName string
	Server   string
	// OpID is the per-invocation identifier that both names the Job and labels its
	// Pod. See Editor.run for why every invocation gets a fresh one.
	OpID    string
	Op      string
	Path    string
	Content []byte // OpWrite only
	// Expect is a write's precondition hash (see Execute); empty writes
	// unconditionally. CreateOnly makes a write refuse an existing path.
	Expect     string
	CreateOnly bool
	// To is a rename's destination.
	To string
	// SourceURL, UploadToken, UploadSize and UploadSHA256 tell an upload Job where
	// to fetch its bytes and what they must be (see Stage); Overwrite lets it
	// replace an existing file.
	SourceURL    string
	UploadToken  string
	UploadSize   int64
	UploadSHA256 string
	Overwrite    bool
	// Async marks a Job felis-api does not wait on: it carries LabelAsync and
	// AnnotationPath, which Ops reads it back by. Only an upload or an unzip
	// runs so.
	Async    bool
	WorldPVC string

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

// FilesJobName is the Job name for one file operation. Unlike RestoreJobName it is
// NOT a pure function of the server: it carries the per-invocation OpID.
//
// That difference is forced by RBAC and is the single most load-bearing decision in
// this package. felis-api holds `jobs: create` in the minecraft namespace and
// NOTHING else — no jobs:get, no jobs:delete (internal/platform.APIMinecraftRole).
// So it can neither poll a Job nor clean one up; ttlSecondsAfterFinished is the only
// reclamation. With a deterministic name the FIRST file operation would leave a
// completed Job squatting the name for the whole TTL window, and every subsequent
// operation would collide with it — and, unable to delete it, the editor would be
// wedged until the TTL expired. A restore can accept that (it runs once per
// incident); an editor cannot (browse a directory, open a file, save it — three
// operations in as many seconds). internal/backupjob reached the same conclusion for
// the same reason.
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

// FilesJob renders the file-editor Job. Its isolation is the strictest of the three
// world executors — a strict SUBSET of what a restore Pod gets — and every guarantee
// is asserted by jobspec_test.go, because no cluster runs in this environment:
//
//   - runs under the weak felis-restore SA (never the felis-api SA) with its token
//     auto-mount disabled, so it cannot reach the K8s API (spec §16, §21). It reuses
//     that bare, Role-less SA for the same reason internal/backupjob does: this Pod
//     needs no K8s API access at all, so a second identity with the same (empty)
//     powers would be a manifest to maintain for no isolation gain;
//   - mounts EXACTLY ONE volume — the world PVC — and NO Secret, NO ConfigMap, and
//     NO backup PVC. It is therefore strictly blinder than the backup Pod, which
//     mounts the config Secret to self-record its row: a file-editor Pod has nothing
//     to record, so it is handed no database URL and no credential of any kind (the
//     four-power red line, spec §22);
//   - mounts that one volume READ-ONLY for list and read (see mutates), so the
//     two operations that only look physically cannot change anything — the
//     kernel refuses, not merely the code. This is why readOnly is derived from the
//     op rather than fixed;
//   - runs as a non-root, fixed uid/gid with an fsGroup matching the operator's
//     StatefulSet, so a file this Pod writes is owned by the identity the minecraft
//     server later runs as — a config file the server cannot read would be worse
//     than no edit at all;
//   - no privilege, no privilege escalation, read-only root filesystem, drop ALL
//     capabilities. The world mount is the only writable path, and only for an op
//     that mutates;
//   - activeDeadlineSeconds + backoffLimit=0 so a wedged mount cannot loop or hang
//     forever; ttlSecondsAfterFinished GCs the finished Job, which — see
//     FilesJobName — is the ONLY cleanup available to felis-api.
//
// The container runs `/usr/local/bin/felis files` (cmd/felis), which performs the
// operation under os.Root containment and prints the marked JSON Result line that
// felis-api reads back through pods/log.
func FilesJob(p JobParams) (*batchv1.Job, error) {
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
	if len(p.Content) > MaxWriteBytes {
		return nil, fmt.Errorf("fileedit: content is %d bytes, over the %d limit", len(p.Content), MaxWriteBytes)
	}
	if p.Op == OpUpload && (p.SourceURL == "" || p.UploadToken == "") {
		return nil, fmt.Errorf("fileedit: an upload needs a source URL and a token")
	}
	if p.Async && p.Op != OpUpload && p.Op != OpUnzip {
		return nil, fmt.Errorf("fileedit: only an upload or an unzip runs in the background, not %s", p.Op)
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

	// Only an op that changes the world gets it read-write. Mounting read-only for
	// list and read makes "a listing cannot damage a world" a kernel guarantee
	// rather than a code-review one.
	readOnlyWorld := !mutates(p.Op)

	args := []string{
		"--op", p.Op,
		"--path", p.Path,
		"--worlds-root", p.WorldsRoot,
	}
	// The expected hash is a digest of content the caller already holds, not a
	// secret, so it rides argv; only the content itself needs the env channel.
	switch p.Op {
	case OpWrite:
		// The content's own SHA-256 goes beside it, so the Job writes only the
		// bytes felis-api handed over (Request.ContentSHA256).
		args = append(args, "--sha256", digest(p.Content))
		if p.Expect != "" {
			args = append(args, "--expect-sha256", p.Expect)
		}
		if p.CreateOnly {
			args = append(args, "--create-only")
		}
	case OpRename:
		args = append(args, "--to", p.To)
	case OpUpload:
		// The URL, size and digest are not secrets — only the token is, and it
		// rides the environment below.
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

	// New content rides the Job spec as base64 env vars (see ContentEnv for why
	// it is several). felis-api cannot create a Secret (it holds secrets:get only),
	// so the spec is the sole channel into the Pod; base64 keeps arbitrary bytes —
	// CRLF line endings, a UTF-8 BOM, a binary blob — intact through a field that
	// must be a valid string. Content is set ONLY for a write and the token ONLY
	// for an upload, so no other Job spec carries either.
	switch p.Op {
	case OpWrite:
		parts := splitContent(p.Content)
		container.Env = []corev1.EnvVar{{Name: ContentPartsEnv, Value: strconv.Itoa(len(parts))}}
		for i, part := range parts {
			container.Env = append(container.Env, corev1.EnvVar{Name: contentPartEnv(i), Value: part})
		}
	case OpUpload:
		container.Env = []corev1.EnvVar{{Name: UploadTokenEnv, Value: p.UploadToken}}
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        FilesJobName(p.Server, p.OpID),
			Namespace:   p.Namespace,
			Labels:      filesLabels(p),
			Annotations: filesAnnotations(p),
		},
		Spec: batchv1.JobSpec{
			// One shot: a file operation that failed must surface its failure, not be
			// retried behind the caller's back — a retried write is a second write.
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

// filesCapabilities is what the root executor keeps after dropping ALL (see
// Config.RunAsUser). DAC_OVERRIDE opens a mode-0600 file (level.dat) the game wrote
// as its own uid, which a fixed non-root uid could not. A write, mkdir, upload or
// unzip also keeps CHOWN so what it creates can be handed to naming.GameUID
// (exec.go ownWritten). List, read, delete and rename create nothing and get no
// more than they need.
func filesCapabilities(op string) []corev1.Capability {
	if op == OpWrite || op == OpMkdir || op == OpUpload || op == OpUnzip {
		return []corev1.Capability{"CHOWN", "DAC_OVERRIDE"}
	}
	return []corev1.Capability{"DAC_OVERRIDE"}
}

// filesPodSecurityContext pins the Pod identity. Root by default: the world volume
// belongs to the game uid (naming.GameUID), and root with DAC_OVERRIDE reaches its
// mode-0600 files as well as any a previous root-run release left behind. FSGroup is
// only rendered when configured so a root executor never chgrps the volume.
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

// filesAnnotations names the file an async Job works on. A path does not fit a
// label (63 characters, no slashes), so it rides an annotation.
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
