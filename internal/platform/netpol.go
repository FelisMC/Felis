package platform

import (
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
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
