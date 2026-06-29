package platform

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// This file renders the running control-plane workloads — the felis-api and
// felis-operator Deployments, the in-cluster image registry (Deployment +
// Service + PVC), and (when configured) the reaper CronJob. They are what make
// the RBAC and NetworkPolicy fence MEAN something: each Deployment runs as its
// matching control-plane SA (so the namespaced Roles actually bind to a workload)
// and stamps the recommended labels the RCON NetworkPolicy peer selects on (so
// the api console / operator prober can reach RCON through the fence).
// workloads_test.go evaluates those correspondences with the SAME selector
// machinery K8s uses, because no cluster runs here.
//
// The reaper CronJob (spec §18 three-clock retention) renders ONLY when the
// storage topology is supplied — WorldsHostPath + BackupPVC + ArchiveLocalPath,
// gated by reaperEnabled. It is opt-in-when-configured rather than always-on
// because archiving idle worlds means mounting where the worlds physically live,
// and the spec keeps that open (§18/§19: tarLocal-on-local-path is the starter,
// Longhorn/snapshot the documented evolution, and they do not share a mount
// model). The starter model — a node-local hostPath worlds-root mounted
// read-only — is the only one coherent with the operator's per-server
// ReadWriteOnce world PVCs (a shared RWX worlds mount would contradict them), so
// that is what renders; when the trio is absent no CronJob is emitted, which is
// the fail-safe choice for a workload that deletes PVCs. SHAPE-ASSERTED and
// runtime-unverified: the rendered CronJob is the correct K8s object, but whether
// the tar finds a world under <WorldsHostPath>/<pvc> on a given cluster depends on
// how that node's storage is arranged (stock local-path-provisioner uses
// PV-name paths, not <root>/<pvc>) and is not provable without a cluster — see the
// WorldsHostPath field doc. No nodeSelector is set: the single-node starter pins
// the worlds to one node implicitly; a multi-node deployment MUST add one (or the
// CronJob could schedule on a node where the hostPath is empty) — a hazard left on
// record here until multi-node retention is built.
const (
	// configSecretName / serviceTokenSecretName are referenced BY NAME and NEVER
	// rendered into the bundle: felis.toml carries the database URL (a credential)
	// and the service token is a credential, so writing either into a checked-in
	// manifest is a hard red line. The deployment provisions both Secrets
	// out-of-band before applying these workloads.
	configSecretName       = "felis-config"
	configSecretKey        = "felis.toml"
	configMountPath        = "/etc/felis"
	configFilePath         = "/etc/felis/felis.toml"
	felisBinaryPath        = "/usr/local/bin/felis"
	serviceTokenSecretName = "felis-service-token"
	serviceTokenSecretKey  = "token"

	// Ports, single-sourced with the entrypoints (cmd/felis). The api external
	// port must match server.listen in felis.toml (default 0.0.0.0:8080); that
	// agreement lives in the out-of-band config Secret and cannot be enforced here.
	apiExternalPort     int32 = 8080
	apiInternalPort     int32 = 8081
	apiHTTPSPort        int32 = 8443
	operatorMetricsPort int32 = 8080
	apiTLSSecretName          = "felis-api-tls"
	apiTLSMountPath           = "/etc/felis/tls"

	registryName        = "registry"
	registryDataPath    = "/var/lib/registry"
	registryStorageSize = "10Gi"

	configVolume   = "config"
	tmpVolume      = "tmp"
	registryVolume = "data"
	worldsVolume   = "worlds"
	backupVolume   = "backup"

	// worldsMountPath is where the reaper CronJob mounts the worlds-root (read-only).
	// It is the default of `felis reaper --worlds-root`; the resolver then reads each
	// world at <worldsMountPath>/<pvc>. Single-sourced with cmd/felis/reaper.go.
	worldsMountPath = "/worlds"

	// reaperSchedule is the daily retention cadence (spec §18: a daily batch). 04:00
	// is an off-peak window; the reaper itself is idempotent and run-once, so the
	// exact minute is not load-bearing. ConcurrencyPolicy=Forbid keeps a slow run
	// from overlapping the next day's.
	reaperSchedule = "0 4 * * *"

	// reaperStartingDeadlineSeconds bounds how late a missed run may still start (a
	// controller outage at 04:00 shouldn't silently skip retention) without letting
	// a long backlog pile up. reaperActiveDeadlineSeconds caps a single run so a
	// wedged archive can't hold the Forbid lock forever.
	reaperStartingDeadlineSeconds int64 = 300
	reaperActiveDeadlineSeconds   int64 = 3600
	reaperBackoffLimit            int32 = 2
	reaperHistoryLimit            int32 = 3

	// nonRootUID matches the tree's non-root identity convention
	// (internal/restore.defaultRunAsID).
	nonRootUID int64 = 1000
)

// Workloads renders the running control-plane: the felis-api Deployment, the
// felis-operator Deployment, and the in-cluster registry (Deployment + Service +
// PVC), plus the reaper CronJob when reaperEnabled(p). FelisImage is required —
// `felis manifests` enforces it (fail-loud), so a rendered bundle always names a
// concrete image.
func Workloads(p Params) []Object {
	p = p.withDefaults()
	objs := []Object{
		APIDeployment(p),
		apiService(p),
		OperatorDeployment(p),
		registryDeployment(p),
		registryService(p),
		registryPVC(p),
	}
	if reaperEnabled(p) {
		objs = append(objs, reaperCronJob(p))
	}
	return objs
}

// reaperEnabled reports whether the retention CronJob should render. It needs all
// three storage coordinates: WorldsHostPath (where worlds live, mounted to read
// them), BackupPVC (where archives are written) and ArchiveLocalPath (the mount
// path that must equal [archive] local_path so absolute archive refs resolve).
// Any missing ⇒ no CronJob (fail-safe). `felis manifests` enforces the trio
// together so a partial configuration fails loudly rather than silently dropping
// retention here.
func reaperEnabled(p Params) bool {
	return p.WorldsHostPath != "" && p.BackupPVC != "" && p.ArchiveLocalPath != ""
}

// APIDeployment renders the felis-api Deployment (spec §7). It runs as the
// felis-api SA (so APIMinecraftRole/APIBuildRole bind to a real workload) and
// carries controlPlanePodLabels(api), which the allow-rcon NetworkPolicy peer
// selects — that correspondence lets the console reach server RCON through the
// fence and is asserted in workloads_test.go.
//
// felis.toml is mounted read-only from a Secret (it carries the database URL, a
// credential, so it must never be a ConfigMap); FELIS_SERVICE_TOKEN comes from a
// second Secret by reference. FELIS_IMAGE is the felis image itself, so the
// restore executor launches `felis restore` with the same image. FELIS_BACKUP_PVC
// is rendered only when a backup PVC is named — otherwise the restore endpoint
// degrades to 503 rather than enqueuing a Job that cannot mount its backup.
func APIDeployment(p Params) *appsv1.Deployment {
	p = p.withDefaults()

	env := []corev1.EnvVar{
		{
			Name: "FELIS_SERVICE_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: serviceTokenSecretName},
					Key:                  serviceTokenSecretKey,
				},
			},
		},
		{Name: "FELIS_IMAGE", Value: p.FelisImage},
	}
	if p.BackupPVC != "" {
		env = append(env, corev1.EnvVar{Name: "FELIS_BACKUP_PVC", Value: p.BackupPVC})
	}

	container := corev1.Container{
		Name:    ComponentAPI,
		Image:   p.FelisImage,
		Command: []string{felisBinaryPath, "api"},
		Args: []string{
			"--config", configFilePath,
			"--internal-addr", fmt.Sprintf(":%d", apiInternalPort),
			"--https-addr", fmt.Sprintf(":%d", apiHTTPSPort),
			"--tls-cert", apiTLSMountPath + "/tls.crt",
			"--tls-key", apiTLSMountPath + "/tls.key",
		},
		Env: env,
		Ports: []corev1.ContainerPort{
			{Name: "external", ContainerPort: apiExternalPort, Protocol: corev1.ProtocolTCP},
			{Name: "https", ContainerPort: apiHTTPSPort, Protocol: corev1.ProtocolTCP},
			{Name: "internal", ContainerPort: apiInternalPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: configVolume, MountPath: configMountPath, ReadOnly: true},
			{Name: "tls", MountPath: apiTLSMountPath, ReadOnly: true},
			{Name: tmpVolume, MountPath: "/tmp"},
		},
		Resources:       controlPlaneResources(),
		SecurityContext: hardenedContainerSecurityContext(),
	}

	volumes := []corev1.Volume{
		{
			Name: configVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: configSecretName},
			},
		},
		{
			Name: "tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: apiTLSSecretName},
			},
		},
		{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}

	return controlPlaneDeployment(p, SAAPI, container, volumes)
}

// apiService exposes the built-in HTTPS panel/API origin as a stable NodePort.
func apiService(p Params) *corev1.Service {
	p = p.withDefaults()
	labels := controlPlanePodLabels(ComponentAPI)
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: SAAPI, Namespace: p.ControlNamespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "https",
				Port:       443,
				TargetPort: intstr.FromString("https"),
				NodePort:   p.PanelNodePort,
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// OperatorDeployment renders the felis-operator Deployment (spec §5). It runs as
// the felis-operator SA and carries controlPlanePodLabels(operator), the second
// pod the allow-rcon peer admits (the readiness prober dials RCON). It takes NO
// config Secret: the operator reads everything from flags + the in-cluster API,
// so it never holds the database URL — a deliberately smaller attack surface than
// the api. It watches the minecraft namespace (--namespace) while running in the
// control namespace, exactly the split cmd/felis/operator.go documents.
func OperatorDeployment(p Params) *appsv1.Deployment {
	p = p.withDefaults()

	container := corev1.Container{
		Name:    ComponentOperator,
		Image:   p.FelisImage,
		Command: []string{felisBinaryPath, "operator"},
		Args: []string{
			"--namespace", p.MinecraftNamespace,
			"--metrics-bind-address", fmt.Sprintf(":%d", operatorMetricsPort),
		},
		Ports: []corev1.ContainerPort{
			{Name: "metrics", ContainerPort: operatorMetricsPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: tmpVolume, MountPath: "/tmp"},
		},
		Resources:       controlPlaneResources(),
		SecurityContext: hardenedContainerSecurityContext(),
	}

	volumes := []corev1.Volume{
		{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}

	return controlPlaneDeployment(p, SAOperator, container, volumes)
}

// reaperCronJob renders the world-retention CronJob (spec §18). It runs the
// `felis reaper` run-once entrypoint on a daily cadence — the CronJob, not the
// process, owns scheduling, so the reaper stays idempotent and restart-safe.
//
// Identity & reach. It runs as SAReaper (felis-reaper), the only SA holding
// minecraftservers:[get,patch] + persistentvolumeclaims:delete in the minecraft
// namespace (rbac.go ReaperRole). Unlike the build/restore/registry pods — which
// set AutomountServiceAccountToken=false because they never call the K8s API — the
// reaper LEGITIMATELY patches MinecraftServers (flip desiredState) and deletes
// world PVCs, so its SA token is left to auto-mount (nil). Its pod carries
// controlPlanePodLabels(ComponentReaper); reaper is deliberately excluded from the
// RCON NetworkPolicy peer (component ∉ {api,operator}), asserted in
// workloads_test.go — the reaper never opens an RCON connection.
//
// Mounts (the storage crux). felis.toml is mounted read-only from the config
// Secret (it carries the DB URL). The worlds-root is a node-local hostPath mounted
// READ-ONLY at /worlds: the reaper only reads worlds to tar them; deleting a world
// is a K8s API call (DeletePVC), never an rm, so the mount never needs write. The
// backup PVC is mounted READ-WRITE at p.ArchiveLocalPath — which MUST equal
// felis.toml [archive] local_path, because tarLocal writes archive refs as absolute
// paths under it and the restore Job later mounts the same PVC at the same path to
// resolve them (see the ArchiveLocalPath field doc). A /tmp emptyDir absorbs writes
// under the read-only root filesystem. hostPath type Directory fails the pod loud
// if the worlds-root is absent, rather than silently creating an empty dir and
// archiving nothing.
//
// Pre-conditions are the caller's: reaperCronJob assumes reaperEnabled(p) — it
// dereferences WorldsHostPath / BackupPVC / ArchiveLocalPath without re-checking.
func reaperCronJob(p Params) *batchv1.CronJob {
	p = p.withDefaults()
	labels := controlPlanePodLabels(ComponentReaper)
	hostPathDir := corev1.HostPathDirectory

	container := corev1.Container{
		Name:    ComponentReaper,
		Image:   p.FelisImage,
		Command: []string{felisBinaryPath, "reaper"},
		Args: []string{
			"--config", configFilePath,
			"--worlds-root", worldsMountPath,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: configVolume, MountPath: configMountPath, ReadOnly: true},
			{Name: worldsVolume, MountPath: worldsMountPath, ReadOnly: true},
			{Name: backupVolume, MountPath: p.ArchiveLocalPath},
			{Name: tmpVolume, MountPath: "/tmp"},
		},
		Resources:       controlPlaneResources(),
		SecurityContext: hardenedContainerSecurityContext(),
	}

	volumes := []corev1.Volume{
		{
			Name: configVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: configSecretName},
			},
		},
		{
			Name: worldsVolume,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: p.WorldsHostPath, Type: &hostPathDir},
			},
		},
		{
			Name: backupVolume,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: p.BackupPVC},
			},
		},
		{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}

	return &batchv1.CronJob{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{Name: SAReaper, Namespace: p.ControlNamespace, Labels: labels},
		Spec: batchv1.CronJobSpec{
			Schedule:                   reaperSchedule,
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			StartingDeadlineSeconds:    int64Ptr(reaperStartingDeadlineSeconds),
			SuccessfulJobsHistoryLimit: int32Ptr(reaperHistoryLimit),
			FailedJobsHistoryLimit:     int32Ptr(reaperHistoryLimit),
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					BackoffLimit:          int32Ptr(reaperBackoffLimit),
					ActiveDeadlineSeconds: int64Ptr(reaperActiveDeadlineSeconds),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							ServiceAccountName: SAReaper,
							RestartPolicy:      corev1.RestartPolicyNever,
							SecurityContext:    hardenedPodSecurityContext(),
							Containers:         []corev1.Container{container},
							Volumes:            volumes,
						},
					},
				},
			},
		},
	}
}

// controlPlaneDeployment assembles a single-replica control-plane Deployment. The
// Deployment, its selector, and the pod template all carry
// controlPlanePodLabels(component) so the three agree (a selector mismatch would
// leave pods unmanaged). The component is taken from the container name, which is
// the component value for both control-plane workloads.
//
// Strategy is Recreate, not RollingUpdate: neither the operator nor the api wires
// leader election, so a RollingUpdate's maxSurge overlap would briefly run two
// instances — two reconcilers fighting, or two processes binding the same ports.
// Recreate guarantees the old pod is gone before the new one starts.
func controlPlaneDeployment(p Params, sa string, container corev1.Container, volumes []corev1.Volume) *appsv1.Deployment {
	labels := controlPlanePodLabels(container.Name)
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: p.ControlNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: sa,
					SecurityContext:    hardenedPodSecurityContext(),
					Containers:         []corev1.Container{container},
					Volumes:            volumes,
				},
			},
		},
	}
}

// registryDeployment renders the in-cluster Distribution registry (spec §16).
// Build Jobs push to registry.<registry-ns>.svc:<port>, the destination the build
// egress NetworkPolicy opens — so this Deployment+Service+PVC make that policy
// target real. The registry never calls the K8s API, so its token auto-mount is
// disabled (matching the weak build/restore SA hygiene), and REGISTRY_HTTP_ADDR
// pins its listen port to the Service port instead of trusting the image default.
func registryDeployment(p Params) *appsv1.Deployment {
	p = p.withDefaults()
	labels := registryLabels()

	container := corev1.Container{
		Name:  registryName,
		Image: p.RegistryImage,
		Env: []corev1.EnvVar{
			{Name: "REGISTRY_HTTP_ADDR", Value: fmt.Sprintf(":%d", p.RegistryPort)},
		},
		Ports: []corev1.ContainerPort{
			{Name: registryName, ContainerPort: p.RegistryPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: registryVolume, MountPath: registryDataPath},
			{Name: tmpVolume, MountPath: "/tmp"},
		},
		Resources:       controlPlaneResources(),
		SecurityContext: hardenedContainerSecurityContext(),
	}

	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: registryName, Namespace: p.RegistryNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext:              hardenedPodSecurityContext(),
					Containers:                   []corev1.Container{container},
					Volumes: []corev1.Volume{
						{
							Name: registryVolume,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: registryName},
							},
						},
						{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

// registryService renders the ClusterIP Service that gives the registry its
// pinned DNS name registry.<registry-ns>.svc:<port> — hardcoded across the build
// subsystem and config. Its selector matches the registry pod labels; because
// those labels are NOT part-of=felis-control-plane, the registry is invisible to
// the RCON NetworkPolicy peer.
func registryService(p Params) *corev1.Service {
	p = p.withDefaults()
	labels := registryLabels()
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: registryName, Namespace: p.RegistryNamespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       registryName,
				Port:       p.RegistryPort,
				TargetPort: intstr.FromInt32(p.RegistryPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// registryPVC renders the registry's data volume (RWO). No storageClassName is
// set, so it binds the cluster's default class — pinning one would be a guess.
func registryPVC(p Params) *corev1.PersistentVolumeClaim {
	p = p.withDefaults()
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: registryName, Namespace: p.RegistryNamespace, Labels: registryLabels()},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(registryStorageSize)},
			},
		},
	}
}

// registryLabels are the registry's recommended labels. Note the absence of
// part-of=felis-control-plane: that is what keeps the registry out of the RCON
// NetworkPolicy peer's reach (asserted in workloads_test.go).
func registryLabels() map[string]string {
	return map[string]string{
		LabelName:      appName,
		LabelComponent: ComponentRegistry,
	}
}

// controlPlaneResources are conservative starting requests/limits. They bound
// resource exhaustion (a security-hygiene baseline) without claiming to be tuned
// for production load — that is a deployment concern.
func controlPlaneResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
}

// hardenedPodSecurityContext is the pod-level hardening shared by every workload
// here: run as a fixed non-root uid/gid with a matching fsGroup (so the registry
// can write its group-owned PVC) and the RuntimeDefault seccomp profile.
//
// SHAPE-ASSERTED, runtime-unverified: this asserts the images can run as
// nonRootUID. The felis image is built to; registry:2 (CNCF Distribution) can,
// with the data PVC fsGroup-owned. Without a cluster the actual start-up is not
// proven here.
func hardenedPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   boolPtr(true),
		RunAsUser:      int64Ptr(nonRootUID),
		RunAsGroup:     int64Ptr(nonRootUID),
		FSGroup:        int64Ptr(nonRootUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// hardenedContainerSecurityContext mirrors the build/restore Job containers: no
// privilege, no escalation, read-only root filesystem (all writes go to the
// mounted volumes — the config/data mounts and the /tmp emptyDir), drop ALL
// capabilities. readOnlyRootFilesystem is shape-asserted, not runtime-proven.
func hardenedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		Privileged:               boolPtr(false),
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func boolPtr(b bool) *bool    { return &b }
func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }
