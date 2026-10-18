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

const (
	PostgresName            = "felis-postgres"
	PostgresContainer       = "postgres"
	PostgresPort      int32 = 5432
	// PostgresHostPort binds loopback for host tools and avoids collision with a host database on 5432.
	PostgresHostPort int32 = 15432
	// PostgresDataHostPath survives workload and namespace deletion; bootstrap creates it with the database UID and mode 0700.
	PostgresDataHostPath = "/var/lib/felis/postgres"
	// PostgresSuperuserSecret initializes empty data only; TCP superuser access is denied and container-local sockets are trusted.
	PostgresSuperuserSecret    = "felis-postgres"
	PostgresSuperuserSecretKey = "superuser-password"
	PostgresSocketDir          = "/var/run/postgresql"

	postgresHBAConfigMap = "felis-postgres-hba"
	postgresHBAMount     = "/etc/felis-postgres"
	// Postgres data lives under a major-specific subdirectory; bootstrap refuses to start a new major over another major's data.
	postgresDataMount = "/var/lib/postgresql"
	postgresDataVol   = "data"
	postgresSocketVol = "socket"
	postgresShmVol    = "shm"
	postgresHBAVol    = "hba"
	// postgresUID must match the image account and installer-created data directory ownership to avoid an entrypoint chown pass.
	postgresUID int64 = 999
)

// postgresHBA trusts local sockets, rejects TCP superusers and requires other TCP roles to authenticate.
const postgresHBA = `# Rendered by felis (internal/platform/postgres.go); edits are overwritten.
local all all                trust
host  all postgres 0.0.0.0/0 reject
host  all postgres ::/0      reject
host  all all      0.0.0.0/0 scram-sha-256
host  all all      ::/0      scram-sha-256
`

var postgresPingCommand = []string{"pg_isready", "-q", "-h", "127.0.0.1", "-p", fmt.Sprint(PostgresPort)}

func PostgresObjects(p Params) []Object {
	p = p.withDefaults()
	objs := []Object{namespaceObject(p.ControlNamespace)}
	if p.ControllerHA {
		objs = append(objs, ReplicatedStorageClass(p), postgresPVC(p))
	}
	return append(objs,
		postgresHBAConfig(p),
		PostgresIngressPolicy(p),
		postgresDeployment(p),
		postgresService(p),
	)
}

// Database labels omit part-of=felis-control-plane to exclude it from control-plane network peers.
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

// Use one replica with Recreate to prevent shared-directory corruption; preStop requests fast shutdown before grace expires.
func postgresDeployment(p Params) *appsv1.Deployment {
	labels := postgresLabels()
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
		Args:  []string{"postgres", "-c", "hba_file=" + postgresHBAMount + "/pg_hba.conf"},
		Env: []corev1.EnvVar{{
			Name: "POSTGRES_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: PostgresSuperuserSecret},
				Key:                  PostgresSuperuserSecretKey,
				// The initialization password is not required to start an existing cluster.
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
		// Wait for startup before liveness checks so initdb and crash recovery can finish on slow disks.
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
					NodeSelector:                  controllerSelector(p),
					AutomountServiceAccountToken:  boolPtr(false),
					EnableServiceLinks:            boolPtr(false),
					Tolerations:                   haTolerations(p),
					PriorityClassName:             controlPlanePriorityName,
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
						postgresDataVolume(p),
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

// postgresDataVolume uses replicated storage under controller HA and the fixed hostPath otherwise.
func postgresDataVolume(p Params) corev1.Volume {
	if p.ControllerHA {
		return corev1.Volume{Name: postgresDataVol, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: PostgresHAClaim}}}
	}
	hostPathDir := corev1.HostPathDirectory
	return corev1.Volume{
		Name: postgresDataVol,
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
			Path: PostgresDataHostPath,
			// Require an installer-created directory; DirectoryOrCreate would silently create root-owned database storage.
			Type: &hostPathDir,
		}},
	}
}

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

// PostgresIngressPolicy admits credential-bearing API, reaper and backup Pods; resident-node traffic remains outside policy blocking.
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
