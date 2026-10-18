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

const (
	// ComponentArtifacts must stay outside part-of=felis-control-plane so control-plane network peers cannot select it.
	ComponentArtifacts = "artifacts"
	ArtifactsName      = "felis-artifacts"
	ArtifactsPort      = int32(8090)

	artifactsStorageSize = "10Gi"
	artifactsDataPath    = "/cache"
)

func ArtifactCacheURL(controlNamespace string) string {
	if controlNamespace == "" {
		controlNamespace = DefaultControlNamespace
	}
	return fmt.Sprintf("http://%s.%s.svc:%d", ArtifactsName, controlNamespace, ArtifactsPort)
}

func artifactsLabels() map[string]string {
	return map[string]string{LabelName: appName, LabelComponent: ComponentArtifacts}
}

// artifactsMaxBytes caps cache usage at 90% of the requested volume because local-path PVC capacity is not enforced.
func artifactsMaxBytes(p Params) int64 {
	q := resource.MustParse(p.ArtifactsStorage)
	return q.Value() / 10 * 9
}

// Cache data is disposable; failed cache access falls back to upstream downloads, so its volume is not replicated.
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

// artifactsPVC uses the default storage class even under controller HA because cached artifacts can be downloaded again.
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

// ArtifactsNetworkPolicies allow game-pod ingress and public upstream egress while excluding private destinations.
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

// Add explicit cache egress because the general server internet rule excludes private Service ranges.
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
