package platform

import (
	"fmt"

	"felis.lolicon.best/internal/naming"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
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
// the fail-safe choice for a workload that deletes PVCs. The reaper resolver
// (cmd/felis/reaper.resolveWorldDir) finds worlds either as <WorldsHostPath>/<pvc>
// or in the stock local-path layout k3s writes under its storage root
// (<pv-name>_<ns>_<pvc-name>, read from the live PVC), so pointing
// --worlds-host-path at /var/lib/rancher/k3s/storage works on a default install —
// see the WorldsHostPath field doc. Whether the tar finds a world still depends on
// the hosting node, and is not provable without a cluster. No nodeSelector is set:
// the single-node starter pins
// the worlds to one node implicitly; a multi-node deployment MUST add one (or the
// CronJob could schedule on a node where the hostPath is empty) — a hazard left on
// record here until multi-node retention is built.
const (
	// configSecretName / serviceTokenSecretName are referenced BY NAME and NEVER
	// rendered into the bundle: felis.toml carries the database URL (a credential)
	// and the service token is a credential, so writing either into a checked-in
	// manifest is a hard red line. The deployment provisions both Secrets
	// out-of-band before applying these workloads.
	configSecretName = "felis-config"
	configSecretKey  = "felis.toml"
	configMountPath  = "/etc/felis"
	configFilePath   = "/etc/felis/felis.toml"
	felisBinaryPath  = "/usr/local/bin/felis"
	// Single-sourced with the operator, which injects the same Secret into the
	// login system server's pod (see internal/naming).
	serviceTokenSecretName = naming.ServiceTokenSecretName
	serviceTokenSecretKey  = naming.ServiceTokenSecretKey

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
	uploadsVolume  = "uploads"

	// uploads* wire the user-uploads build-context store into felis-api. The PVC
	// backs a LOCAL user_uploads_context — durable across pod restarts and
	// fsGroup-writable by the non-root pod (unlike a root-owned hostPath). It is
	// always rendered but only written to when uploads are local. The S3 secret/env
	// carry credentials for an s3:// user_uploads_context and are OPTIONAL, so a
	// local-storage install (no such Secret) still starts.
	uploadsPVCName     = "felis-uploads"
	uploadsStorageSize = "5Gi"
	// backupStorageSize is the world-archive store's default capacity. It is a
	// starter-sized floor (registry 10Gi, uploads 5Gi sit beside it): archives are
	// compressed worlds and a fresh one is ~200MB, so this holds many while leaving
	// the growth knob (resize the PVC / move to a snapshot backend, spec §19) to the
	// operator. There is no storageClassName: the cluster default is the only safe
	// binding, exactly like registryPVC.
	backupStorageSize = "10Gi"
	// UploadsLocalPath is the in-pod mount of the uploads PVC; a local
	// user_uploads_context points here so the derived context ref and the on-disk
	// write location agree. Exported so the setup wizard stamps it into felis.toml.
	UploadsLocalPath = "/var/lib/felis/uploads"
	// UploadsS3SecretName is the out-of-band Secret carrying the S3 credentials for
	// an s3:// user_uploads_context. felis-api mounts its keys into env (optionally)
	// and the setup wizard creates it. Like configSecretName it is NEVER rendered
	// into the bundle — the credentials are the same red line.
	UploadsS3SecretName      = "felis-uploads-s3"
	UploadsS3SecretAccessKey = "access_key_id"
	UploadsS3SecretSecretKey = "secret_access_key"
	// UploadsS3AccessKeyEnv / UploadsS3SecretKeyEnv are the env vars felis-api reads
	// the S3 credentials from; registry.s3.access_key_ref / secret_key_ref default to
	// these names. The deployment injects them from UploadsS3SecretName (optional).
	UploadsS3AccessKeyEnv = "FELIS_UPLOADS_S3_ACCESS_KEY"
	UploadsS3SecretKeyEnv = "FELIS_UPLOADS_S3_SECRET_KEY"

	// SMTPSecretName is the out-of-band Secret carrying the [smtp] relay password.
	// Same red line as the S3 credentials: the setup wizard's "configure email"
	// step creates it, felis-api reads it via SMTPPasswordEnv (optionally — a
	// mailer-less install has no such Secret and still starts, falling back to
	// logging codes), and it is never rendered into the bundle.
	SMTPSecretName        = "felis-smtp"
	SMTPSecretPasswordKey = "password"
	// SMTPPasswordEnv is the env var felis-api reads the relay password from;
	// [smtp] password_ref defaults to this name.
	SMTPPasswordEnv = "FELIS_SMTP_PASSWORD"

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

// APIInternalServiceName is the ClusterIP Service that fronts the felis-api
// internal face (8081). It is SEPARATE from the external NodePort Service (SAAPI)
// on purpose — see apiInternalService. The login pod resolves it by cross-namespace
// DNS; the on-node break-glass console resolves its ClusterIP and dials it directly.
const APIInternalServiceName = SAAPI + "-internal"

// APIInternalPort is the felis-api internal-face port, exported for the on-node
// console which builds http://<clusterIP>:APIInternalPort after a Service lookup.
const APIInternalPort = apiInternalPort

// InternalAPIBaseURL returns the in-cluster base URL of the felis-api INTERNAL
// face for a caller in another namespace — specifically the login system server,
// which dials it with the service token to mint bind codes and poll link status.
// It single-sources the internal Service name (APIInternalServiceName, in the
// control namespace) and the internal port with the Deployment/Service above, so a
// rename or port change here can never drift from what the login pod is told to
// call. Cross-namespace DNS is always resolvable, and apiInternalService actually
// programs 8081 on that ClusterIP; reachability additionally depends on there being
// no fence in the way (today neither the minecraft-ns egress nor the control-ns
// ingress is policy-locked, so the path is open — see internal/platform/netpol.go).
func InternalAPIBaseURL(controlNamespace string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", APIInternalServiceName, controlNamespace, apiInternalPort)
}

// Workloads renders the running control-plane: the felis-api Deployment, the
// felis-operator Deployment, and the in-cluster registry (Deployment + Service +
// PVC), the world-archive PVC when p.BackupPVC names it (it backs the
// backup/restore Jobs and the reaper), plus the reaper CronJob when
// reaperEnabled(p). Every pod template carries the felis-control-plane
// PriorityClass (controlPlanePriorityClass below), the node-pressure eviction
// shield. FelisImage is required — `felis manifests` enforces it (fail-loud), so
// a rendered bundle always names a concrete image.
func Workloads(p Params) []Object {
	p = p.withDefaults()
	objs := []Object{
		controlPlanePriorityClass(),
		APIDeployment(p),
		apiService(p),
		apiInternalService(p),
		OperatorDeployment(p),
		registryDeployment(p),
		registryService(p),
		registryPVC(p),
		uploadsPVC(p),
	}
	if p.BackupPVC != "" {
		objs = append(objs, backupPVC(p))
	}
	if reaperEnabled(p) {
		objs = append(objs, reaperCronJob(p))
	}
	return objs
}

// controlPlanePriorityName is the PriorityClass every control-plane workload runs
// under (api, operator, reaper, registry). Node-pressure eviction (a full disk
// being the realistic case on a game box) removes pods in ASCENDING priority, and
// a classless pod is priority 0 — the same as the game servers, which are exactly
// the pods a burst of joins has just filled the node with. A full disk then takes
// the api/operator down too, and recovery needs a human: with no reachable
// registry on an air-gapped box the images are gone, so the fix is a re-run of the
// installer to re-import them. The class is an eviction shield, not a preemption
// lever: PreemptionPolicy=Never, so a busy node never loses a running game server
// merely to schedule the api. Value 1,000,000 sits above every game pod (0) and
// far below the kubelet's system-reserved classes (2,000,000,000).
const (
	controlPlanePriorityName         = "felis-control-plane"
	controlPlanePriorityValue  int32 = 1_000_000
	controlPlanePriorityReason       = "Felis control plane (api/operator/reaper/registry) survives node-pressure eviction before game servers"
)

// controlPlanePriorityClass renders the cluster-scoped PriorityClass the
// control-plane pod templates reference (the ONE cluster-scoped object in the
// bundle; a PriorityClass is not namespaced by design).
func controlPlanePriorityClass() *schedulingv1.PriorityClass {
	never := corev1.PreemptNever
	return &schedulingv1.PriorityClass{
		TypeMeta:         metav1.TypeMeta{APIVersion: "scheduling.k8s.io/v1", Kind: "PriorityClass"},
		ObjectMeta:       metav1.ObjectMeta{Name: controlPlanePriorityName},
		Value:            controlPlanePriorityValue,
		PreemptionPolicy: &never,
		Description:      controlPlanePriorityReason,
	}
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
//
// The uploads PVC is mounted read-write at UploadsLocalPath for a local
// user_uploads_context, and the two optional S3 credential env vars
// (UploadsS3*Env, from the felis-uploads-s3 Secret) feed an s3:// one — the two
// storage backends the setup wizard chooses between.
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
	// S3 credentials for an s3:// user_uploads_context, sourced from the
	// felis-uploads-s3 Secret. Optional: a local-storage install has no such Secret,
	// and marking these optional lets the pod start anyway (the store falls back to
	// the uploads PVC). The setup wizard creates the Secret and rolls the API when
	// the operator picks S3 storage.
	optional := boolPtr(true)
	env = append(env,
		corev1.EnvVar{Name: UploadsS3AccessKeyEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: UploadsS3SecretName}, Key: UploadsS3SecretAccessKey, Optional: optional,
		}}},
		corev1.EnvVar{Name: UploadsS3SecretKeyEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: UploadsS3SecretName}, Key: UploadsS3SecretSecretKey, Optional: optional,
		}}},
		// The [smtp] relay password, same optional-Secret pattern: absent until the
		// setup wizard's "configure email" step creates felis-smtp.
		corev1.EnvVar{Name: SMTPPasswordEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: SMTPSecretName}, Key: SMTPSecretPasswordKey, Optional: optional,
		}}},
	)

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
			// Read-WRITE: local uploads land here (an s3:// store bypasses it). The
			// hardened container root is read-only, so this PVC mount is where a local
			// LocalContextStore can persist a submitted context.
			{Name: uploadsVolume, MountPath: UploadsLocalPath},
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
		{
			Name: uploadsVolume,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: uploadsPVCName},
			},
		},
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

// apiInternalService fronts the felis-api INTERNAL face (service-token, no Zero
// Trust) on a ClusterIP-only Service, kept SEPARATE from the external NodePort
// apiService on purpose: a NodePort Service allocates a node port for EVERY declared
// port with no per-port opt-out, so folding 8081 into apiService would publish the
// no-Zero-Trust internal face on every node's external IP — a hard red line for a
// face whose only guard is the bearer service token. A distinct ClusterIP Service
// exposes 8081 in-cluster only: reachable by the login pod (cross-namespace DNS to
// APIInternalServiceName) and, on the k3s node, by the break-glass console dialing
// this Service's ClusterIP. Without it the felis-api DNS name has no 8081 port and
// every internal-face call silently fails to connect.
func apiInternalService(p Params) *corev1.Service {
	p = p.withDefaults()
	labels := controlPlanePodLabels(ComponentAPI)
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: APIInternalServiceName, Namespace: p.ControlNamespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "internal",
				Port:       apiInternalPort,
				TargetPort: intstr.FromString("internal"),
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
		// FELIS_IMAGE names this same image so the operator can run it as the
		// forwarding-config initContainer it injects into user servers (it must
		// name an image to run, and its own is the one image guaranteed present).
		Env: []corev1.EnvVar{
			{Name: "FELIS_IMAGE", Value: p.FelisImage},
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
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		// The CronJob lives in the MINECRAFT namespace: a Pod can only mount PVCs
		// from its own namespace and the backup PVC is provisioned there alongside
		// the backup Jobs. Placed under ControlNamespace it could never schedule
		// (FailedScheduling: persistentvolumeclaim not found) in any stock install;
		// the reaper Role/RoleBinding were already minecraft-scoped for the same
		// reason, and the minecraft felis-config replica (felis setup) supplies the
		// config mount.
		ObjectMeta: metav1.ObjectMeta{Name: SAReaper, Namespace: p.MinecraftNamespace, Labels: labels},
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
							PriorityClassName:  controlPlanePriorityName,
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
					PriorityClassName:  controlPlanePriorityName,
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
					PriorityClassName:            controlPlanePriorityName,
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

// uploadsPVC renders the felis-api user-uploads PVC (spec §16 build-context input
// domain). It backs a LOCAL user_uploads_context: felis-api mounts it read-write
// at UploadsLocalPath and LocalContextStore writes each submission's context there.
// It is always rendered (an s3:// install simply never writes to it) and carries
// control-plane labels so it reads as part of felis-api's storage. ReadWriteOnce is
// the fail-safe access mode: the single-node starter binds it to felis-api's node,
// and a future Kaniko-read integration mounts the same PVC on that node.
func uploadsPVC(p Params) *corev1.PersistentVolumeClaim {
	p = p.withDefaults()
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: uploadsPVCName, Namespace: p.ControlNamespace, Labels: controlPlanePodLabels(ComponentAPI)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(uploadsStorageSize)},
			},
		},
	}
}

// backupPVC renders the world-archive store (spec §18/§19): the PVC the backup
// and restore Jobs mount read-write, and the one the reaper CronJob writes
// archives into. It renders ONLY when p.BackupPVC names it, so the same value
// gates the PVC and the FELIS_BACKUP_PVC env on the felis-api Deployment — with
// no name there is no PVC, no env, and the backup/restore endpoints keep
// answering an honest 503 rather than enqueuing a Job that cannot mount its
// backup. It lives in the Minecraft namespace because every pod that mounts it
// runs there (a Pod can only mount PVCs from its own namespace; the reaper
// CronJob is rendered there for the same reason). No storageClassName: binding
// the cluster default is the only safe default.
func backupPVC(p Params) *corev1.PersistentVolumeClaim {
	p = p.withDefaults()
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: p.BackupPVC, Namespace: p.MinecraftNamespace, Labels: controlPlanePodLabels(ComponentAPI)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(backupStorageSize)},
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
