package operator

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/software"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Inject envServiceToken only into the setup-owned login system pod, sourced from a Secret.
const envServiceToken = "FELIS_SERVICE_TOKEN"

// envForwardingSecret goes to all backends because modern-forwarding mode is proxy-wide.
const envForwardingSecret = "FELIS_FORWARDING_SECRET"

const (
	GamePort int32 = 25565

	DefaultRconPort int32 = 25575

	containerName  = "minecraft"
	dataVolumeName = "world"
	dataMountPath  = "/data"

	felisBinaryPath = "/usr/local/bin/felis"

	ManagedByValue = "felis-operator"
	ComponentValue = "server"

	defaultGraceSeconds int64  = 300
	defaultStorageSize  string = "8Gi"
)

// selectorFor contains only immutable server identity labels.
func selectorFor(server *v1alpha1.MinecraftServer) map[string]string {
	return map[string]string{v1alpha1.LabelServer: server.Name}
}

func labelsFor(server *v1alpha1.MinecraftServer) map[string]string {
	return map[string]string{
		v1alpha1.LabelServer:    server.Name,
		v1alpha1.LabelManagedBy: ManagedByValue,
		v1alpha1.LabelComponent: ComponentValue,
	}
}

// podLabelsFor adds the setup-owned role only to the template; changing the StatefulSet selector is forbidden.
func podLabelsFor(server *v1alpha1.MinecraftServer) map[string]string {
	l := labelsFor(server)
	if role := server.Labels[v1alpha1.LabelSystemRole]; role != "" {
		l[v1alpha1.LabelSystemRole] = role
	}
	return l
}

func headlessServiceName(name string) string { return name + "-hl" }

func rconPort(server *v1alpha1.MinecraftServer) int32 {
	if server.Spec.Rcon.Port > 0 {
		return server.Spec.Rcon.Port
	}
	return DefaultRconPort
}

func graceSeconds(server *v1alpha1.MinecraftServer) int64 {
	if server.Spec.Lifecycle.TerminationGracePeriodSeconds > 0 {
		return server.Spec.Lifecycle.TerminationGracePeriodSeconds
	}
	return defaultGraceSeconds
}

func rconAddress(server *v1alpha1.MinecraftServer) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", server.Name, server.Namespace, rconPort(server))
}

func buildHeadlessService(server *v1alpha1.MinecraftServer) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      headlessServiceName(server.Name),
			Namespace: server.Namespace,
			Labels:    labelsFor(server),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  selectorFor(server),
			Ports:     servicePorts(server),
		},
	}
	return svc
}

func buildClientService(server *v1alpha1.MinecraftServer) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      server.Name,
			Namespace: server.Namespace,
			Labels:    labelsFor(server),
		},
		Spec: corev1.ServiceSpec{
			Selector: selectorFor(server),
			Ports:    servicePorts(server),
		},
	}
}

func servicePorts(server *v1alpha1.MinecraftServer) []corev1.ServicePort {
	ports := []corev1.ServicePort{{
		Name:       "game",
		Port:       GamePort,
		TargetPort: intstr.FromInt32(GamePort),
		Protocol:   corev1.ProtocolTCP,
	}}
	if server.Spec.Rcon.Enabled {
		p := rconPort(server)
		ports = append(ports, corev1.ServicePort{
			Name:       "rcon",
			Port:       p,
			TargetPort: intstr.FromInt32(p),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	return ports
}

// readinessProbe uses the declared HTTP health endpoint when present, otherwise TCP on the game port.
func readinessProbe(server *v1alpha1.MinecraftServer) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        healthHandler(server),
		InitialDelaySeconds: 20,
		PeriodSeconds:       10,
		FailureThreshold:    6,
	}
}

// startupProbe outlasts operator startup timeout and initial backoff so kubelet cannot create an uncounted restart loop.
func startupProbe(server *v1alpha1.MinecraftServer) *corev1.Probe {
	const period = 10
	budget := startupTimeout(server) + autoRestartBaseBackoff
	return &corev1.Probe{
		ProbeHandler:     healthHandler(server),
		PeriodSeconds:    period,
		FailureThreshold: int32((budget+period*time.Second-1)/(period*time.Second)) + 1,
	}
}

func livenessProbe(server *v1alpha1.MinecraftServer) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:     healthHandler(server),
		PeriodSeconds:    20,
		TimeoutSeconds:   5,
		FailureThreshold: 6,
	}
}

func healthHandler(server *v1alpha1.MinecraftServer) corev1.ProbeHandler {
	if hp := server.Spec.Startup.HealthHTTPPort; hp > 0 {
		path := server.Spec.Startup.HealthHTTPPath
		if path == "" {
			path = "/healthz"
		}
		return corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(hp)}}
	}
	return corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(GamePort)}}
}

// buildStatefulSet supplies shutdown grace; the reconciler flushes over RCON before scaling down.
func buildStatefulSet(server *v1alpha1.MinecraftServer, replicas int32, felisImage string, gateProbe ...string) (*appsv1.StatefulSet, error) {
	o := podOptions{FelisImage: felisImage}
	if len(gateProbe) > 0 {
		o.GateProbe = gateProbe[0]
	}
	return buildStatefulSetWith(server, replicas, o)
}

type podOptions struct {
	FelisImage string
	GateProbe  string
	// ArtifactCache empty downloads directly from upstream.
	ArtifactCache string
}

func buildStatefulSetWith(server *v1alpha1.MinecraftServer, replicas int32, o podOptions) (*appsv1.StatefulSet, error) {
	felisImage := o.FelisImage
	var gateProbe []string
	if o.GateProbe != "" {
		gateProbe = []string{o.GateProbe}
	}
	storageSize := server.Spec.Storage.Size
	if storageSize == "" {
		storageSize = defaultStorageSize
	}
	storageQty, err := resource.ParseQuantity(storageSize)
	if err != nil {
		return nil, fmt.Errorf("invalid storage size %q: %w", storageSize, err)
	}

	container := corev1.Container{
		Name:      containerName,
		Image:     server.Spec.Image,
		Stdin:     true,
		Resources: server.Spec.Resources,
		Env:       buildEnv(server),
		Ports: []corev1.ContainerPort{
			{Name: "game", ContainerPort: GamePort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dataMountPath},
		},
		ReadinessProbe: readinessProbe(server),
		StartupProbe:   startupProbe(server),
		LivenessProbe:  livenessProbe(server),
		// The root filesystem remains writable because arbitrary server images may unpack runtimes or temporary files outside /data.
		SecurityContext: hardenedContainerSecurityContext(false),
	}
	if hp := server.Spec.Startup.HealthHTTPPort; hp > 0 {
		container.Ports = append(container.Ports, corev1.ContainerPort{
			Name: "health", ContainerPort: hp, Protocol: corev1.ProtocolTCP,
		})
	}
	if len(server.Spec.Args) > 0 {
		container.Args = append([]string(nil), server.Spec.Args...)
	}
	if server.Spec.Rcon.Enabled {
		container.Ports = append(container.Ports, corev1.ContainerPort{
			Name: "rcon", ContainerPort: rconPort(server), Protocol: corev1.ProtocolTCP,
		})
	}

	// Prepare volume ownership before forwarding config, then wait for egress policy before executing the server image.
	var initContainers []corev1.Container
	if felisImage != "" {
		initContainers = append(initContainers, prepareDataInitContainer(felisImage))
		if server.Labels[v1alpha1.LabelSystemRole] != naming.SystemLoginServer {
			initContainers = append(initContainers, forwardingInitContainer(felisImage))
			if server.Labels[v1alpha1.LabelSystemRole] == naming.SystemLobbyServer {
				container.Env = append(container.Env, corev1.EnvVar{Name: "FELIS_MANAGED_FORWARDING", Value: "true"})
			}
		}
		gate := egressGateInitContainer(felisImage)
		if server.Spec.NodeName != "" || (len(gateProbe) > 0 && gateProbe[0] != "") {
			gate.Command = append(gate.Command[:len(gate.Command)-1], "--positive-probe", "kube-dns.kube-system.svc:53")
			if len(gateProbe) > 0 && gateProbe[0] != "" {
				gate.Command = append(gate.Command, "--probe", gateProbe[0])
			}
		}
		initContainers = append(initContainers, gate)
	}

	grace := graceSeconds(server)
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: dataVolumeName},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storageQty},
			},
		},
	}
	if sc := server.Spec.Storage.StorageClassName; sc != "" {
		pvc.Spec.StorageClassName = &sc
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      server.Name,
			Namespace: server.Namespace,
			Labels:    labelsFor(server),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: headlessServiceName(server.Name),
			Selector:    &metav1.LabelSelector{MatchLabels: selectorFor(server)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabelsFor(server)},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: &grace,
					InitContainers:                initContainers,
					Containers:                    []corev1.Container{container},
					// Disable service-account token automount because game plugins must not authenticate to Kubernetes.
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext:              gamePodSecurityContext(),
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{pvc},
			// Retain world claims after StatefulSet deletion; only the reaper removes archived world volumes.
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
	}
	if server.Spec.Storage.ClaimName != "" {
		sts.Spec.VolumeClaimTemplates = nil
		sts.Spec.Template.Spec.Volumes = append(sts.Spec.Template.Spec.Volumes, corev1.Volume{Name: dataVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: server.WorldPVC()}}})
	}
	if server.Spec.NodeName != "" {
		sts.Spec.Template.Spec.NodeSelector = map[string]string{placement.LabelIdentity: server.Spec.NodeName}
	}
	if server.Spec.Software != nil {
		if err := applySoftware(&sts.Spec.Template.Spec, server, o); err != nil {
			return nil, err
		}
	}
	return sts, nil
}

const (
	felisBinVolume = "felis-bin"
	felisBinDir    = "/felis/bin"
	felisBinCopy   = felisBinDir + "/felis"
	installTmp     = "install-tmp"
	envSoftware    = "FELIS_SOFTWARE"
	envArtifacts   = "FELIS_ARTIFACT_CACHE"
)

func SoftwareJSON(s *v1alpha1.SoftwareSpec) (string, error) {
	b, err := json.Marshal(software.Spec{
		Type:             software.Type(s.Type),
		MinecraftVersion: s.MinecraftVersion,
		Build:            s.Build,
		Java:             int(s.Java),
		Jar:              s.Jar,
		Forwarding:       s.Forwarding,
	})
	return string(b), err
}

// applySoftware runs install after the egress gate with server resources but without RCON or forwarding credentials.
func applySoftware(pod *corev1.PodSpec, server *v1alpha1.MinecraftServer, o podOptions) error {
	if o.FelisImage == "" {
		return fmt.Errorf("server %s runs server software from the runtime image, which needs the felis image (FELIS_IMAGE) for its install steps", server.Name)
	}
	spec, err := SoftwareJSON(server.Spec.Software)
	if err != nil {
		return err
	}
	pod.Volumes = append(pod.Volumes,
		corev1.Volume{Name: felisBinVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: quantityPtr("256Mi")}}},
		corev1.Volume{Name: installTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: quantityPtr("2Gi")}}},
	)
	binRO := corev1.VolumeMount{Name: felisBinVolume, MountPath: felisBinDir, ReadOnly: true}
	data := corev1.VolumeMount{Name: dataVolumeName, MountPath: dataMountPath}

	installSelf := corev1.Container{
		Name:            "install-self",
		Image:           o.FelisImage,
		Command:         []string{felisBinaryPath, "install-self", "--to", felisBinDir},
		VolumeMounts:    []corev1.VolumeMount{{Name: felisBinVolume, MountPath: felisBinDir}},
		Resources:       initContainerResources(),
		SecurityContext: hardenedContainerSecurityContext(true),
	}
	install := corev1.Container{
		Name:    "install-server",
		Image:   server.Spec.Image,
		Command: []string{felisBinCopy, "install-server", "--data", dataMountPath},
		Env: []corev1.EnvVar{
			{Name: envSoftware, Value: spec},
			{Name: envArtifacts, Value: o.ArtifactCache},
			{Name: "HOME", Value: "/tmp"},
		},
		WorkingDir:      dataMountPath,
		VolumeMounts:    []corev1.VolumeMount{data, binRO, {Name: installTmp, MountPath: "/tmp"}},
		Resources:       server.Spec.Resources,
		SecurityContext: hardenedContainerSecurityContext(true),
	}
	forwarding := forwardingInitContainer(o.FelisImage)
	forwarding.Env = append(forwarding.Env, corev1.EnvVar{Name: envSoftware, Value: spec})

	var inits []corev1.Container
	inits = append(inits, installSelf)
	for _, c := range pod.InitContainers {
		switch c.Name {
		case "prepare-data", "egress-gate":
			inits = append(inits, c)
		}
	}
	inits = append(inits, install, forwarding)
	pod.InitContainers = inits

	main := &pod.Containers[0]
	main.Command = []string{felisBinCopy, "launch", "--data", dataMountPath}
	main.WorkingDir = dataMountPath
	main.Env = append(main.Env, corev1.EnvVar{Name: envSoftware, Value: spec})
	main.VolumeMounts = append(main.VolumeMounts, binRO)
	return nil
}

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func buildEnv(server *v1alpha1.MinecraftServer) []corev1.EnvVar {
	var env []corev1.EnvVar
	if mem := server.Spec.JavaMemory; mem != "" {
		env = append(env, corev1.EnvVar{Name: "JAVA_MEMORY", Value: mem})
	}
	if len(server.Spec.JavaFlags) > 0 {
		env = append(env, corev1.EnvVar{Name: "JAVA_FLAGS", Value: joinFlags(server.Spec.JavaFlags)})
	}
	for _, e := range server.Spec.Env {
		env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}
	if server.Spec.Rcon.Enabled && server.Spec.Rcon.SecretRef.Name != "" {
		env = append(env,
			corev1.EnvVar{Name: "RCON_PORT", Value: strconv.Itoa(int(rconPort(server)))},
			corev1.EnvVar{Name: "RCON_PASSWORD", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: server.Spec.Rcon.SecretRef.Name},
					Key:                  server.Spec.Rcon.SecretRef.Key,
				},
			}},
		)
	}
	// The internal token requires both reserved login name and setup-owned role label so a legacy user server cannot receive it.
	if server.Name == naming.SystemLoginServer &&
		server.Labels[v1alpha1.LabelSystemRole] == naming.SystemLoginServer {
		env = append(env, corev1.EnvVar{
			Name: envServiceToken,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: naming.LimboTokenSecretName},
					Key:                  naming.ServiceTokenSecretKey,
				},
			},
		})
	}
	// All backends need the shared forwarding key; network policy enforces proxy ingress and Paper init writes config for images that ignore the env.
	env = append(env, forwardingSecretEnvVar())
	return env
}

// forwardingSecretEnvVar is optional so non-modern-forwarding installs can start without a Secret.
func forwardingSecretEnvVar() corev1.EnvVar {
	return corev1.EnvVar{
		Name: envForwardingSecret,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: naming.ForwardingSecretName},
				Key:                  naming.ForwardingSecretKey,
				Optional:             boolPtr(true),
			},
		},
	}
}

// forwardingInitContainer merges Paper forwarding config as the game UID after prepare-data; Limbo handles its own format.
func forwardingInitContainer(felisImage string) corev1.Container {
	return corev1.Container{
		Name:    "init-forwarding",
		Image:   felisImage,
		Command: []string{felisBinaryPath, "init-forwarding"},
		Env:     []corev1.EnvVar{forwardingSecretEnvVar()},
		VolumeMounts: []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dataMountPath},
		},
		Resources:       initContainerResources(),
		SecurityContext: hardenedContainerSecurityContext(true),
	}
}

// egressGateWait bounds the policy-programming wait with margin for loaded nodes.
const egressGateWait = 30 * time.Second

// The server egress gate waits for policy but eventually fails open because explicit allow CIDRs can legitimately keep its probe reachable.
func egressGateInitContainer(felisImage string) corev1.Container {
	return corev1.Container{
		Name:  "egress-gate",
		Image: felisImage,
		Command: []string{felisBinaryPath, "egress-gate",
			"--wait", egressGateWait.String(), "--fail-open"},
		Resources:       initContainerResources(),
		SecurityContext: hardenedContainerSecurityContext(true),
	}
}

// prepareDataInitContainer needs CHOWN and DAC_OVERRIDE because hostPath ignores fsGroup and private directories require traversal rights.
func prepareDataInitContainer(felisImage string) corev1.Container {
	return corev1.Container{
		Name:    "prepare-data",
		Image:   felisImage,
		Command: []string{felisBinaryPath, "init-volume", "--data", dataMountPath},
		VolumeMounts: []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dataMountPath},
		},
		Resources: initContainerResources(),
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                int64Ptr(0),
			RunAsGroup:               int64Ptr(0),
			RunAsNonRoot:             boolPtr(false),
			Privileged:               boolPtr(false),
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"CHOWN", "DAC_OVERRIDE"},
			},
		},
	}
}

// gamePodSecurityContext enforces the game UID regardless of image USER; OnRootMismatch avoids repeated ownership walks.
func gamePodSecurityContext() *corev1.PodSecurityContext {
	onRootMismatch := corev1.FSGroupChangeOnRootMismatch
	return &corev1.PodSecurityContext{
		RunAsNonRoot:        boolPtr(true),
		RunAsUser:           int64Ptr(naming.GameUID),
		RunAsGroup:          int64Ptr(naming.GameGID),
		FSGroup:             int64Ptr(naming.GameGID),
		FSGroupChangePolicy: &onRootMismatch,
		SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// hardenedContainerSecurityContext drops capabilities and blocks privilege gain; read-only root applies to platform init containers.
func hardenedContainerSecurityContext(readOnlyRoot bool) *corev1.SecurityContext {
	sc := &corev1.SecurityContext{
		Privileged:               boolPtr(false),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	if readOnlyRoot {
		sc.ReadOnlyRootFilesystem = boolPtr(true)
	}
	return sc
}

// Init containers cap memory but omit CPU limits so large ownership walks do not exhaust the startup budget.
func initContainerResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
}

func boolPtr(b bool) *bool { return &b }

func int64Ptr(i int64) *int64 { return &i }

func joinFlags(flags []string) string {
	out := ""
	for i, f := range flags {
		if i > 0 {
			out += " "
		}
		out += f
	}
	return out
}
