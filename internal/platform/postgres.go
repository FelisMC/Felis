package platform

import (
	"fmt"

	"felis.lolicon.best/internal/backupjob"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The control-plane database. It used to be the host's own PostgreSQL, which the
// installer had to find in the distro's repositories, initialise, open a hole in
// the firewall for and keep in step with the distro's major version — the part of
// an install that broke first whenever a host differed from the ones it was tried
// on. It is now the official image, pinned by digest (Params.PostgresImage), run
// by k3s beside the rest of the control plane.
const (
	// PostgresName names the Deployment, the Service, the NetworkPolicy and the
	// Secret. deploy/bootstrap.sh and `felis db` reach the pod as
	// deploy/<PostgresName> in the control namespace.
	PostgresName = "felis-postgres"
	// PostgresContainer is the database container `kubectl exec -c` targets.
	PostgresContainer = "postgres"
	// PostgresPort is the port the server listens on, in the pod and on the
	// Service.
	PostgresPort int32 = 5432
	// PostgresHostPort is the node-loopback port the host's own tools reach the
	// database on (`felis migrate`, the watchdog, off-site backups): a hostPort
	// bound to 127.0.0.1 only, the path the registry's node-side pulls already
	// take (registryLoopbackHost). It sits clear of 5432 so a host PostgreSQL
	// left running from an older install does not collide with it.
	PostgresHostPort int32 = 15432
	// PostgresDataHostPath is the node directory the cluster lives under,
	// mounted at postgresDataMount. deploy/bootstrap.sh creates it owned by
	// postgresUID, mode 0700, before the first apply; `felis uninstall` keeps
	// it unless --purge. A hostPath instead of a local-path PVC so the data sits
	// at a fixed, documented place under /var/lib/felis that survives the
	// Deployment, the namespace and k3s itself being deleted.
	PostgresDataHostPath = "/var/lib/felis/postgres"
	// PostgresSuperuserSecret holds the postgres role's password, which the
	// image's entrypoint needs only to initialise an empty data directory.
	// deploy/bootstrap.sh creates it once with a random value and nothing reads
	// it back: pg_hba.conf (postgresHBA) refuses the postgres role over TCP, and
	// the socket inside the container trusts local connections.
	PostgresSuperuserSecret = "felis-postgres"
	// PostgresSuperuserSecretKey is the key in PostgresSuperuserSecret.
	PostgresSuperuserSecretKey = "superuser-password"
	// PostgresSocketDir is the image's socket directory, which `felis db` and
	// deploy/bootstrap.sh connect through under kubectl exec.
	PostgresSocketDir = "/var/run/postgresql"

	postgresHBAConfigMap = "felis-postgres-hba"
	postgresHBAMount     = "/etc/felis-postgres"
	// postgresDataMount is the image's VOLUME. The image's PGDATA is
	// <mount>/<major>/docker, so a new major starts beside the old one's data
	// instead of on top of it; deploy/bootstrap.sh refuses to run an image whose
	// major finds only another major's cluster there.
	postgresDataMount = "/var/lib/postgresql"
	postgresDataVol   = "data"
	postgresSocketVol = "socket"
	postgresShmVol    = "shm"
	postgresHBAVol    = "hba"
	// postgresUID is the postgres account the official Debian image creates.
	// Running as it (instead of root) skips the entrypoint's chown pass, which
	// is why the installer owns the data directory to it up front.
	postgresUID int64 = 999
)

// postgresHBA is the pg_hba.conf the server runs under, from a ConfigMap
// (hba_file) instead of the one initdb writes into the data directory, so the
// rules are the rendered ones on every start, whatever the cluster was
// initialised with. The container's socket trusts every role: only something
// that can already exec into the pod reaches it. Over TCP the postgres
// superuser is refused outright and every other role needs its password; which
// pods may open a TCP connection at all is postgresIngressPolicy's business.
const postgresHBA = `# Rendered by felis (internal/platform/postgres.go); edits are overwritten.
local all all                trust
host  all postgres 0.0.0.0/0 reject
host  all postgres ::/0      reject
host  all all      0.0.0.0/0 scram-sha-256
host  all all      ::/0      scram-sha-256
`

// postgresPingCommand answers only once the server that takes connections is
// up: the entrypoint's initialisation server listens on no TCP address.
var postgresPingCommand = []string{"pg_isready", "-q", "-h", "127.0.0.1", "-p", fmt.Sprint(PostgresPort)}

// PostgresObjects renders the database: its control namespace, pg_hba.conf, the
// ingress fence, the Deployment and the Service. deploy/bootstrap.sh applies
// exactly these (`felis manifests --only postgres`) before it runs migrations,
// which is before the rest of the bundle can render; Objects includes them
// again so a full apply never prunes or drifts them.
func PostgresObjects(p Params) []Object {
	p = p.withDefaults()
	return []Object{
		namespaceObject(p.ControlNamespace),
		postgresHBAConfig(p),
		PostgresIngressPolicy(p),
		postgresDeployment(p),
		postgresService(p),
	}
}

// postgresLabels are the database's labels. Like the registry it is not
// part-of=felis-control-plane, which keeps it out of the RCON peer.
func postgresLabels() map[string]string {
	return map[string]string{
		LabelName:      appName,
		LabelComponent: ComponentPostgres,
	}
}

func postgresHBAConfig(p Params) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: postgresHBAConfigMap, Namespace: p.ControlNamespace, Labels: postgresLabels()},
		Data:       map[string]string{"pg_hba.conf": postgresHBA},
	}
}

// postgresDeployment renders the single database pod.
//
// Recreate, one replica: two servers on one data directory corrupt it, and a
// rolling update would start the second before stopping the first. The pod
// runs as the image's postgres account under the control plane's hardening
// (read-only root, no capabilities); everything the server writes lands on the
// data hostPath, the socket emptyDir or the /dev/shm emptyDir (dynamic shared
// memory, which the runtime's 64Mi default would cap).
//
// Stopping: the image's STOPSIGNAL is SIGINT (fast shutdown: roll back open
// transactions, checkpoint, exit), which containerd sends in place of SIGTERM;
// SIGTERM would wait for felis-api's pooled connections to leave and run into
// the kill at the end of the grace period. The preStop hook asks for the same
// fast shutdown explicitly so the stop does not rest on the runtime honouring
// the image's signal.
func postgresDeployment(p Params) *appsv1.Deployment {
	labels := postgresLabels()
	hostPathDir := corev1.HostPathDirectory
	probe := func(period, timeout, failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: postgresPingCommand}},
			PeriodSeconds:    period,
			TimeoutSeconds:   timeout,
			FailureThreshold: failures,
		}
	}
	container := corev1.Container{
		Name:  PostgresContainer,
		Image: p.PostgresImage,
		// The image's entrypoint initialises an empty data directory, then execs
		// these arguments.
		Args: []string{"postgres", "-c", "hba_file=" + postgresHBAMount + "/pg_hba.conf"},
		Env: []corev1.EnvVar{{
			Name: "POSTGRES_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: PostgresSuperuserSecret},
				Key:                  PostgresSuperuserSecretKey,
				// Only initdb reads it: a cluster that already exists starts
				// without the Secret.
				Optional: boolPtr(true),
			}},
		}},
		Ports: []corev1.ContainerPort{{
			Name: PostgresContainer, ContainerPort: PostgresPort, Protocol: corev1.ProtocolTCP,
			HostPort: PostgresHostPort, HostIP: registryLoopbackHost,
		}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: postgresDataVol, MountPath: postgresDataMount},
			{Name: postgresSocketVol, MountPath: PostgresSocketDir},
			{Name: tmpVolume, MountPath: "/tmp"},
			{Name: postgresShmVol, MountPath: "/dev/shm"},
			{Name: postgresHBAVol, MountPath: postgresHBAMount, ReadOnly: true},
		},
		// Crash recovery after an unclean stop, or initdb on a slow disk, can
		// take minutes; liveness waits for the startup probe.
		StartupProbe:   probe(5, 5, 120),
		ReadinessProbe: probe(10, 5, 3),
		LivenessProbe:  probe(30, 10, 6),
		Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{
			Command: []string{"/bin/sh", "-c", `pg_ctl -D "$PGDATA" -m fast -w -t 50 stop`},
		}}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
		SecurityContext: hardenedContainerSecurityContext(),
	}
	shmLimit := resource.MustParse("256Mi")
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: PostgresName, Namespace: p.ControlNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector:                 controllerSelector(p),
					AutomountServiceAccountToken: boolPtr(false),
					EnableServiceLinks:           boolPtr(false),
					PriorityClassName:            controlPlanePriorityName,
					// pg_ctl's -t 50 in the preStop hook fits inside it.
					TerminationGracePeriodSeconds: int64Ptr(60),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   boolPtr(true),
						RunAsUser:      int64Ptr(postgresUID),
						RunAsGroup:     int64Ptr(postgresUID),
						FSGroup:        int64Ptr(postgresUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{container},
					Volumes: []corev1.Volume{
						{
							Name: postgresDataVol,
							VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
								Path: PostgresDataHostPath,
								// Directory, not DirectoryOrCreate: a directory
								// kubelet made would belong to root, and a missing
								// one means the installer did not run.
								Type: &hostPathDir,
							}},
						},
						{Name: postgresSocketVol, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: postgresShmVol, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium: corev1.StorageMediumMemory, SizeLimit: &shmLimit,
						}}},
						{Name: postgresHBAVol, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: postgresHBAConfigMap},
						}}},
					},
				},
			},
		},
	}
}

// postgresService is the name the pods reach the database by:
// felis-postgres.<control-ns>.svc:5432 (the pod config's [database] url).
func postgresService(p Params) *corev1.Service {
	labels := postgresLabels()
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: PostgresName, Namespace: p.ControlNamespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       PostgresContainer,
				Port:       PostgresPort,
				TargetPort: intstr.FromString(PostgresContainer),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// PostgresIngressPolicy admits TCP to the database from the pods that open it
// and nothing else in the cluster: felis-api, the reaper (which records what it
// archives) and the world-backup Jobs (which record each backup). The operator,
// restore and file Jobs and every game server hold no database credential and
// get no route. The node itself is always admitted by the policy controller,
// which is how the host's tools reach PostgresHostPort.
func PostgresIngressPolicy(p Params) *networkingv1.NetworkPolicy {
	p = p.withDefaults()
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(PostgresPort)
	inNamespace := func(ns string, pods map[string]string) networkingv1.NetworkPolicyPeer {
		return networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: pods},
		}
	}
	return netpol(PostgresName+"-ingress", p.ControlNamespace,
		metav1.LabelSelector{MatchLabels: postgresLabels()},
		[]networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{
				inNamespace(p.ControlNamespace, map[string]string{LabelPartOf: controlPlanePartOf, LabelComponent: ComponentAPI}),
				inNamespace(p.MinecraftNamespace, map[string]string{LabelPartOf: controlPlanePartOf, LabelComponent: ComponentReaper}),
				inNamespace(p.MinecraftNamespace, backupjob.PodSelector()),
			},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		}},
	)
}
