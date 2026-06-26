package build

import (
	"fmt"
	"time"

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
)

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
	KanikoImage    string
	TrivyImage     string
	Deadline       time.Duration
	CPULimit       string
	MemLimit       string
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

	// Hardened container security context shared by both build containers: no
	// privilege, no privilege escalation, drop all capabilities. Kaniko needs a
	// writable root filesystem to unpack layers, so we do not force read-only
	// root here, but it gains no privilege.
	sec := &corev1.SecurityContext{
		Privileged:               boolPtr(false),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	kaniko := corev1.Container{
		Name:  ContainerKaniko,
		Image: p.KanikoImage,
		Args: []string{
			"--dockerfile=Dockerfile",
			"--context=" + p.ContextRef,
			"--destination=" + p.ImageRef,
			// The internal registry is in-cluster only and may serve plain HTTP;
			// it is never a public ingress (spec §17).
			"--insecure",
			"--skip-tls-verify",
		},
		Resources:       corev1.ResourceRequirements{Limits: limits, Requests: limits},
		SecurityContext: sec,
	}

	trivy := corev1.Container{
		Name:  ContainerTrivy,
		Image: p.TrivyImage,
		Args: []string{
			"image",
			"--exit-code", "1",
			"--severity", "CRITICAL",
			"--no-progress",
			"--insecure",
			p.ImageRef,
		},
		Resources:       corev1.ResourceRequirements{Limits: limits, Requests: limits},
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
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: buildLabels(p)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           p.ServiceAccount,
					AutomountServiceAccountToken: boolPtr(false),
					InitContainers:               []corev1.Container{kaniko},
					Containers:                   []corev1.Container{trivy},
				},
			},
		},
	}
	return job, nil
}

// NetPolParams parameterises the build-namespace egress lock.
type NetPolParams struct {
	Namespace         string
	RegistryNamespace string
	RegistryPort      int32
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
	dnsUDP := corev1.ProtocolUDP
	dnsTCP := corev1.ProtocolTCP
	dns53 := intstr.FromInt32(53)
	regPort := intstr.FromInt32(port)

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

func boolPtr(b bool) *bool    { return &b }
func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }
