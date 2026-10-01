package platform

import (
	"fmt"

	"felis.lolicon.best/internal/archivetransfer"
	"felis.lolicon.best/internal/distributed"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const ArchiveName = "felis-archive"
const ArchiveSecret = "felis-archive-key"
const ArchivePort int32 = 8090

func archiveKeyEnv() corev1.EnvVar {
	return corev1.EnvVar{Name: archivetransfer.KeyEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: ArchiveSecret}, Key: "key"}}}
}

func distributedEnv(p Params) []corev1.EnvVar {
	return []corev1.EnvVar{{Name: "FELIS_DISTRIBUTED", Value: "true"}, {Name: "FELIS_CONTROLLER_NODE", Value: p.ControllerNode}, {Name: "FELIS_ARCHIVE_URL", Value: fmt.Sprintf("http://%s.%s.svc:%d", ArchiveName, p.MinecraftNamespace, ArchivePort)}, {Name: "FELIS_EGRESS_PROBE", Value: p.EgressProbe}, archiveKeyEnv()}
}

func ArchiveDeployment(p Params) *appsv1.Deployment {
	labels := map[string]string{"app.kubernetes.io/name": ArchiveName}
	container := corev1.Container{Name: ArchiveName, Image: p.FelisImage, Command: []string{felisBinaryPath, "archive-serve"}, Args: []string{"--root", p.ArchiveLocalPath}, Env: []corev1.EnvVar{archiveKeyEnv()}, Ports: []corev1.ContainerPort{{Name: "archive", ContainerPort: ArchivePort}}, Resources: controlPlaneResources(), SecurityContext: &corev1.SecurityContext{RunAsUser: int64Ptr(0), RunAsNonRoot: boolPtr(false), AllowPrivilegeEscalation: boolPtr(false), Privileged: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}, VolumeMounts: []corev1.VolumeMount{{Name: "archives", MountPath: p.ArchiveLocalPath}}}
	container.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("archive")}}}
	container.LivenessProbe = container.ReadinessProbe.DeepCopy()
	return &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: ArchiveName, Namespace: p.MinecraftNamespace, Labels: labels}, Spec: appsv1.DeploymentSpec{Replicas: int32Ptr(1), Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}, Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{NodeSelector: controllerSelector(p), AutomountServiceAccountToken: boolPtr(false), SecurityContext: &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}, Containers: []corev1.Container{container}, Volumes: []corev1.Volume{{Name: "archives", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: p.BackupPVC}}}}}}}}
}
func ArchiveService(p Params) *corev1.Service {
	return &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metav1.ObjectMeta{Name: ArchiveName, Namespace: p.MinecraftNamespace}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app.kubernetes.io/name": ArchiveName}, Ports: []corev1.ServicePort{{Name: "archive", Port: ArchivePort, TargetPort: intstr.FromInt32(ArchivePort)}}}}
}

// Cluster grants are read-only and bound only to A's API/operator/reaper identities.
func DistributedRBAC(p Params) []Object {
	var out []Object
	for _, entry := range []struct {
		name, ns string
		rules    []rbacv1.PolicyRule
	}{
		{SAAPI, p.ControlNamespace, []rbacv1.PolicyRule{rule([]string{groupCore}, []string{"nodes"}, []string{"get", "list"}), rule([]string{groupCore}, []string{"persistentvolumes"}, []string{"get"})}},
		{SAOperator, p.ControlNamespace, []rbacv1.PolicyRule{rule([]string{groupCore}, []string{"nodes"}, []string{"get"})}},
		{SAReaper, p.MinecraftNamespace, []rbacv1.PolicyRule{rule([]string{groupCore}, []string{"nodes"}, []string{"get"})}},
	} {
		name := entry.name + "-" + p.MinecraftNamespace + "-nodes"
		out = append(out, &rbacv1.ClusterRole{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"}, ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: entry.rules}, &rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: name}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: entry.name, Namespace: entry.ns}}})
	}
	return out
}

func ArchiveNetworkPolicies(p Params) []Object {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	archive := metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": ArchiveName}}
	job := metav1.LabelSelector{MatchLabels: map[string]string{distributed.LabelTransfer: "true"}}
	control := networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": p.ControlNamespace}}, PodSelector: &metav1.LabelSelector{MatchLabels: controlPlanePodLabels(ComponentAPI)}}
	reaper := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: controlPlanePodLabels(ComponentReaper)}}
	peers := []networkingv1.NetworkPolicyPeer{control, reaper, {PodSelector: &job}}
	ingress := &networkingv1.NetworkPolicy{TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}, ObjectMeta: metav1.ObjectMeta{Name: "felis-archive", Namespace: p.MinecraftNamespace}, Spec: networkingv1.NetworkPolicySpec{PodSelector: archive, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, Ingress: []networkingv1.NetworkPolicyIngressRule{{From: peers, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: portPtr(ArchivePort)}}}}}}
	egress := &networkingv1.NetworkPolicy{TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}, ObjectMeta: metav1.ObjectMeta{Name: "felis-archive-jobs", Namespace: p.MinecraftNamespace}, Spec: networkingv1.NetworkPolicySpec{PodSelector: job, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, Egress: []networkingv1.NetworkPolicyEgressRule{
		{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &archive}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: portPtr(ArchivePort)}}},
		{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}}}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: portPtr(53)}, {Protocol: &tcp, Port: portPtr(53)}}},
	}}}
	// Files/export maintenance Pods may send results only to A's existing upload receiver.
	// Transfer Pods are excluded so grants remain limited to the archive service.
	maintenance := &networkingv1.NetworkPolicy{TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}, ObjectMeta: metav1.ObjectMeta{Name: "felis-maintenance-egress", Namespace: p.MinecraftNamespace}, Spec: networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "app.kubernetes.io/managed-by", Operator: metav1.LabelSelectorOpIn, Values: []string{"felis-files", "felis-export", "felis-restore", "felis-backup"}},
			{Key: distributed.LabelTransfer, Operator: metav1.LabelSelectorOpDoesNotExist},
		}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		Egress: []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{control}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: portPtr(8081)}}},
			egress.Spec.Egress[1],
		},
	}}
	return []Object{ingress, egress, maintenance}
}
func portPtr(p int32) *intstr.IntOrString { v := intstr.FromInt32(p); return &v }
