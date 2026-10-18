package platform

import (
	"net"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/operator"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const gamePort = operator.GamePort

// RCON is namespace-wide on one default port; changing supported ports requires updating both the CRD constraint and network policy.
const rconPort = operator.DefaultRconPort

func serverPodSelector() metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{
		v1alpha1.LabelManagedBy: operator.ManagedByValue,
		v1alpha1.LabelComponent: operator.ComponentValue,
	}}
}

// MinecraftNetworkPolicies allow game traffic from Velocity and RCON from API or operator over a default-deny baseline.
func MinecraftNetworkPolicies(p Params) []*networkingv1.NetworkPolicy {
	p = p.withDefaults()
	return []*networkingv1.NetworkPolicy{
		defaultDenyIngress(p),
		allowRConFromControlPlane(p),
		allowGameFromVelocity(p),
	}
}

func defaultDenyIngress(p Params) *networkingv1.NetworkPolicy {
	return netpol("felis-default-deny-ingress", p.MinecraftNamespace,
		metav1.LabelSelector{},
		nil,
	)
}

// Keep namespaceSelector and podSelector in one peer for intersection; separate peers would widen access by union.
func allowRConFromControlPlane(p Params) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(rconPort)
	np := netpol("felis-allow-rcon-from-control-plane", p.MinecraftNamespace,
		serverPodSelector(),
		[]networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": p.ControlNamespace},
				},
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{LabelPartOf: controlPlanePartOf},
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      LabelComponent,
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{ComponentAPI, ComponentOperator},
					}},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		}},
	)
	return np
}

// No VelocityCIDRs means no allow rule, not an empty From that permits all; resident-node traffic bypasses NetworkPolicy.
func allowGameFromVelocity(p Params) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(gamePort)

	var ingress []networkingv1.NetworkPolicyIngressRule
	if len(p.VelocityCIDRs) > 0 {
		peers := make([]networkingv1.NetworkPolicyPeer, 0, len(p.VelocityCIDRs))
		for _, cidr := range p.VelocityCIDRs {
			peers = append(peers, networkingv1.NetworkPolicyPeer{
				IPBlock: &networkingv1.IPBlock{CIDR: cidr},
			})
		}
		ingress = []networkingv1.NetworkPolicyIngressRule{{
			From:  peers,
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		}}
	}
	return netpol("felis-allow-game-from-velocity", p.MinecraftNamespace, serverPodSelector(), ingress)
}

// serverEgressExceptV4 and V6 exclude private, shared, link-local, loopback, multicast and reserved destinations.
var (
	serverEgressExceptV4 = []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "240.0.0.0/4",
	}
	serverEgressExceptV6 = []string{"::1/128", "fc00::/7", "fe80::/10", "ff00::/8"}
)

// ServerEgressPolicies allow public internet and artifact-cache access; only the login system pod also reaches the internal API.
func ServerEgressPolicies(p Params) []*networkingv1.NetworkPolicy {
	p = p.withDefaults()
	return []*networkingv1.NetworkPolicy{serverEgress(p), loginToInternalAPI(p)}
}

func serverEgress(p Params) *networkingv1.NetworkPolicy {
	v4 := append([]string{}, serverEgressExceptV4...)
	v6 := append([]string{}, serverEgressExceptV6...)
	for _, c := range p.ServerEgressDenyCIDRs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		if n.IP.To4() != nil {
			v4 = append(v4, n.String())
		} else {
			v6 = append(v6, n.String())
		}
	}
	peers := []networkingv1.NetworkPolicyPeer{
		{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: v4}},
		{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: v6}},
	}
	for _, c := range p.ServerEgressAllowCIDRs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: n.String()}})
		}
	}
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	dns := intstr.FromInt32(53)
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "felis-server-egress", Namespace: p.MinecraftNamespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: serverPodSelector(),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To:    []networkingv1.NetworkPolicyPeer{build.ClusterDNSPeer()},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
				},
				{To: peers},
				serverToArtifacts(p),
			},
		},
	}
}

// loginToInternalAPI requires both reserved name and setup-owned role labels, matching the service-token injection gate.
func loginToInternalAPI(p Params) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(apiInternalPort)
	sel := serverPodSelector()
	sel.MatchLabels[v1alpha1.LabelServer] = naming.SystemLoginServer
	sel.MatchLabels[v1alpha1.LabelSystemRole] = naming.SystemLoginServer
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "felis-login-to-internal-api", Namespace: p.MinecraftNamespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: sel,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": p.ControlNamespace},
					},
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						LabelPartOf:    controlPlanePartOf,
						LabelComponent: ComponentAPI,
					}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}},
		},
	}
}

// RegistryIngressPolicy admits build and API Pods; node pulls use loopback, while the registry gate separately authorizes writes.
func RegistryIngressPolicy(p Params) *networkingv1.NetworkPolicy {
	p = p.withDefaults()
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(p.RegistryPort)
	np := netpol("felis-registry-ingress", p.RegistryNamespace,
		metav1.LabelSelector{MatchLabels: registryLabels()},
		[]networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{
				{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": p.BuildNamespace},
					},
				},
				{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": p.ControlNamespace},
					},
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						LabelPartOf:    controlPlanePartOf,
						LabelComponent: ComponentAPI,
					}},
				},
			},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		}},
	)
	for _, cidr := range p.RegistryNodeCIDRs {
		np.Spec.Ingress[0].From = append(np.Spec.Ingress[0].From, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	return np
}

// netpol with ingress policy type and an empty rule list denies all ingress.
func netpol(name, ns string, sel metav1.LabelSelector, ingress []networkingv1.NetworkPolicyIngressRule) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: sel,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     ingress,
		},
	}
}
