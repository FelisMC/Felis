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

// gamePort is the Minecraft TCP port, sourced from the operator so the policy and
// the StatefulSet container port are one source of truth.
const gamePort = operator.GamePort

// rconPort is the RCON port the allow-rcon policy opens, sourced from the operator
// so the policy and the server container's default RCON port are one source of
// truth.
//
// LIMITATION (honestly labeled, not verifiable without a cluster): RCON is
// per-server overridable via spec.rcon.port (internal/operator.rconPort), but this
// is one namespace-wide policy that can open only a single port. It opens the
// default. A server that overrides spec.rcon.port to a non-default value would have
// its RCON port denied by this fence, so the operator's readiness prober could not
// reach it. The supported deployment keeps the default RCON port; a per-server-port
// deployment would need per-server NetworkPolicies, deferred until a concrete need
// exists.
const rconPort = operator.DefaultRconPort

// serverPodSelector matches every operator-managed Minecraft server pod by the
// exact labels the operator stamps (internal/operator.labelsFor). Sourcing the
// values from the operator package means the fence can never silently stop
// matching the pods it protects.
func serverPodSelector() metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{
		v1alpha1.LabelManagedBy: operator.ManagedByValue,
		v1alpha1.LabelComponent: operator.ComponentValue,
	}}
}

// MinecraftNetworkPolicies renders the ingress fence for the minecraft namespace
// (spec §20, §21): a default-deny baseline, RCON (25575) reachable only from the
// {api, operator} control-plane pods, and the game port (25565) reachable only
// from the off-cluster Velocity proxy host(s). NetworkPolicies are additive, so
// the union admits exactly those two paths to server pods and denies all else.
func MinecraftNetworkPolicies(p Params) []*networkingv1.NetworkPolicy {
	p = p.withDefaults()
	return []*networkingv1.NetworkPolicy{
		defaultDenyIngress(p),
		allowRConFromControlPlane(p),
		allowGameFromVelocity(p),
	}
}

// defaultDenyIngress selects every pod in the namespace and permits no ingress —
// the baseline that makes the two allow policies a strict allowlist.
func defaultDenyIngress(p Params) *networkingv1.NetworkPolicy {
	return netpol("felis-default-deny-ingress", p.MinecraftNamespace,
		metav1.LabelSelector{}, // empty selector = all pods in the namespace
		nil,                    // nil ingress rules = deny all ingress
	)
}

// allowRConFromControlPlane opens 25575 on server pods to the felis-api and
// felis-operator pods only. Both dial RCON: felis-api for console writes
// (internal/api.console) and felis-operator for the readiness prober
// (internal/operator.prober). felis-reaper never opens RCON, so it is excluded.
//
// The single peer combines a namespaceSelector AND a podSelector, which K8s reads
// as an intersection: pods matching the podSelector that also live in the control
// namespace. Splitting them into two peers would be a union (allow ALL pods in
// the control ns OR api/operator pods anywhere) — the wrong, wider semantics.
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

// allowGameFromVelocity opens 25565 on server pods to Velocity proxy host(s) by
// ipBlock. The supported proxy runs outside the pod network (on the k3s node or a
// separate host), so the peer is an ipBlock rather than a podSelector. Kubernetes
// always permits resident-node traffic; these rules constrain other sources.
//
// With no VelocityCIDRs the policy carries NO ingress rule, never an empty-From
// rule (which K8s would read as allow-all). That denies non-node game traffic;
// resident-node traffic remains outside NetworkPolicy's blocking capability. The
// manifest generator still refuses an empty list so remote proxies fail loudly.
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

// serverEgressExceptV4 / V6 are the destinations a game server never reaches
// through the internet rule: every private, shared, link-local, loopback,
// multicast and reserved range. They cover the pod and Service CIDRs of any stock
// k3s/k8s install (10.42/16, 10.43/16), the node's private addresses, cloud
// metadata (169.254.169.254) and the operator's LAN.
var (
	serverEgressExceptV4 = []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "240.0.0.0/4",
	}
	serverEgressExceptV6 = []string{"::1/128", "fc00::/7", "fe80::/10", "ff00::/8"}
)

// ServerEgressPolicies render the egress fence for game server pods. A server runs
// code its owner chose — plugins, mods, a whole image — so the namespace used to
// be a launch pad: any server could push to the unauthenticated registry, dial
// felis-api's internal face, PostgreSQL on the node, or the kube API. Now:
//
//   - every server may resolve names and reach the public internet (plugin
//     updates, resource packs, web maps) and nothing private;
//   - the login system server additionally reaches felis-api's internal face,
//     the one platform service it is built to call.
//
// Policies are additive, so the login pod gets the union of both.
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
			continue // `felis manifests` rejects these before rendering
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
				// Name resolution through the cluster resolver: the DNS Service
				// sits inside 10/8, which the internet rule excludes.
				{
					To:    []networkingv1.NetworkPolicyPeer{build.ClusterDNSPeer()},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
				},
				{To: peers},
			},
		},
	}
}

// loginToInternalAPI opens felis-api's internal face (8081) to the login system
// server only. The selector needs both the reserved name and the setup-owned
// system-role label the operator copies onto that pod — the same pair that decides
// who receives FELIS_SERVICE_TOKEN (internal/operator buildEnv), so a user server
// can never match it by picking a name.
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

// RegistryIngressPolicy fences the registry pod: only build pods reach its port.
// Everything else that uses the registry runs on the node — containerd's pulls and
// the installer's pushes both arrive through the loopback hostPort — and Kubernetes
// never blocks resident-node traffic. Write authorization is the gate's job; this
// policy keeps every other pod from even trying.
func RegistryIngressPolicy(p Params) *networkingv1.NetworkPolicy {
	p = p.withDefaults()
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(p.RegistryPort)
	np := netpol("felis-registry-ingress", p.RegistryNamespace,
		metav1.LabelSelector{MatchLabels: registryLabels()},
		[]networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": p.BuildNamespace},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		}},
	)
	return np
}

// netpol assembles an ingress-only NetworkPolicy. A nil/empty ingress slice with
// PolicyTypeIngress is the canonical "deny all ingress" shape.
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
