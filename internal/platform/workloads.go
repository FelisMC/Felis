package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"time"

	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/placement"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// Credential Secrets are referenced by name and never rendered into this bundle.
	configSecretName      = "felis-config"
	configSecretKey       = "felis.toml"
	configMountPath       = "/etc/felis"
	configFilePath        = "/etc/felis/felis.toml"
	felisBinaryPath       = "/usr/local/bin/felis"
	serviceTokenSecretKey = naming.ServiceTokenSecretKey

	// The external API port must match server.listen in felis.toml.
	apiExternalPort     int32 = 8080
	apiInternalPort     int32 = 8081
	apiHTTPSPort        int32 = 8443
	operatorMetricsPort int32 = 8080
	operatorHealthPort  int32 = 8081 // must match operator --health-probe-bind-address
	apiTLSSecretName          = "felis-api-tls"
	apiTLSMountPath           = "/etc/felis/tls"

	registryName        = "registry"
	registryDataPath    = "/var/lib/registry"
	registryStorageSize = "10Gi"
	// Node containerd mirrors to the loopback hostPort because it cannot use the registry Service VIP; this hop is HTTP.
	registryLoopbackHost  = "127.0.0.1"
	registryGateName      = "registry-gate"
	registryAuthVolume    = "registry-auth"
	registryAuthMountPath = "/etc/felis-registry-auth"
	// registryMaint* stores the gate's read-only maintenance window across container restarts.
	registryGCName         = "registry-gc"
	registryMaintVolume    = "maint"
	registryMaintMountPath = "/run/felis-maint"
	registryGCInterval     = 24 * time.Hour

	configVolume   = "config"
	tmpVolume      = "tmp"
	registryVolume = "data"
	worldsVolume   = "worlds"
	backupVolume   = "backup"
	uploadsVolume  = "uploads"

	uploadsPVCName     = "felis-uploads"
	uploadsStorageSize = "5Gi"
	backupStorageSize  = "10Gi"
	UploadsLocalPath   = "/var/lib/felis/uploads"
	// The S3 Secret is optional and never rendered.
	UploadsS3SecretName      = "felis-uploads-s3"
	UploadsS3SecretAccessKey = "access_key_id"
	UploadsS3SecretSecretKey = "secret_access_key"
	UploadsS3AccessKeyEnv    = "FELIS_UPLOADS_S3_ACCESS_KEY"
	UploadsS3SecretKeyEnv    = "FELIS_UPLOADS_S3_SECRET_KEY"

	// The SMTP Secret is optional and never rendered.
	SMTPSecretName        = "felis-smtp"
	SMTPSecretPasswordKey = "password"
	SMTPPasswordEnv       = "FELIS_SMTP_PASSWORD"

	// The prune token comes from naming.RegistryAuthSecretName's optional prune key.
	RegistryPruneTokenEnv = "FELIS_REGISTRY_PRUNE_TOKEN"
	registryPruneTokenKey = "prune"

	worldsMountPath = "/worlds"

	reaperSchedule = "0 4 * * *"

	reaperStartingDeadlineSeconds int64 = 300
	reaperActiveDeadlineSeconds   int64 = 3600
	reaperBackoffLimit            int32 = 2
	reaperHistoryLimit            int32 = 3

	nonRootUID int64 = 1000 // must match internal/restore.defaultRunAsID
)

const UploadsPVCName = uploadsPVCName

const (
	ConfigSecretName = configSecretName
	ConfigSecretKey  = configSecretKey
	APITLSSecretName = apiTLSSecretName
)

const ControlPlaneUID = int(nonRootUID)

const APIInternalServiceName = SAAPI + "-internal"

const OperatorMetricsServiceName = SAOperator + "-metrics"

func scrapeAnnotations(port int32) map[string]string {
	return map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   fmt.Sprint(port),
		"prometheus.io/path":   "/metrics",
	}
}

const APIInternalPort = apiInternalPort

func InternalAPIBaseURL(controlNamespace string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", APIInternalServiceName, controlNamespace, apiInternalPort)
}

func Workloads(p Params) []Object {
	p = p.withDefaults()
	objs := []Object{
		postgresDeployment(p),
		postgresService(p),
		APIDeployment(p),
		apiService(p),
		apiInternalService(p),
		OperatorDeployment(p),
		operatorMetricsService(p),
		registryDeployment(p),
		registryService(p),
		registryPVC(p),
		uploadsPVC(p),
		artifactsDeployment(p),
		artifactsService(p),
		artifactsPVC(p),
	}
	if p.BackupPVC != "" {
		objs = append(objs, backupPVC(p))
	}
	if p.ControllerHA {
		objs = append(objs, ReplicatedStorageClass(p), postgresPVC(p))
	}
	switch {
	case p.Distributed && retentionEnabled(p):
		objs = append(objs, reaperCronJob(p))
	case reaperEnabled(p):
		objs = append(objs, worldsRootPV(p), worldsRootPVC(p), reaperCronJob(p))
	case retentionEnabled(p):
		objs = append(objs, reaperCronJob(p))
	}
	if p.Distributed {
		objs = append(objs, ArchiveDeployment(p), ArchiveService(p))
	}
	return objs
}

// The built-in PriorityClass crosses the kubelet critical-pod eviction threshold, unlike user-defined classes.
const controlPlanePriorityName = "system-cluster-critical"

func reaperEnabled(p Params) bool {
	return (p.WorldsHostPath != "" || p.Distributed) && retentionEnabled(p)
}

// retentionEnabled permits archive-only cleanup without a worlds root.
func retentionEnabled(p Params) bool {
	return p.BackupPVC != "" && p.ArchiveLocalPath != ""
}

func APIDeployment(p Params) *appsv1.Deployment {
	p = p.withDefaults()

	var env []corev1.EnvVar
	// Required token Secrets prevent replacement Pods from starting with missing service credentials.
	for _, ct := range naming.CallerTokens {
		env = append(env, corev1.EnvVar{
			Name: ct.APIEnv,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: ct.Secret},
					Key:                  serviceTokenSecretKey,
				},
			},
		})
	}
	env = append(env,
		corev1.EnvVar{Name: "FELIS_IMAGE", Value: p.FelisImage},
		corev1.EnvVar{Name: "FELIS_PANEL_NODEPORT", Value: fmt.Sprint(p.PanelNodePort)},
		corev1.EnvVar{Name: naming.EnvAPIBaseURL, Value: InternalAPIBaseURL(p.ControlNamespace)},
	)
	if p.BackupPVC != "" {
		env = append(env, corev1.EnvVar{Name: "FELIS_BACKUP_PVC", Value: claimName(p, p.BackupPVC)})
	}
	optional := boolPtr(true)
	env = append(env,
		corev1.EnvVar{Name: UploadsS3AccessKeyEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: UploadsS3SecretName}, Key: UploadsS3SecretAccessKey, Optional: optional,
		}}},
		corev1.EnvVar{Name: UploadsS3SecretKeyEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: UploadsS3SecretName}, Key: UploadsS3SecretSecretKey, Optional: optional,
		}}},
		corev1.EnvVar{Name: SMTPPasswordEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: SMTPSecretName}, Key: SMTPSecretPasswordKey, Optional: optional,
		}}},
		corev1.EnvVar{Name: RegistryPruneTokenEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: naming.RegistryAuthSecretName}, Key: registryPruneTokenKey, Optional: optional,
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
			{Name: uploadsVolume, MountPath: UploadsLocalPath},
		},
		// Use healthz for liveness so database outages do not restart API Pods.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/readyz", Port: intstr.FromInt32(apiInternalPort),
			}},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
			TimeoutSeconds:      3,
			FailureThreshold:    3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", Port: intstr.FromInt32(apiInternalPort),
			}},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
			TimeoutSeconds:      3,
			FailureThreshold:    3,
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
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName(p, uploadsPVCName)},
			},
		},
	}

	if p.NodeControlSocket != "" {
		directory := filepath.Dir(p.NodeControlSocket)
		hostType := corev1.HostPathDirectory
		volumes = append(volumes, corev1.Volume{Name: "node-control", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: directory, Type: &hostType}}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "node-control", MountPath: directory, ReadOnly: true})
		container.Env = append(container.Env, corev1.EnvVar{Name: "FELIS_NODE_CONTROL_SOCKET", Value: p.NodeControlSocket})
	}
	deployment := controlPlaneDeployment(p, SAAPI, container, volumes)
	if p.NodeControlSocket != "" {
		deployment.Spec.Template.Spec.SecurityContext.SupplementalGroups = []int64{65532}
	}
	if p.NodeControlNode != "" && p.ControllerNode == "" {
		deployment.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": p.NodeControlNode}
	}
	return deployment
}

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

// Keep the internal face on a separate ClusterIP Service; adding it to the NodePort would expose it on every node.
func apiInternalService(p Params) *corev1.Service {
	p = p.withDefaults()
	labels := controlPlanePodLabels(ComponentAPI)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: APIInternalServiceName, Namespace: p.ControlNamespace, Labels: labels,
			Annotations: scrapeAnnotations(apiInternalPort)},
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

func operatorMetricsService(p Params) *corev1.Service {
	p = p.withDefaults()
	labels := controlPlanePodLabels(ComponentOperator)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: OperatorMetricsServiceName, Namespace: p.ControlNamespace, Labels: labels,
			Annotations: scrapeAnnotations(operatorMetricsPort)},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "metrics",
				Port:       operatorMetricsPort,
				TargetPort: intstr.FromString("metrics"),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

func OperatorDeployment(p Params) *appsv1.Deployment {
	p = p.withDefaults()

	container := corev1.Container{
		Name:    ComponentOperator,
		Image:   p.FelisImage,
		Command: []string{felisBinaryPath, "operator"},
		Args: []string{
			"--namespace", p.MinecraftNamespace,
			"--metrics-bind-address", fmt.Sprintf(":%d", operatorMetricsPort),
			"--health-probe-bind-address", fmt.Sprintf(":%d", operatorHealthPort),
		},
		Env: []corev1.EnvVar{
			{Name: "FELIS_IMAGE", Value: p.FelisImage},
			{Name: "FELIS_ARTIFACT_CACHE", Value: ArtifactCacheURL(p.ControlNamespace)},
			{Name: "FELIS_CONTROL_NAMESPACE", Value: p.ControlNamespace},
		},
		Ports: []corev1.ContainerPort{
			{Name: "metrics", ContainerPort: operatorMetricsPort, Protocol: corev1.ProtocolTCP},
			{Name: "health", ContainerPort: operatorHealthPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: tmpVolume, MountPath: "/tmp"},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/readyz", Port: intstr.FromInt32(operatorHealthPort),
			}},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
			TimeoutSeconds:      3,
			FailureThreshold:    3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", Port: intstr.FromInt32(operatorHealthPort),
			}},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
			TimeoutSeconds:      3,
			FailureThreshold:    3,
		},
		Resources:       controlPlaneResources(),
		SecurityContext: hardenedContainerSecurityContext(),
	}

	volumes := []corev1.Volume{
		{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}

	return controlPlaneDeployment(p, SAOperator, container, volumes)
}

// Without WorldsHostPath the reaper runs retention-only; its archive mount must match felis.toml archive.local_path.
func reaperCronJob(p Params) *batchv1.CronJob {
	p = p.withDefaults()
	labels := controlPlanePodLabels(ComponentReaper)
	container := corev1.Container{
		Name:    ComponentReaper,
		Image:   p.FelisImage,
		Command: []string{felisBinaryPath, "reaper"},
		Args:    []string{"--config", configFilePath, "--retention-only"},
		VolumeMounts: []corev1.VolumeMount{
			{Name: configVolume, MountPath: configMountPath, ReadOnly: true},
			{Name: backupVolume, MountPath: p.ArchiveLocalPath},
			{Name: tmpVolume, MountPath: "/tmp"},
		},
		Resources:       controlPlaneResources(),
		SecurityContext: reaperContainerSecurityContext(),
	}

	volumes := []corev1.Volume{
		{
			Name: configVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: configSecretName},
			},
		},
		{
			Name: backupVolume,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName(p, p.BackupPVC)},
			},
		},
		{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}

	if p.WorldsHostPath != "" {
		container.Args = []string{"--config", configFilePath, "--worlds-root", worldsMountPath}
		// secretKeyRef resolves the SMTP mirror in the Minecraft namespace.
		container.Env = []corev1.EnvVar{
			{Name: SMTPPasswordEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: SMTPSecretName},
				Key:                  SMTPSecretPasswordKey,
				Optional:             boolPtr(true),
			}}},
		}
		container.VolumeMounts = append(container.VolumeMounts,
			corev1.VolumeMount{Name: worldsVolume, MountPath: worldsMountPath, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: worldsVolume,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: worldsRootName(p), ReadOnly: true,
				},
			},
		})
	}

	if p.Distributed {
		container.Args = []string{"--config", configFilePath}
		container.Env = append(container.Env, distributedEnv(p)...)
		container.Env = append(container.Env, corev1.EnvVar{Name: "FELIS_IMAGE", Value: p.FelisImage})
	}
	return &batchv1.CronJob{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
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
						Spec:       reaperPodSpec(p, container, volumes),
					},
				},
			},
		},
	}
}

// worldsRootName hashes immutable hostPath and node affinity so changing either creates a new PV and PVC pair.
func worldsRootName(p Params) string {
	sum := sha256.Sum256([]byte(p.MinecraftNamespace + "\x00" + p.WorldsHostPath + "\x00" + p.ReaperNode))
	return "felis-worlds-root-" + hex.EncodeToString(sum[:4])
}

// worldsRootStorage is nominal; hostPath enforces no capacity limit.
const worldsRootStorage = "1Gi"

// Use a prebound retained static PV because baseline forbids inline hostPath; empty storageClassName prevents dynamic provisioning.
func worldsRootPV(p Params) *corev1.PersistentVolume {
	p = p.withDefaults()
	name := worldsRootName(p)
	hostPathDir := corev1.HostPathDirectory
	pv := &corev1.PersistentVolume{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolume"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: controlPlanePodLabels(ComponentReaper)},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(worldsRootStorage)},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              "",
			ClaimRef:                      &corev1.ObjectReference{Namespace: p.MinecraftNamespace, Name: name},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: p.WorldsHostPath, Type: &hostPathDir},
			},
		},
	}
	if p.ReaperNode != "" {
		pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{p.ReaperNode},
			}}}},
		}}
	}
	return pv
}

func worldsRootPVC(p Params) *corev1.PersistentVolumeClaim {
	p = p.withDefaults()
	name := worldsRootName(p)
	noClass := ""
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.MinecraftNamespace, Labels: controlPlanePodLabels(ComponentReaper)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &noClass,
			VolumeName:       name,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(worldsRootStorage)},
			},
		},
	}
}

// Use SAReaper only with reaping RBAC; spell out default otherwise because the API may refill an empty account from its deprecated alias.
func reaperPodSpec(p Params, container corev1.Container, volumes []corev1.Volume) corev1.PodSpec {
	spec := corev1.PodSpec{
		PriorityClassName: controlPlanePriorityName,
		RestartPolicy:     corev1.RestartPolicyNever,
		SecurityContext:   reaperPodSecurityContext(),
		Containers:        []corev1.Container{container},
		Volumes:           volumes,
	}
	if p.ControllerNode != "" {
		spec.NodeSelector = controllerSelector(p)
	} else if p.ReaperNode != "" {
		spec.NodeSelector = map[string]string{"kubernetes.io/hostname": p.ReaperNode}
	}
	haPlacement(p, &spec)
	if p.WorldsHostPath != "" || p.Distributed {
		spec.ServiceAccountName = SAReaper
	} else {
		spec.ServiceAccountName = "default"
		spec.AutomountServiceAccountToken = boolPtr(false)
	}
	return spec
}

func controlPlaneDeployment(p Params, sa string, container corev1.Container, volumes []corev1.Volume) *appsv1.Deployment {
	labels := controlPlanePodLabels(container.Name)
	if p.ControllerHA {
		container.Env = append(container.Env, corev1.EnvVar{Name: "FELIS_CONTROLLER_HA", Value: "true"})
	}
	if p.Distributed {
		if sa == SAOperator {
			container.Env = append(container.Env, corev1.EnvVar{Name: "FELIS_DISTRIBUTED", Value: "true"}, corev1.EnvVar{Name: "FELIS_CONTROLLER_NODE", Value: p.ControllerNode}, corev1.EnvVar{Name: "FELIS_EGRESS_PROBE", Value: p.EgressProbe})
		} else {
			container.Env = append(container.Env, distributedEnv(p)...)
		}
	}
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: p.ControlNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector:       controllerSelector(p),
					ServiceAccountName: sa,
					PriorityClassName:  controlPlanePriorityName,
					SecurityContext:    hardenedPodSecurityContext(),
					Containers:         []corev1.Container{container},
					Volumes:            volumes,
				},
			},
		},
	}
	haPlacement(p, &deployment.Spec.Template.Spec)
	return deployment
}

// The registry gate exposes a loopback hostPort for node pulls; pin its Felis image alongside the registry image.
func registryDeployment(p Params) *appsv1.Deployment {
	p = p.withDefaults()
	labels := registryLabels()
	upstreamPort := registryUpstreamPort(p)

	registry := corev1.Container{
		Name:  registryName,
		Image: p.RegistryImage,
		Env: []corev1.EnvVar{
			{Name: "REGISTRY_HTTP_ADDR", Value: fmt.Sprintf("127.0.0.1:%d", upstreamPort)},
			{Name: "REGISTRY_STORAGE_DELETE_ENABLED", Value: "true"},
			// Disable the blob descriptor cache because garbage collection can remove blobs whose cached descriptors would suppress reupload.
			{Name: "REGISTRY_STORAGE_CACHE_BLOBDESCRIPTOR", Value: "none"},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: registryVolume, MountPath: registryDataPath},
			{Name: tmpVolume, MountPath: "/tmp"},
		},
		Resources:       registryResources(),
		SecurityContext: hardenedContainerSecurityContext(),
	}

	gate := corev1.Container{
		Name:    registryGateName,
		Image:   p.FelisImage,
		Command: []string{felisBinaryPath, "registry-gate"},
		Args: []string{
			fmt.Sprintf("--listen=:%d", p.RegistryPort),
			fmt.Sprintf("--upstream=http://127.0.0.1:%d", upstreamPort),
			"--auth-dir=" + registryAuthMountPath,
			fmt.Sprintf("--maint-listen=127.0.0.1:%d", registryMaintPort(p)),
			"--maint-dir=" + registryMaintMountPath,
			"--data-dir=" + registryDataPath,
		},
		Ports: []corev1.ContainerPort{
			{
				Name: registryName, ContainerPort: p.RegistryPort, Protocol: corev1.ProtocolTCP,
				HostPort: p.RegistryPort, HostIP: registryLoopbackHost,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: registryAuthVolume, MountPath: registryAuthMountPath, ReadOnly: true},
			{Name: registryMaintVolume, MountPath: registryMaintMountPath},
			{Name: registryVolume, MountPath: registryDataPath, ReadOnly: true},
		},
		// Readiness requires a healthy registry upstream; liveness tests the gate alone.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", Port: intstr.FromString(registryName),
			}},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/livez", Port: intstr.FromString(registryName),
			}},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
			TimeoutSeconds:      3,
			FailureThreshold:    3,
		},
		Resources:       registryGateResources(),
		SecurityContext: hardenedContainerSecurityContext(),
	}

	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: registryName, Namespace: p.RegistryNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector:                 controllerSelector(p),
					AutomountServiceAccountToken: boolPtr(false),
					PriorityClassName:            controlPlanePriorityName,
					SecurityContext:              hardenedPodSecurityContext(),
					Containers:                   []corev1.Container{registry, gate, registryGCContainer(p)},
					Volumes: []corev1.Volume{
						{
							Name: registryVolume,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName(p, registryName)},
							},
						},
						{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: registryMaintVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{
							Name: registryAuthVolume,
							VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
								SecretName:  naming.RegistryAuthSecretName,
								Optional:    boolPtr(true), // missing credentials deny writes but permit pulls
								DefaultMode: int32Ptr(0o440),
							}},
						},
					},
				},
			},
		},
	}
	haPlacement(p, &deployment.Spec.Template.Spec)
	return deployment
}

func registryUpstreamPort(p Params) int32 { return p.RegistryPort + 1 }

func registryMaintPort(p Params) int32 { return p.RegistryPort + 2 }

// Registry GC uses a leased read-only window and preserves untagged manifests because servers pin images by digest.
const registryGCScript = `set -u
# sh as PID 1 ignores SIGTERM unless it traps it, and runs the trap only once the
# foreground child exits: sleep in the background and wait, so a pod delete does
# not sit out the grace period.
trap 'exit 0' TERM
nap() { sleep "$1" & wait $!; }
maint="http://127.0.0.1:${FELIS_GC_MAINT_PORT}"
stamp=/var/lib/registry/.felis-last-gc
while :; do
  now=$(date +%s)
  last=$(cat "$stamp" 2>/dev/null || echo 0)
  case "$last" in ''|*[!0-9]*) last=0 ;; esac
  if [ $((now - last)) -ge "$FELIS_GC_INTERVAL_SECONDS" ]; then
    tries=0
    until wget -q -O /dev/null --post-data '' "$maint/readonly?lease=3600"; do
      tries=$((tries + 1))
      [ "$tries" -ge 180 ] && break
      nap 10
    done
    if [ "$tries" -lt 180 ]; then
      echo "felis-gc: registry is read-only; collecting"
      if registry garbage-collect /etc/docker/registry/config.yml > /tmp/gc.log 2>&1; then
        date +%s > "$stamp"
        echo "felis-gc: done ($(grep -c 'blob eligible for deletion' /tmp/gc.log) blob(s) deleted)"
      else
        echo "felis-gc: garbage-collect failed:" >&2
        tail -n 20 /tmp/gc.log >&2
      fi
      wget -q -O /dev/null --post-data '' "$maint/readwrite" \
        || echo "felis-gc: could not hand the read-only window back; it lapses with its lease" >&2
    else
      echo "felis-gc: pushes never went quiet for 30 minutes; trying again later" >&2
    fi
  fi
  nap 600
done
`

func registryGCContainer(p Params) corev1.Container {
	return corev1.Container{
		Name:    registryGCName,
		Image:   p.RegistryImage,
		Command: []string{"/bin/sh", "-c", registryGCScript},
		Env: []corev1.EnvVar{
			{Name: "FELIS_GC_MAINT_PORT", Value: fmt.Sprint(registryMaintPort(p))},
			{Name: "FELIS_GC_INTERVAL_SECONDS", Value: fmt.Sprint(int64(registryGCInterval / time.Second))},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: registryVolume, MountPath: registryDataPath},
			{Name: tmpVolume, MountPath: "/tmp"},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
		SecurityContext: hardenedContainerSecurityContext(),
	}
}

func registryGateResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("20m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
}

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

func registryPVC(p Params) *corev1.PersistentVolumeClaim {
	p = p.withDefaults()
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: claimName(p, registryName), Namespace: p.RegistryNamespace, Labels: registryLabels()},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: storageClassFor(p),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(p.RegistryStorage)},
			},
		},
	}
}

func uploadsPVC(p Params) *corev1.PersistentVolumeClaim {
	p = p.withDefaults()
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: claimName(p, uploadsPVCName), Namespace: p.ControlNamespace, Labels: controlPlanePodLabels(ComponentAPI)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: storageClassFor(p),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(p.UploadsStorage)},
			},
		},
	}
}

func backupPVC(p Params) *corev1.PersistentVolumeClaim {
	p = p.withDefaults()
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: claimName(p, p.BackupPVC), Namespace: p.MinecraftNamespace, Labels: controlPlanePodLabels(ComponentAPI)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: storageClassFor(p),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(p.BackupStorage)},
			},
		},
	}
}

func VolumeBinderPod(ns, pvc, image string) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "felis-bind-" + pvc + "-",
			Namespace:    ns,
			Labels:       map[string]string{LabelName: appName, LabelComponent: "volume-binder"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(false),
			SecurityContext:              hardenedPodSecurityContext(),
			Containers: []corev1.Container{{
				Name:            "bind",
				Image:           image,
				Command:         []string{felisBinaryPath, "version"},
				SecurityContext: hardenedContainerSecurityContext(),
				VolumeMounts:    []corev1.VolumeMount{{Name: "archives", MountPath: "/archives"}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
			}},
			Volumes: []corev1.Volume{{
				Name:         "archives",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc}},
			}},
		},
	}
}

func registryLabels() map[string]string {
	return map[string]string{
		LabelName:      appName,
		LabelComponent: ComponentRegistry,
	}
}

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

// The registry needs headroom for large layer pushes; a 256Mi limit was insufficient for a 475 MB layer.
func registryResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
}

func hardenedPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   boolPtr(true),
		RunAsUser:      int64Ptr(nonRootUID),
		RunAsGroup:     int64Ptr(nonRootUID),
		FSGroup:        int64Ptr(nonRootUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// The reaper needs root to read world files owned by game users or previous root-run processes.
func reaperPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   boolPtr(false),
		RunAsUser:      int64Ptr(0),
		RunAsGroup:     int64Ptr(0),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// DAC_OVERRIDE permits root to read private game-owned world files.
func reaperContainerSecurityContext() *corev1.SecurityContext {
	sc := hardenedContainerSecurityContext()
	sc.Capabilities = &corev1.Capabilities{
		Drop: []corev1.Capability{"ALL"},
		Add:  []corev1.Capability{"DAC_OVERRIDE"},
	}
	return sc
}

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

func controllerSelector(p Params) map[string]string {
	if p.ControllerHA {
		return controllerRoleSelector()
	}
	if p.ControllerNode == "" {
		return nil
	}
	return map[string]string{placement.LabelIdentity: p.ControllerNode, placement.LabelRole: placement.RoleController}
}
