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

// LabelManagedBy is also the NetworkPolicy pod selector, so every build Pod falls under the egress lock.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelBuildID   = "felis.lolicon.best/build-id"

	managedByValue = "felis-build"
	componentValue = "image-build"
)

// Container names of the build Pod; the build-log streamer and the outcome reader use the same names.
const (
	ContainerGate     = "egress-gate"
	ContainerKaniko   = "kaniko"
	ContainerTrivy    = "trivy"
	ContainerSBOM     = "sbom"
	ContainerScanGate = "scan-gate"
	ContainerPush     = "push"
	ContainerFetch    = "context-fetch" // rendered only for an http(s) ContextRef

	// The fetch container writes the extracted context here; kaniko reads it read-only.
	contextVolume    = "context"
	contextMountPath = "/context"

	// The image tarball goes from kaniko to trivy to push.
	imageVolume    = "image"
	imageMountPath = "/image"
	imageTarPath   = imageMountPath + "/image.tar"

	// Kaniko never mounts reports, so a Dockerfile cannot write its own scan verdict.
	reportsVolume    = "reports"
	reportsMountPath = "/reports"
	trivyReportPath  = reportsMountPath + "/trivy.json"
	sbomPath         = reportsMountPath + "/sbom.cdx.json"
)

var reportsSizeLimit = resource.MustParse("512Mi")

var imageSizeLimit = resource.MustParse("10Gi")

// contextSizeLimit bounds the extracted, attacker-controlled context; the compressed upload is capped at 1 GiB.
var contextSizeLimit = resource.MustParse("4Gi")

// Kaniko's disk limit determines the Pod cap; Trivy also needs room for unpacked databases and downloads.
var (
	gateDisk     = diskBounds{request: resource.MustParse("16Mi"), limit: resource.MustParse("64Mi")}
	fetchDisk    = diskBounds{request: resource.MustParse("64Mi"), limit: resource.MustParse("256Mi")}
	kanikoDiskRq = resource.MustParse("1Gi")
	trivyDisk    = diskBounds{request: resource.MustParse("3Gi"), limit: resource.MustParse("8Gi")}
	sbomDisk     = diskBounds{request: resource.MustParse("16Mi"), limit: resource.MustParse("256Mi")}
	scanGateDisk = diskBounds{request: resource.MustParse("16Mi"), limit: resource.MustParse("64Mi")}
	pushDisk     = diskBounds{request: resource.MustParse("16Mi"), limit: resource.MustParse("256Mi")}
)

type diskBounds struct{ request, limit resource.Quantity }

// buildJobTTL keeps a finished Job, whose Pod log is the failure-triage surface, for a week; Sync tolerates a vanished Job.
const buildJobTTL = 7 * 24 * time.Hour

type JobParams struct {
	BuildID    string
	ImageRef   string
	ContextRef string
	// ContextDigest, when set, makes the fetch container refuse a context with a different sha256.
	ContextDigest     string
	Namespace         string
	ServiceAccount    string
	RegistryURL       string
	FelisImage        string
	TrivyDBRepository string
	// TrivyJavaDBRepository is the --java-db-repository flag; empty keeps Trivy's default,
	// which the egress lock denies, so a jar-bearing image then fails the scan.
	TrivyJavaDBRepository string
	KanikoImage           string
	TrivyImage            string
	// ScanFailOn, ScanFailUnfixed and ScanAccept are the scan-gate policy; empty ScanFailOn applies DefaultScanFailOn.
	ScanFailOn      []string
	ScanFailUnfixed bool
	ScanAccept      []string
	Deadline        time.Duration
	CPULimit        string
	MemLimit        string
	DiskLimit       string // kaniko ephemeral storage and the pod cap; empty applies defaultDiskLimit
	UserNamespaces  bool   // hostUsers: false; needs idmapped mounts
	RuntimeClass    string
}

func BuildJobName(buildID string) string { return "build-" + buildID }

func buildLabels(p JobParams) map[string]string {
	return map[string]string{
		LabelManagedBy: managedByValue,
		LabelComponent: componentValue,
		LabelBuildID:   p.BuildID,
	}
}

// Publish only after scanning passes; registry credentials belong exclusively to the push container.
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

	sec := &corev1.SecurityContext{
		Privileged:               boolPtr(false),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	// Unpacking base-image layers must chown/chmod files, which fails under drop-ALL.
	kanikoSec := sec.DeepCopy()
	kanikoSec.Capabilities = &corev1.Capabilities{
		Drop: []corev1.Capability{"ALL"},
		Add:  []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER"},
	}

	// An http(s) ref is fetched into a shared emptyDir; any other ref is read by kaniko in place.
	contextPath := p.ContextRef

	// A new pod's NetworkPolicy is programmed asynchronously (~0.7 s on k3s); the gate blocks until a denied destination stops answering.
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
		// A failed step's last log lines become the build error recorded by Sync.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
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
		// Fetch runs as root like kaniko: kaniko chowns its copy of the Dockerfile to the source file's owner, which fails for any other uid.
		fetchSec := sec.DeepCopy()
		fetchSec.RunAsUser = int64Ptr(0)
		fetchSec.RunAsGroup = int64Ptr(0)
		fetch := corev1.Container{
			Name:  ContainerFetch,
			Image: p.FelisImage,
			Args:  fetchArgs(p),
			// Only this container gets the context-read token; kaniko runs the untrusted Dockerfile.
			Env: []corev1.EnvVar{{
				Name: "FELIS_SERVICE_TOKEN",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: naming.BuildTokenSecretName},
					Key:                  naming.ServiceTokenSecretKey,
				}},
			}},
			VolumeMounts:             []corev1.VolumeMount{{Name: contextVolume, MountPath: contextMountPath}},
			Resources:                withDisk(limits, fetchDisk),
			SecurityContext:          fetchSec,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		}
		initContainers = append(initContainers, fetch)
		kanikoMounts = append(kanikoMounts, corev1.VolumeMount{Name: contextVolume, MountPath: contextMountPath, ReadOnly: true})
		podVolumes = append(podVolumes, corev1.Volume{
			Name: contextVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
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
			// --destination only names the image inside the tarball; --no-push keeps kaniko off the registry.
			"--destination=" + p.ImageRef,
			"--no-push",
			"--tar-path=" + imageTarPath,
			// The in-cluster registry serves plain HTTP; without these, FROM a platform image fails.
			"--insecure-pull",
			"--skip-tls-verify-pull",
		},
		VolumeMounts:             kanikoMounts,
		Resources:                withDisk(limits, diskBounds{request: kanikoRq, limit: kanikoDisk}),
		SecurityContext:          kanikoSec,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	initContainers = append(initContainers, kaniko)

	// The full report (no severity filter) is also the SBOM source; trivy exits 0 on findings and scan-gate applies the policy.
	trivyArgs := []string{
		"image",
		"--input", imageTarPath,
		"--format", "json",
		"--output", trivyReportPath,
		"--no-progress",
		"--insecure",
	}
	// The default DB source is denied by the build egress lock, so an install must mirror the DB; the scan fails closed otherwise.
	if p.TrivyDBRepository != "" {
		trivyArgs = append(trivyArgs, "--db-repository", p.TrivyDBRepository)
	}
	if p.TrivyJavaDBRepository != "" {
		trivyArgs = append(trivyArgs, "--java-db-repository", p.TrivyJavaDBRepository)
	}
	reportsMount := corev1.VolumeMount{Name: reportsVolume, MountPath: reportsMountPath}
	trivy := corev1.Container{
		Name:  ContainerTrivy,
		Image: p.TrivyImage,
		Args:  trivyArgs,
		VolumeMounts: []corev1.VolumeMount{
			{Name: imageVolume, MountPath: imageMountPath, ReadOnly: true},
			reportsMount,
		},
		Resources:                withDisk(limits, trivyDisk),
		SecurityContext:          sec,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	sbom := corev1.Container{
		Name:                     ContainerSBOM,
		Image:                    p.TrivyImage,
		Args:                     []string{"convert", "--format", "cyclonedx", "--output", sbomPath, trivyReportPath},
		VolumeMounts:             []corev1.VolumeMount{reportsMount},
		Resources:                withDisk(limits, sbomDisk),
		SecurityContext:          sec,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	failOn := p.ScanFailOn
	if len(failOn) == 0 {
		failOn = DefaultScanFailOn
	}
	gateArgs := []string{"scan-gate", "--report=" + trivyReportPath, "--sbom=" + sbomPath,
		"--fail-on=" + strings.Join(failOn, ",")}
	if p.ScanFailUnfixed {
		gateArgs = append(gateArgs, "--fail-unfixed")
	}
	if len(p.ScanAccept) > 0 {
		gateArgs = append(gateArgs, "--accept="+strings.Join(p.ScanAccept, ","))
	}
	// scan-gate writes its own verdict to the termination log, so no log-tail fallback.
	scanGate := corev1.Container{
		Name:            ContainerScanGate,
		Image:           p.FelisImage,
		Args:            gateArgs,
		VolumeMounts:    []corev1.VolumeMount{{Name: reportsVolume, MountPath: reportsMountPath, ReadOnly: true}},
		Resources:       withDisk(limits, scanGateDisk),
		SecurityContext: gateSec,
	}
	initContainers = append(initContainers, trivy, sbom, scanGate)
	podVolumes = append(podVolumes, corev1.Volume{
		Name: reportsVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: quantityPtr(reportsSizeLimit),
		}},
	})

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
		VolumeMounts:             []corev1.VolumeMount{{Name: imageVolume, MountPath: imageMountPath, ReadOnly: true}},
		Resources:                withDisk(limits, pushDisk),
		SecurityContext:          pushSec,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BuildJobName(p.BuildID),
			Namespace: p.Namespace,
			Labels:    buildLabels(p),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   int64Ptr(deadline),
			TTLSecondsAfterFinished: int32Ptr(int32(buildJobTTL / time.Second)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: buildLabels(p)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           p.ServiceAccount,
					AutomountServiceAccountToken: boolPtr(false),
					// RUN steps execute in kaniko's container; RuntimeDefault blocks kernel-escape syscalls (unshare, mount, keyctl, bpf).
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

func withDisk(limits corev1.ResourceList, d diskBounds) corev1.ResourceRequirements {
	lim := limits.DeepCopy()
	lim[corev1.ResourceEphemeralStorage] = d.limit
	req := buildRequests(limits)
	req[corev1.ResourceEphemeralStorage] = d.request
	return corev1.ResourceRequirements{Limits: lim, Requests: req}
}

func fetchArgs(p JobParams) []string {
	args := []string{"fetch-context", "--url=" + p.ContextRef, "--out=" + contextMountPath}
	if p.ContextDigest != "" {
		args = append(args, "--sha256="+p.ContextDigest)
	}
	return args
}

func IsHTTPContextRef(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

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

type NetPolParams struct {
	Namespace         string
	RegistryNamespace string
	RegistryPort      int32
	// ControlNamespace and APIPort locate the felis-api internal face; defaults are felis and 8081.
	ControlNamespace string
	APIPort          int32
	// PackageSourceCIDRs allowlists external package mirrors; empty means no internet egress.
	PackageSourceCIDRs []string
}

// BuildNetworkPolicy denies all ingress and allows egress only to DNS, the registry, the felis-api internal face and PackageSourceCIDRs.
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
		// Port 53 to any address would let a Dockerfile exfiltrate over DNS; only the cluster resolver is allowed.
		{
			To: []networkingv1.NetworkPolicyPeer{ClusterDNSPeer()},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &dnsUDP, Port: &dns53},
				{Protocol: &dnsTCP, Port: &dns53},
			},
		},
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
			// An empty Ingress slice denies all ingress.
			Ingress: []networkingv1.NetworkPolicyIngressRule{},
			Egress:  egress,
		},
	}
}

// BuildServiceAccount renders a bare SA: no secrets, no token auto-mount and no Role bindings, so zero K8s API permissions.
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

// BuildLimitRange is the backstop for containers that arrive without limits; build Jobs set their own.
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

// BuildResourceQuota caps running pods at MaxConcurrentLimit builds plus the user-namespace probe and one spare.
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

// buildRequests is the scheduler floor of a build container; requesting the full limit would leave a small node unable to schedule any build.
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

func quantityPtr(q resource.Quantity) *resource.Quantity {
	return &q
}
func int64Ptr(i int64) *int64 { return &i }
