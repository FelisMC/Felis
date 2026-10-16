package platform

import (
	"fmt"

	"felis.lolicon.best/internal/build"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The server-software download cache (internal/artifactcache): one Deployment,
// its Service and its volume in the control namespace.
const (
	// ComponentArtifacts labels the cache. Like the registry it is a supporting
	// workload and not part-of=felis-control-plane, so no control-plane peer
	// selector can select it.
	ComponentArtifacts = "artifacts"
	ArtifactsName      = "felis-artifacts"
	ArtifactsPort      = int32(8090)

	artifactsStorageSize = "10Gi"
	artifactsDataPath    = "/cache"
)

// ArtifactCacheURL is the cache's in-cluster base URL, which the operator hands
// runtime-image servers' install steps.
func ArtifactCacheURL(controlNamespace string) string {
	if controlNamespace == "" {
		controlNamespace = DefaultControlNamespace
	}
	return fmt.Sprintf("http://%s.%s.svc:%d", ArtifactsName, controlNamespace, ArtifactsPort)
}

func artifactsLabels() map[string]string {
	return map[string]string{LabelName: appName, LabelComponent: ComponentArtifacts}
}

// artifactsMaxBytes is the cache's own cap, 90% of the volume's request: on
// k3s local-path the request is only a label, so the cache evicts by itself to
// stay inside it.
func artifactsMaxBytes(p Params) int64 {
	q := resource.MustParse(p.ArtifactsStorage)
	return q.Value() / 10 * 9
}

// artifactsDeployment runs `felis artifact-cache`. It calls no Kubernetes API
// (no service account token), runs as the control-plane's non-root uid with a
// read-only root filesystem and no capability, and writes only its volume.
// Losing it costs nothing but bandwidth: an install that cannot reach it
// downloads from upstream itself, so its volume is not replicated. It runs
// under the control plane's priority class all the same: its image is the felis
// image the installer pins, it asks for almost nothing, and evicting it under
// disk pressure would free none of the disk its volume holds.
func artifactsDeployment(p Params) *appsv1.Deployment {
	p = p.withDefaults()
	labels := artifactsLabels()
	probe := func(initial int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(ArtifactsPort)}},
			InitialDelaySeconds: initial,
			PeriodSeconds:       10,
			TimeoutSeconds:      3,
			FailureThreshold:    3,
		}
	}
	container := corev1.Container{
		Name:    ComponentArtifacts,
		Image:   p.FelisImage,
		Command: []string{felisBinaryPath, "artifact-cache"},
		Args: []string{
			"--listen", fmt.Sprintf(":%d", ArtifactsPort),
			"--dir", artifactsDataPath,
			"--max-bytes", fmt.Sprintf("%d", artifactsMaxBytes(p)),
		},
		Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: ArtifactsPort, Protocol: corev1.ProtocolTCP}},
		VolumeMounts:   []corev1.VolumeMount{{Name: "cache", MountPath: artifactsDataPath}},
		ReadinessProbe: probe(2),
		LivenessProbe:  probe(10),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("20m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		SecurityContext: hardenedContainerSecurityContext(),
	}
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: ArtifactsName, Namespace: p.ControlNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			// One writer of the volume at a time.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector:                 controllerSelector(p),
					PriorityClassName:            controlPlanePriorityName,
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext:              hardenedPodSecurityContext(),
					Containers:                   []corev1.Container{container},
					Volumes: []corev1.Volume{{
						Name:         "cache",
						VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: ArtifactsName}},
					}},
				},
			},
		},
	}
}

func artifactsService(p Params) *corev1.Service {
	p = p.withDefaults()
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: ArtifactsName, Namespace: p.ControlNamespace, Labels: artifactsLabels()},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: artifactsLabels(),
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       ArtifactsPort,
				TargetPort: intstr.FromInt32(ArtifactsPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// artifactsPVC is the cache's volume, on the cluster default class even with
// controller HA: everything on it can be downloaded again.
func artifactsPVC(p Params) *corev1.PersistentVolumeClaim {
	p = p.withDefaults()
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: ArtifactsName, Namespace: p.ControlNamespace, Labels: artifactsLabels()},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(p.ArtifactsStorage)},
			},
		},
	}
}

// ArtifactsNetworkPolicies fence the cache both ways. Only game server pods
// reach its port: they are what it serves. Out of the pod it may resolve names
// and reach the public internet, the official download hosts, and nothing
// private: a server pod hands it the URL to fetch, and although the cache only
// accepts its allowlisted hosts, a URL must never be a way into the cluster or
// the node's network.
func ArtifactsNetworkPolicies(p Params) []*networkingv1.NetworkPolicy {
	p = p.withDefaults()
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	port := intstr.FromInt32(ArtifactsPort)
	dns := intstr.FromInt32(53)
	sel := metav1.LabelSelector{MatchLabels: artifactsLabels()}
	serverPods := serverPodSelector()
	ingress := netpol("felis-artifacts-ingress", p.ControlNamespace, sel, []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": p.MinecraftNamespace}},
			PodSelector:       &serverPods,
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
	}})
	egress := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "felis-artifacts-egress", Namespace: p.ControlNamespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: sel,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To:    []networkingv1.NetworkPolicyPeer{build.ClusterDNSPeer()},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
				},
				{To: []networkingv1.NetworkPolicyPeer{
					{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: append([]string{}, serverEgressExceptV4...)}},
					{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: append([]string{}, serverEgressExceptV6...)}},
				}},
			},
		},
	}
	return []*networkingv1.NetworkPolicy{ingress, egress}
}

// serverToArtifacts is the server egress rule to the cache: the cache's pods in
// the control namespace, on its port. The internet rule excludes every private
// range, so without it a server could not reach the Service at all.
func serverToArtifacts(p Params) networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(ArtifactsPort)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": p.ControlNamespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: artifactsLabels()},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
	}
}
