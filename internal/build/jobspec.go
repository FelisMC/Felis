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
// and pushes the image — its log IS the "build log" an admin watches (spec §16);
// Trivy is the main container whose CRITICAL-CVE verdict gates admission and is
// surfaced via the build status, not the log stream. Exported so the build-log
// streamer (internal/api.K8sBuildLogStreamer, spec §416 日志流复用 §8) follows the
// same container this Job defines — one source of truth for the name.
const (
	ContainerKaniko = "kaniko"
	ContainerTrivy  = "trivy"
	// ContainerFetch is the initContainer that pulls a submission's build context
	// from the felis-api internal face and extracts it into the shared emptyDir.
	// It exists only for an http(s) ContextRef (see BuildJob); a ref Kaniko can
	// read natively (s3://) or a pre-mounted path renders no such container.
	ContainerFetch = "context-fetch"

	// contextVolume/contextMountPath carry a fetched build context: the fetch
	// initContainer writes the extracted tree there, Kaniko reads it read-only.
	contextVolume    = "context"
	contextMountPath = "/context"
)

// contextSizeLimit bounds the extracted (attacker-controlled) context tree so a
// tarball bomb wedges the build pod instead of the node's disk. The compressed
// upload is capped at 1 GiB by the submit lane; 4 GiB leaves expansion room.
var contextSizeLimit = resource.MustParse("4Gi")

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
	BuildID        string
	ImageRef       string
	ContextRef     string
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
	KanikoImage       string
	TrivyImage        string
	Deadline          time.Duration
	CPULimit          string
	MemLimit          string
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
//   - activeDeadlineSeconds + backoffLimit=0 + per-container resource limits so
//     a runaway or poisoned build cannot exhaust the cluster (spec §16);
//   - the Trivy step runs with `--exit-code 1 --severity CRITICAL`, so a
//     CRITICAL CVE fails the Pod and therefore the Job — the only retained
//     automatic admission gate (spec §16).
//
// Sequencing: kaniko runs as an initContainer (build + push to the internal
// registry) and trivy as the main container (scan the pushed ref). The Pod
// succeeds only if kaniko pushed AND trivy found no CRITICAL CVE.
func BuildJob(p JobParams) (*batchv1.Job, error) {
	limits, err := resourceLimits(p.CPULimit, p.MemLimit)
	if err != nil {
		return nil, err
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
	initContainers := []corev1.Container{}
	var kanikoMounts []corev1.VolumeMount
	var podVolumes []corev1.Volume
	if isHTTPContextRef(p.ContextRef) {
		if p.FelisImage == "" {
			return nil, fmt.Errorf("build: context ref %q needs FelisImage for the fetch initContainer", p.ContextRef)
		}
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
			Args: []string{
				"fetch-context",
				"--url=" + p.ContextRef,
				"--out=" + contextMountPath,
			},
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
			Resources:       corev1.ResourceRequirements{Limits: limits, Requests: buildRequests(limits)},
			SecurityContext: fetchSec,
		}
		initContainers = append(initContainers, fetch)
		kanikoMounts = []corev1.VolumeMount{{Name: contextVolume, MountPath: contextMountPath, ReadOnly: true}}
		podVolumes = []corev1.Volume{{
			Name: contextVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				// The extracted tree is attacker-controlled; bound it so a tarball
				// bomb wedges THIS pod (admitted failure) instead of filling the
				// node's disk. The compressed upload is capped at 1 GiB by the
				// submit lane, and 4 GiB leaves room for a typical expansion.
				SizeLimit: sizeLimitPtr(),
			}},
		}}
	}

	kaniko := corev1.Container{
		Name:  ContainerKaniko,
		Image: p.KanikoImage,
		Args: []string{
			"--dockerfile=Dockerfile",
			"--context=" + contextPath,
			"--destination=" + p.ImageRef,
			// The internal registry is in-cluster only and may serve plain HTTP;
			// it is never a public ingress (spec §17). Both directions need the
			// insecure flags: --insecure/--skip-tls-verify cover the PUSH, while
			// the pull side needs its own pair — a Dockerfile's `FROM
			// registry.felis.svc:5000/...` otherwise fails with "server gave
			// HTTP response to HTTPS client", breaking every build based on a
			// platform image (the canonical modpack shape).
			"--insecure",
			"--skip-tls-verify",
			"--insecure-pull",
			"--skip-tls-verify-pull",
		},
		VolumeMounts:    kanikoMounts,
		Resources:       corev1.ResourceRequirements{Limits: limits, Requests: buildRequests(limits)},
		SecurityContext: kanikoSec,
	}
	initContainers = append(initContainers, kaniko)

	trivyArgs := []string{
		"image",
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
	trivyArgs = append(trivyArgs, p.ImageRef)
	trivy := corev1.Container{
		Name:            ContainerTrivy,
		Image:           p.TrivyImage,
		Args:            trivyArgs,
		Resources:       corev1.ResourceRequirements{Limits: limits, Requests: buildRequests(limits)},
		SecurityContext: sec,
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
					InitContainers:               initContainers,
					Containers:                   []corev1.Container{trivy},
					Volumes:                      podVolumes,
				},
			},
		},
	}
	return job, nil
}

// isHTTPContextRef reports whether ref is an http(s) URL — the shape the submit
// lane derives when the API is the blob transport — i.e. a context only the
// fetch initContainer can turn into a local path for Kaniko.
func isHTTPContextRef(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
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
// only to DNS, the internal registry, and any explicitly configured package
// mirrors. There is deliberately no allow-all egress rule.
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
		// DNS resolution: port-restricted to 53, so this is not an open-internet
		// hole — name resolution only.
		{
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

// sizeLimitPtr returns a copy of contextSizeLimit for a VolumeSource (the API
// object only ever gets serialized, but a shared pointer across rendered Jobs
// invites accidental aliasing).
func sizeLimitPtr() *resource.Quantity {
	q := contextSizeLimit
	return &q
}
func int64Ptr(i int64) *int64 { return &i }
