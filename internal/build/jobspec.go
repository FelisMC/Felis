package build

import (
	"fmt"
	"strings"
	"time"

	"felis.lolicon.best/internal/naming"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Label keys applied to build objects. ManagedBy doubles as the NetworkPolicy
// pod selector, so every build Pod is captured by the egress lock.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelBuildID   = "felis.lolicon.best/build-id"

	managedByValue = "felis-build"
	componentValue = "image-build"
)

// Container names within the build Pod. Kaniko is the initContainer that builds
// the image into a tarball — its log IS the "build log" an admin watches (spec
// §16); Trivy is the next initContainer, whose CRITICAL-CVE verdict gates both the
// push and admission and is surfaced via the build status, not the log stream;
// Push is the main container that publishes the scanned tarball. Exported so the
// build-log streamer (internal/api.K8sBuildLogStreamer, spec §416 日志流复用 §8)
// follows the same container this Job defines — one source of truth for the name.
const (
	ContainerGate   = "egress-gate"
	ContainerKaniko = "kaniko"
	ContainerTrivy  = "trivy"
	ContainerPush   = "push"
	// ContainerFetch is the initContainer that pulls a submission's build context
	// from the felis-api internal face and extracts it into the shared emptyDir.
	// It exists only for an http(s) ContextRef (see BuildJob); a ref Kaniko can
	// read natively (s3://) or a pre-mounted path renders no such container.
	ContainerFetch = "context-fetch"

	// contextVolume/contextMountPath carry a fetched build context: the fetch
	// initContainer writes the extracted tree there, Kaniko reads it read-only.
	contextVolume    = "context"
	contextMountPath = "/context"

	// imageVolume/imageTarPath carry the built image from Kaniko (--tar-path) to
	// Trivy (--input) and then to the push container.
	imageVolume    = "image"
	imageMountPath = "/image"
	imageTarPath   = imageMountPath + "/image.tar"
)

// imageSizeLimit bounds the built image tarball. A modpack image is typically a
// JRE, a server jar and a few hundred MiB of mods; 10 GiB leaves ample room while
// still stopping a runaway build from filling the node's disk.
var imageSizeLimit = resource.MustParse("10Gi")

// contextSizeLimit bounds the extracted (attacker-controlled) context tree so a
// tarball bomb wedges the build pod instead of the node's disk. The compressed
// upload is capped at 1 GiB by the submit lane; 4 GiB leaves expansion room.
var contextSizeLimit = resource.MustParse("4Gi")

// Per-container ephemeral-storage bounds (writable layer + logs; emptyDirs count
// toward the pod as a whole). Kaniko's limit is the operator's disk cap because
// kaniko unpacks the base image into its own root filesystem, which no emptyDir
// bound covers; it is also the largest limit in the pod, so it becomes the
// pod-level cap the kubelet holds context + unpacked rootfs + image tarball to.
// Trivy keeps its vulnerability and Java DBs (about 1.4 GiB live) in its layer.
// The others write nothing but logs.
var (
	gateDisk     = diskBounds{request: resource.MustParse("16Mi"), limit: resource.MustParse("64Mi")}
	fetchDisk    = diskBounds{request: resource.MustParse("64Mi"), limit: resource.MustParse("256Mi")}
	kanikoDiskRq = resource.MustParse("1Gi")
	trivyDisk    = diskBounds{request: resource.MustParse("256Mi"), limit: resource.MustParse("4Gi")}
	pushDisk     = diskBounds{request: resource.MustParse("16Mi"), limit: resource.MustParse("256Mi")}
)

type diskBounds struct{ request, limit resource.Quantity }

// buildJobTTL is how long a finished build Job survives before the Job
// controller deletes it — and with it the Pod whose kaniko log is the admin
// failure-triage surface (GET /api/v1/images/build/{id}/logs).
//
// Every other Job family the platform renders carries a TTL (fileedit 2m,
// backup/restore 10m); the build lane deliberately keeps a much longer one
// because the logs are the point. Without ANY TTL the Job and its completed
// Pod accumulate one pair per build forever: they count against the node's
// pod budget (110 on stock k3s), grow etcd, and eventually block new builds.
// Sync already tolerates a vanished Job (JobUnknown → failed; terminal builds
// are returned unchanged), so a week-old log falling off costs a 404, not a
// status flip.
const buildJobTTL = 7 * 24 * time.Hour

// JobParams are the rendered inputs to a build Job. They are derived from a
// Build + Config by the Builder; jobspec is a pure function of them so the
// security-critical Job shape is unit-tested without a cluster.
type JobParams struct {
	BuildID    string
	ImageRef   string
	ContextRef string
	// ContextDigest, when set, is passed to the fetch container, which refuses a
	// context whose sha256 differs (see Request.ContextDigest).
	ContextDigest  string
	Namespace      string
	ServiceAccount string
	RegistryURL    string
	// FelisImage runs the context-fetch initContainer (the felis binary's
	// fetch-context entrypoint). Required when ContextRef is an http(s) URL.
	FelisImage string
	// TrivyDBRepository overrides Trivy's vulnerability-DB source (the
	// --db-repository flag). Empty keeps Trivy's own default; see
	// build.Config.TrivyDBRepository for why an in-cluster install sets it.
	TrivyDBRepository string
	// TrivyJavaDBRepository overrides Trivy's Java-DB source (the
	// --java-db-repository flag), fetched lazily when the image contains Java
	// artifacts; empty keeps Trivy's own default, which the build egress lock
	// denies — a jar-bearing image then fails the scan.
	TrivyJavaDBRepository string
	KanikoImage           string
	TrivyImage            string
	Deadline              time.Duration
	CPULimit              string
	MemLimit              string
	// DiskLimit caps kaniko's ephemeral storage, and with it the pod's (see
	// kanikoDiskRq). Empty applies defaultDiskLimit.
	DiskLimit string
	// UserNamespaces runs the pod with hostUsers: false, so root in the build
	// containers is an unprivileged uid on the node. It needs a kernel and runtime
	// with idmapped mounts; Config.UserNamespaces decides.
	UserNamespaces bool
	// RuntimeClass, when set, runs the pod under that RuntimeClass (gVisor, Kata).
	RuntimeClass string
}

// BuildJobName is the deterministic Job name for a build id.
func BuildJobName(buildID string) string { return "build-" + buildID }

func buildLabels(p JobParams) map[string]string {
	return map[string]string{
		LabelManagedBy: managedByValue,
		LabelComponent: componentValue,
		LabelBuildID:   p.BuildID,
	}
}

// BuildJob renders the Kaniko+Trivy build Job (spec §16). Every isolation
// guarantee the spec demands is encoded here and asserted by jobspec_test.go,
// because no cluster runs in this environment:
//
//   - runs in the isolated felis-build namespace with the weak felis-build SA
//     (never the felis-api SA) and does NOT mount the SA token, so it cannot
//     reach the K8s API (spec §16, §21);
//   - no privileged container — Kaniko builds the Dockerfile without a daemon,
//     so docker-in-docker / privileged is never needed (spec §16, §22);
//   - the RuntimeDefault seccomp profile on the whole pod, and optionally a user
//     namespace (hostUsers: false) and a sandbox RuntimeClass, because kaniko is
//     no isolation boundary: the Dockerfile's RUN steps execute in its container;
//   - an egress gate ahead of everything else, so nothing runs before the
//     namespace's NetworkPolicy is enforced for this pod (cmd/felis egress-gate);
//   - activeDeadlineSeconds + backoffLimit=0 + per-container CPU, memory and
//     ephemeral-storage limits so a runaway or poisoned build cannot exhaust the
//     node (spec §16);
//   - the Trivy step runs with `--exit-code 1 --severity CRITICAL`, so a
//     CRITICAL CVE fails the Pod and therefore the Job — the only retained
//     automatic admission gate (spec §16).
//
// Sequencing: kaniko builds with --no-push into a tarball, trivy scans that
// tarball, and only then does the push container publish it. So:
//
//   - an image that fails the scan is never published — it used to be pushed to
//     the final tag first and scanned after, overwriting whatever that tag held;
//   - the registry credential lives in the push container alone. Kaniko executes
//     the untrusted Dockerfile and holds no credential at all, and the registry
//     refuses anonymous writes (internal/registrygate).
//
// The Pod succeeds only if kaniko built, trivy found no CRITICAL CVE, and the push
// landed.
func BuildJob(p JobParams) (*batchv1.Job, error) {
	limits, err := resourceLimits(p.CPULimit, p.MemLimit)
	if err != nil {
		return nil, err
	}
	diskCap := p.DiskLimit
	if diskCap == "" {
		diskCap = defaultDiskLimit
	}
	kanikoDisk, err := resource.ParseQuantity(diskCap)
	if err != nil {
		return nil, fmt.Errorf("build: invalid disk limit %q: %w", diskCap, err)
	}
	kanikoRq := kanikoDiskRq.DeepCopy()
	if kanikoDisk.Cmp(kanikoRq) < 0 {
		kanikoRq = kanikoDisk.DeepCopy()
	}
	if p.FelisImage == "" {
		return nil, fmt.Errorf("build: FelisImage is required: the push container runs it")
	}
	deadline := int64(p.Deadline / time.Second)
	if deadline <= 0 {
		deadline = int64(defaultDeadline / time.Second)
	}

	// Hardened container security context baseline: no privilege, no privilege
	// escalation, drop all capabilities. Kaniko needs a writable root filesystem
	// to unpack layers, so we do not force read-only root here; it also needs a
	// minimal capability subset added back (kanikoSec below), while fetch and
	// trivy run with exactly this baseline.
	sec := &corev1.SecurityContext{
		Privileged:               boolPtr(false),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	// Kaniko unpacks base-image layers as root, and the tar apply must chown/chmod
	// files to the owners the layer recorded — impossible under drop-ALL (live:
	// "failed to get filesystem from image: chown /etc/gshadow: operation not
	// permitted" for any FROM <base image>; scratch builds masked this because
	// COPY only ever creates files kaniko itself owns). Add back exactly the caps
	// the unpack needs and nothing else: CHOWN/FOWNER for the ownership and mode
	// restore, DAC_OVERRIDE to write entries whose bits would otherwise exclude
	// even root once the capability-based exemption is gone.
	kanikoSec := sec.DeepCopy()
	kanikoSec.Capabilities = &corev1.Capabilities{
		Drop: []corev1.Capability{"ALL"},
		Add:  []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER"},
	}

	// The context Kaniko reads. An http(s) ref (the submit lane's derived ref: the
	// API streams the blob on its internal face, because the build Pod can neither
	// mount the control-plane uploads PVC across namespaces nor hold object-store
	// credentials) is first fetched into a shared emptyDir; a ref Kaniko can read
	// in place (s3://, or a path an installer pre-mounted) passes through untouched.
	contextPath := p.ContextRef

	// The gate runs before anything else. A new pod's NetworkPolicy is programmed
	// asynchronously: live on k3s (kube-router), a build-labelled pod reached the
	// internet and the Kubernetes API for the first ~0.7 s of its life. The gate
	// holds the pod until a destination the policy denies stops answering, so the
	// Dockerfile never runs inside that window.
	gateSec := sec.DeepCopy()
	gateSec.ReadOnlyRootFilesystem = boolPtr(true)
	gateSec.RunAsNonRoot = boolPtr(true)
	gateSec.RunAsUser = int64Ptr(nonRootUID)
	gate := corev1.Container{
		Name:  ContainerGate,
		Image: p.FelisImage,
		Args:  []string{"egress-gate"},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("100m"),
				corev1.ResourceMemory:           resource.MustParse("64Mi"),
				corev1.ResourceEphemeralStorage: gateDisk.limit,
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("10m"),
				corev1.ResourceMemory:           resource.MustParse("16Mi"),
				corev1.ResourceEphemeralStorage: gateDisk.request,
			},
		},
		SecurityContext: gateSec,
	}
	initContainers := []corev1.Container{gate}
	imageMount := corev1.VolumeMount{Name: imageVolume, MountPath: imageMountPath}
	kanikoMounts := []corev1.VolumeMount{imageMount}
	podVolumes := []corev1.Volume{{
		Name: imageVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: quantityPtr(imageSizeLimit),
		}},
	}}
	if IsHTTPContextRef(p.ContextRef) {
		contextPath = contextMountPath
		// The fetch container runs as root while Kaniko keeps the image default
		// (also root): Kaniko re-copies the Dockerfile out of the context and
		// chowns/chmods it to the SOURCE file's owner, which fails for any other
		// owner without CAP_CHOWN/CAP_FOWNER — capabilities this pod deliberately
		// drops (the live drill hit exactly this: "copying dockerfile: chown
		// /kaniko/Dockerfile: operation not permitted" with the distroless uid
		// 65532). Extracting as root, the uid Kaniko itself runs as, keeps the
		// context owned by the only user that can satisfy that copy. The pod is
		// root by necessity regardless: Kaniko unpacks base-image layers into its
		// own filesystem.
		fetchSec := sec.DeepCopy()
		fetchSec.RunAsUser = int64Ptr(0)
		fetchSec.RunAsGroup = int64Ptr(0)
		fetch := corev1.Container{
			Name:  ContainerFetch,
			Image: p.FelisImage,
			Args:  fetchArgs(p),
			// The internal face is service-token gated, and the token is read from a
			// Secret the installer materializes in THIS namespace (secretKeyRef is
			// namespace-local). It is mounted into this initContainer only: the Kaniko
			// container executes the untrusted Dockerfile and must never hold it, and
			// pod containers share neither environment nor PID namespace.
			Env: []corev1.EnvVar{{
				Name: "FELIS_SERVICE_TOKEN",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: naming.ServiceTokenSecretName},
					Key:                  naming.ServiceTokenSecretKey,
				}},
			}},
			VolumeMounts:    []corev1.VolumeMount{{Name: contextVolume, MountPath: contextMountPath}},
			Resources:       withDisk(limits, fetchDisk),
			SecurityContext: fetchSec,
		}
		initContainers = append(initContainers, fetch)
		kanikoMounts = append(kanikoMounts, corev1.VolumeMount{Name: contextVolume, MountPath: contextMountPath, ReadOnly: true})
		podVolumes = append(podVolumes, corev1.Volume{
			Name: contextVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				// The extracted tree is attacker-controlled; bound it so a tarball
				// bomb wedges THIS pod (admitted failure) instead of filling the
				// node's disk. The compressed upload is capped at 1 GiB by the
				// submit lane, and 4 GiB leaves room for a typical expansion.
				SizeLimit: quantityPtr(contextSizeLimit),
			}},
		})
	}

	kaniko := corev1.Container{
		Name:  ContainerKaniko,
		Image: p.KanikoImage,
		Args: []string{
			"--dockerfile=Dockerfile",
			"--context=" + contextPath,
			// --destination only names the image inside the tarball; --no-push
			// keeps Kaniko off the registry's write path entirely.
			"--destination=" + p.ImageRef,
			"--no-push",
			"--tar-path=" + imageTarPath,
			// The internal registry is in-cluster only and serves plain HTTP; it
			// is never a public ingress (spec §17). A Dockerfile's `FROM
			// registry.felis.svc:5000/...` fails with "server gave HTTP response
			// to HTTPS client" without these, breaking every build based on a
			// platform image (the canonical modpack shape).
			"--insecure-pull",
			"--skip-tls-verify-pull",
		},
		VolumeMounts:    kanikoMounts,
		Resources:       withDisk(limits, diskBounds{request: kanikoRq, limit: kanikoDisk}),
		SecurityContext: kanikoSec,
	}
	initContainers = append(initContainers, kaniko)

	trivyArgs := []string{
		"image",
		"--input", imageTarPath,
		"--exit-code", "1",
		"--severity", "CRITICAL",
		"--no-progress",
		"--insecure",
	}
	// The DB source is configurable because the default (mirror.gcr.io/ghcr.io)
	// is exactly what the build egress lock denies: an install that never mirrors
	// the DB cannot complete a scan, and the gate fails closed on purpose. The
	// supported shape is the internal registry (`--insecure` above already covers
	// its plain HTTP).
	if p.TrivyDBRepository != "" {
		trivyArgs = append(trivyArgs, "--db-repository", p.TrivyDBRepository)
	}
	if p.TrivyJavaDBRepository != "" {
		trivyArgs = append(trivyArgs, "--java-db-repository", p.TrivyJavaDBRepository)
	}
	trivy := corev1.Container{
		Name:            ContainerTrivy,
		Image:           p.TrivyImage,
		Args:            trivyArgs,
		VolumeMounts:    []corev1.VolumeMount{{Name: imageVolume, MountPath: imageMountPath, ReadOnly: true}},
		Resources:       withDisk(limits, trivyDisk),
		SecurityContext: sec,
	}
	initContainers = append(initContainers, trivy)

	// The publish step: the only container that holds the registry credential,
	// read from a Secret the installer materializes in this namespace. It runs
	// the felis binary (internal/imagepush), which only reads the tarball.
	pushSec := sec.DeepCopy()
	pushSec.ReadOnlyRootFilesystem = boolPtr(true)
	secretEnv := func(name, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: naming.RegistryPushSecretName},
			Key:                  key,
		}}}
	}
	push := corev1.Container{
		Name:  ContainerPush,
		Image: p.FelisImage,
		Args: []string{
			"push-image",
			"--tar=" + imageTarPath,
			"--ref=" + p.ImageRef,
			"--scheme=http",
		},
		Env: []corev1.EnvVar{
			secretEnv("FELIS_REGISTRY_USERNAME", naming.RegistryPushUsernameKey),
			secretEnv("FELIS_REGISTRY_PASSWORD", naming.RegistryPushPasswordKey),
		},
		VolumeMounts:    []corev1.VolumeMount{{Name: imageVolume, MountPath: imageMountPath, ReadOnly: true}},
		Resources:       withDisk(limits, pushDisk),
		SecurityContext: pushSec,
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BuildJobName(p.BuildID),
			Namespace: p.Namespace,
			Labels:    buildLabels(p),
		},
		Spec: batchv1.JobSpec{
			// A poisoned build must not loop — one shot, then a terminal verdict.
			BackoffLimit:          int32Ptr(0),
			ActiveDeadlineSeconds: int64Ptr(deadline),
			// ...and a finished one must not linger forever (see buildJobTTL).
			TTLSecondsAfterFinished: int32Ptr(int32(buildJobTTL / time.Second)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: buildLabels(p)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           p.ServiceAccount,
					AutomountServiceAccountToken: boolPtr(false),
					// RUN steps execute in kaniko's container with root and three
					// capabilities; RuntimeDefault takes away the syscalls a container
					// never needs, among them most kernel-escape primitives (unshare,
					// mount, keyctl, bpf). Every step was checked to run under it live.
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					InitContainers: initContainers,
					Containers:     []corev1.Container{push},
					Volumes:        podVolumes,
				},
			},
		},
	}
	if p.UserNamespaces {
		job.Spec.Template.Spec.HostUsers = boolPtr(false)
	}
	if p.RuntimeClass != "" {
		rc := p.RuntimeClass
		job.Spec.Template.Spec.RuntimeClassName = &rc
	}
	return job, nil
}

// nonRootUID is the distroless nonroot user the platform image ships as.
const nonRootUID = 65532

// withDisk is the resources block for one build container: the CPU and memory
// caps with their schedulable floor (buildRequests), plus its ephemeral-storage
// request and limit.
func withDisk(limits corev1.ResourceList, d diskBounds) corev1.ResourceRequirements {
	lim := limits.DeepCopy()
	lim[corev1.ResourceEphemeralStorage] = d.limit
	req := buildRequests(limits)
	req[corev1.ResourceEphemeralStorage] = d.request
	return corev1.ResourceRequirements{Limits: lim, Requests: req}
}

// fetchArgs is the context-fetch container's argv. The digest flag rides along
// only when the build pins one; admin builds from a URL they supplied have none.
func fetchArgs(p JobParams) []string {
	args := []string{"fetch-context", "--url=" + p.ContextRef, "--out=" + contextMountPath}
	if p.ContextDigest != "" {
		args = append(args, "--sha256="+p.ContextDigest)
	}
	return args
}

// IsHTTPContextRef reports whether ref is an http(s) URL — the shape the submit
// lane derives when the API is the blob transport — i.e. a context only the
// fetch initContainer can turn into a local path for Kaniko.
func IsHTTPContextRef(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

// ClusterDNSPeer selects the cluster DNS pods (CoreDNS in kube-system, labelled
// k8s-app=kube-dns on k3s and upstream alike) — the only resolver a sandboxed pod
// needs.
func ClusterDNSPeer() networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"k8s-app": "kube-dns"},
		},
	}
}

// NetPolParams parameterises the build-namespace egress lock.
type NetPolParams struct {
	Namespace         string
	RegistryNamespace string
	RegistryPort      int32
	// ControlNamespace and APIPort are where the felis-api internal face lives:
	// the fetch initContainer's only egress besides DNS and the registry. Both
	// defaults (felis, 8081) match platform.DefaultControlNamespace and the
	// internal listener, so an unset Params is still the safe shape.
	ControlNamespace string
	APIPort          int32
	// PackageSourceCIDRs is an optional, explicit allowlist of external package
	// mirrors (spec §16: egress 仅 registry + 包源). Empty means the most
	// locked-down default — no internet egress at all (默认拒外网).
	PackageSourceCIDRs []string
}

// BuildNetworkPolicy renders the default-deny egress policy for build Pods
// (spec §16, §21: build ns egress 仅放 registry + 包源,默认拒外网). It selects
// build Pods by the managed-by label, denies all ingress, and allows egress
// only to the cluster DNS pods, the internal registry, felis-api's internal
// face, and any explicitly configured package mirrors. There is deliberately no allow-all egress rule.
func BuildNetworkPolicy(p NetPolParams) *networkingv1.NetworkPolicy {
	port := p.RegistryPort
	if port == 0 {
		port = 5000
	}
	controlNS := p.ControlNamespace
	if controlNS == "" {
		controlNS = "felis"
	}
	apiPort := p.APIPort
	if apiPort == 0 {
		apiPort = 8081
	}
	dnsUDP := corev1.ProtocolUDP
	dnsTCP := corev1.ProtocolTCP
	dns53 := intstr.FromInt32(53)
	regPort := intstr.FromInt32(port)
	ctxPort := intstr.FromInt32(apiPort)

	egress := []networkingv1.NetworkPolicyEgressRule{
		// DNS resolution, to the cluster resolver only. Port 53 to ANY address
		// would be an exfiltration channel out of an otherwise sealed sandbox
		// (a Dockerfile RUN can speak DNS, or anything else, to a resolver it
		// controls); the cluster DNS Service is DNATed to these pods before the
		// policy is evaluated, so selecting them is what "resolve names" means.
		{
			To: []networkingv1.NetworkPolicyPeer{ClusterDNSPeer()},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &dnsUDP, Port: &dns53},
				{Protocol: &dnsTCP, Port: &dns53},
			},
		},
		// The internal registry, selected by the namespace's immutable
		// kubernetes.io/metadata.name label, on the registry port only.
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": p.RegistryNamespace},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &dnsTCP, Port: &regPort},
			},
		},
		// felis-api's internal face, where the fetch initContainer streams the
		// submission's build context from. Without this rule the build Pod could
		// not read the context and every user build would fail in its first init
		// step — the default-deny here is exactly why the transport had to be
		// planned, not assumed.
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": controlNS},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &dnsTCP, Port: &ctxPort},
			},
		},
	}
	// Explicit package-mirror CIDRs, when configured. No CIDR ⇒ no internet.
	for _, cidr := range p.PackageSourceCIDRs {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: cidr},
			}},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "felis-build-egress",
			Namespace: p.Namespace,
			Labels: map[string]string{
				LabelManagedBy: managedByValue,
				LabelComponent: componentValue,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{LabelManagedBy: managedByValue},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// Empty Ingress slice = deny all ingress: nothing connects to a
			// build Pod.
			Ingress: []networkingv1.NetworkPolicyIngressRule{},
			Egress:  egress,
		},
	}
}

// BuildServiceAccount renders the weak build SA (spec §16, §21). It is the most
// dangerous identity in the platform if mis-scoped, so it is created bare: no
// secrets, token auto-mounting disabled, and — by virtue of having no Role or
// RoleBinding anywhere — zero K8s API permissions. Its only capability is
// network reachability to push to the registry, which RBAC does not grant.
func BuildServiceAccount(namespace, name string) *corev1.ServiceAccount {
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

// BuildLimitRange bounds any container in the build namespace that arrives
// without its own limits. Build Jobs set every limit themselves (BuildJob); this
// is the backstop for anything else that lands in the namespace, which shares
// the node's disk with the game worlds.
func BuildLimitRange(namespace string) *corev1.LimitRange {
	return &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "felis-build-limits",
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy: managedByValue,
				LabelComponent: componentValue,
			},
		},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type: corev1.LimitTypeContainer,
			Default: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("1"),
				corev1.ResourceMemory:           resource.MustParse("1Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
			},
			DefaultRequest: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("100m"),
				corev1.ResourceMemory:           resource.MustParse("128Mi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("64Mi"),
			},
		}}},
	}
}

// BuildResourceQuota caps how many pods can run in the build namespace at once:
// MaxConcurrentLimit builds, the user-namespace probe, and one to spare. The
// Builder's queue keeps builds under it; the quota holds even when something
// else creates pods there. Finished pods do not count.
func BuildResourceQuota(namespace string) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "felis-build-quota",
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy: managedByValue,
				LabelComponent: componentValue,
			},
		},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourcePods:                   *resource.NewQuantity(MaxConcurrentLimit+2, resource.DecimalSI),
			corev1.ResourcePersistentVolumeClaims: *resource.NewQuantity(0, resource.DecimalSI),
		}},
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
		return nil, fmt.Errorf("build: invalid cpu limit %q: %w", cpu, err)
	}
	memQty, err := resource.ParseQuantity(mem)
	if err != nil {
		return nil, fmt.Errorf("build: invalid memory limit %q: %w", mem, err)
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:    cpuQty,
		corev1.ResourceMemory: memQty,
	}, nil
}

// buildRequests is the scheduler floor a build container asks for while its
// configured limit stays the safety cap. Reserving the full cap as a request is
// what once made a default install on the platform's starter node (4 vCPU /
// 5.5 GiB) unable to schedule ANY build — caught by the live end-to-end drill, not
// by any unit test. A build is best-effort batch work: it may be throttled or
// evicted under contention, which fails the Job loudly, and the caps still stop a
// runaway build from exhausting the node.
func buildRequests(limits corev1.ResourceList) corev1.ResourceList {
	req := corev1.ResourceList{}
	for res, floor := range map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceCPU:    resource.MustParse("250m"),
		corev1.ResourceMemory: resource.MustParse("512Mi"),
	} {
		limit, ok := limits[res]
		if ok && limit.Cmp(floor) < 0 {
			floor = limit // never ask for more than the cap
		}
		req[res] = floor
	}
	return req
}

func boolPtr(b bool) *bool    { return &b }
func int32Ptr(i int32) *int32 { return &i }

// quantityPtr returns a pointer to a copy of q for a VolumeSource (the API object
// only ever gets serialized, but a shared pointer across rendered Jobs invites
// accidental aliasing).
func quantityPtr(q resource.Quantity) *resource.Quantity {
	return &q
}
func int64Ptr(i int64) *int64 { return &i }
