package operator

import (
	"fmt"
	"strconv"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// envServiceToken is the environment variable the felis-limbo login plugin reads
// its internal-API bearer credential from. It is injected ONLY into the login
// system server (see buildEnv), sourced from a Secret, never a literal.
const envServiceToken = "FELIS_SERVICE_TOKEN"

// envForwardingSecret is the environment variable a backend reads the Velocity
// modern-forwarding secret from. Unlike the service token it goes to EVERY backend
// (see buildEnv), because Velocity's forwarding mode is proxy-wide.
const envForwardingSecret = "FELIS_FORWARDING_SECRET"

// Workload constants shared by the builders.
const (
	// GamePort is the Minecraft TCP port the proxy and readiness probe target.
	GamePort int32 = 25565

	// DefaultRconPort is the RCON port used when a server does not override
	// spec.rcon.port (see rconPort). It is exported because the platform package's
	// allow-rcon NetworkPolicy opens this port for the control plane — sharing the
	// constant keeps the policy port and the container's default RCON port a single
	// source of truth, so the operator's prober can always reach a default-port
	// server through the fence.
	DefaultRconPort int32 = 25575

	containerName  = "minecraft"
	dataVolumeName = "world"
	dataMountPath  = "/data"

	// ManagedByValue / ComponentValue are the values of the LabelManagedBy /
	// LabelComponent labels stamped on every per-server pod (see labelsFor). They
	// are exported because the platform package's minecraft-namespace
	// NetworkPolicies select server pods by exactly these labels — keeping the
	// selector and the pod labels a single source of truth, so an isolation policy
	// can never silently stop matching the pods it is meant to fence.
	ManagedByValue = "felis-operator"
	ComponentValue = "server"

	defaultGraceSeconds int64  = 300
	defaultStorageSize  string = "8Gi"
)

// selectorFor returns the immutable selector labels (a StatefulSet selector
// must never change after creation, so it carries only the server identity).
func selectorFor(server *v1alpha1.MinecraftServer) map[string]string {
	return map[string]string{v1alpha1.LabelServer: server.Name}
}

// labelsFor returns the full label set applied to managed objects.
func labelsFor(server *v1alpha1.MinecraftServer) map[string]string {
	return map[string]string{
		v1alpha1.LabelServer:    server.Name,
		v1alpha1.LabelManagedBy: ManagedByValue,
		v1alpha1.LabelComponent: ComponentValue,
	}
}

func headlessServiceName(name string) string { return name + "-hl" }

// rconPort resolves the RCON port, defaulting to the conventional DefaultRconPort.
func rconPort(server *v1alpha1.MinecraftServer) int32 {
	if server.Spec.Rcon.Port > 0 {
		return server.Spec.Rcon.Port
	}
	return DefaultRconPort
}

// graceSeconds resolves the pod termination grace period (spec §7).
func graceSeconds(server *v1alpha1.MinecraftServer) int64 {
	if server.Spec.Lifecycle.TerminationGracePeriodSeconds > 0 {
		return server.Spec.Lifecycle.TerminationGracePeriodSeconds
	}
	return defaultGraceSeconds
}

// rconAddress is the in-cluster RCON endpoint the operator probes for readiness.
func rconAddress(server *v1alpha1.MinecraftServer) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", server.Name, server.Namespace, rconPort(server))
}

// preStopScript is the operator-injected graceful-shutdown sequence (spec §7):
// flush the world, then stop the server, both over RCON. It relies on rcon-cli
// being present in the Felis base image and reading the RCON_* env injected
// alongside it.
func preStopScript(server *v1alpha1.MinecraftServer) string {
	port := rconPort(server)
	return fmt.Sprintf(
		`rcon-cli --port %d --password "$RCON_PASSWORD" save-all flush; rcon-cli --port %d --password "$RCON_PASSWORD" stop`,
		port, port,
	)
}

// buildHeadlessService backs the StatefulSet's stable network identity.
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

// buildClientService is the stable ClusterIP the proxy and operator dial.
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

// readinessProbe selects the pod readiness probe. By default it is a plain TCP
// check on the game port; when the server declares an HTTP health port
// (StartupSpec.HealthHTTPPort > 0) it becomes an HTTP GET on that port, so an
// RCON-less loader's own "started" signal — not the mere fact that the game
// socket is bound — gates readiness. Timings are identical across both modes.
func readinessProbe(server *v1alpha1.MinecraftServer) *corev1.Probe {
	probe := &corev1.Probe{
		InitialDelaySeconds: 20,
		PeriodSeconds:       10,
		FailureThreshold:    6,
	}
	if hp := server.Spec.Startup.HealthHTTPPort; hp > 0 {
		path := server.Spec.Startup.HealthHTTPPath
		if path == "" {
			path = "/healthz"
		}
		probe.ProbeHandler = corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(hp)},
		}
		return probe
	}
	probe.ProbeHandler = corev1.ProbeHandler{
		TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(GamePort)},
	}
	return probe
}

// buildStatefulSet renders the workload for replicas in {0,1}. It is where
// graceful shutdown is injected: the pod gets terminationGracePeriodSeconds and
// (when enabled) a preStop RCON save+stop hook.
func buildStatefulSet(server *v1alpha1.MinecraftServer, replicas int32) (*appsv1.StatefulSet, error) {
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
		Resources: server.Spec.Resources,
		Env:       buildEnv(server),
		Ports: []corev1.ContainerPort{
			{Name: "game", ContainerPort: GamePort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dataMountPath},
		},
		// Readiness defaults to a plain TCP check (spec §5: readinessProbe is only
		// tcpSocket; the RCON gate is enforced by the operator, not the kubelet).
		// An RCON-less loader may instead publish an HTTP health endpoint (see
		// StartupSpec.HealthHTTPPort) that reports true readiness — used below when
		// set.
		ReadinessProbe: readinessProbe(server),
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
		if server.Spec.Lifecycle.PreStopSaveAndStop {
			container.Lifecycle = &corev1.Lifecycle{
				PreStop: &corev1.LifecycleHandler{
					Exec: &corev1.ExecAction{
						Command: []string{"/bin/sh", "-c", preStopScript(server)},
					},
				},
			}
		}
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
				ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(server)},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: &grace,
					Containers:                    []corev1.Container{container},
					// A Minecraft server runs untrusted user worlds and plugins and
					// has no business calling the K8s API, so its pod must NOT carry the
					// default ServiceAccount token: a compromised plugin could otherwise
					// authenticate as the namespace default SA (spec §21: user servers
					// default to no SA-token mount). The pod keeps the default SA but
					// with automounting explicitly disabled.
					AutomountServiceAccountToken: boolPtr(false),
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{pvc},
		},
	}
	return sts, nil
}

// buildEnv assembles the container environment: heap sizing, user-supplied
// vars, and the RCON_* pair (password sourced from the referenced Secret, never
// inlined into the CRD).
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
	// The login system server is the ONE workload that authenticates to the
	// felis-api internal face (its felis-limbo plugin mints bind codes and polls
	// link status), so it — and only it — receives the service token. Injected
	// from a Secret in this namespace, never inlined into the CRD (the same
	// discipline as RCON_PASSWORD above; the CRD's EnvVar type has no valueFrom
	// precisely so a user server cannot mount an arbitrary secret). Require both
	// the reserved name and the setup-owned system-role label: the label prevents
	// a legacy user server named "login" from receiving the token after upgrade.
	// The Secret must exist in this (minecraft) namespace; `felis setup` replicates
	// it there from the control namespace before creating this server.
	if server.Name == naming.SystemLoginServer &&
		server.Labels[v1alpha1.LabelSystemRole] == naming.SystemLoginServer {
		env = append(env, corev1.EnvVar{
			Name: envServiceToken,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: naming.ServiceTokenSecretName},
					Key:                  naming.ServiceTokenSecretKey,
				},
			},
		})
	}
	// The Velocity modern-forwarding secret goes to EVERY backend, system and user
	// alike — not because user servers are trusted, but because Velocity's forwarding
	// mode is one proxy-wide setting: with it on, a backend that cannot verify the
	// signed handshake rejects every login the proxy sends it. Withholding the secret
	// from user servers would not harden them, it would simply make them unjoinable.
	// It is the backend's proof that a login really came from the proxy (and so that
	// the player's UUID is Mojang-verified, not offline-derived) — the pod-level fence
	// against bypassing the proxy is the NetworkPolicy, not this value's secrecy.
	//
	// A user server is built from an operator-typed Dockerfile, so Felis cannot make it
	// consume this; the two images Felis does build (deploy/limbo, deploy/lobby) read it
	// in their entrypoints and refuse to start without it. Optional so a cluster whose
	// proxy is not in modern mode — no Secret provisioned — still schedules its pods
	// instead of wedging them all in CreateContainerConfigError.
	env = append(env, corev1.EnvVar{
		Name: envForwardingSecret,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: naming.ForwardingSecretName},
				Key:                  naming.ForwardingSecretKey,
				Optional:             boolPtr(true),
			},
		},
	})
	return env
}

func boolPtr(b bool) *bool { return &b }

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
