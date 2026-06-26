package platform

import (
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/operator"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func npByName(t *testing.T, nps []*networkingv1.NetworkPolicy, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	for _, np := range nps {
		if np.Name == name {
			return np
		}
	}
	t.Fatalf("network policy %q not found", name)
	return nil
}

// TestServerSelector_MatchesOperatorLabels proves the fence selects exactly the
// pods the operator labels. If the operator ever renamed its label values the
// policy would silently stop protecting the pods — this couples the two.
func TestServerSelector_MatchesOperatorLabels(t *testing.T) {
	sel := serverPodSelector()
	want := map[string]string{
		v1alpha1.LabelManagedBy: operator.ManagedByValue,
		v1alpha1.LabelComponent: operator.ComponentValue,
	}
	if len(sel.MatchLabels) != len(want) {
		t.Fatalf("selector has %d labels, want %d", len(sel.MatchLabels), len(want))
	}
	for k, v := range want {
		if sel.MatchLabels[k] != v {
			t.Errorf("selector[%q] = %q, want %q", k, sel.MatchLabels[k], v)
		}
	}
	// Guard against the values being empty (a typo making the selector match-all-ish).
	if operator.ManagedByValue == "" || operator.ComponentValue == "" {
		t.Fatal("operator label values must be non-empty")
	}
}

// TestDefaultDeny enforces the baseline: select all pods, declare Ingress policy,
// admit nothing.
func TestDefaultDeny(t *testing.T) {
	nps := MinecraftNetworkPolicies(testParams())
	dd := npByName(t, nps, "felis-default-deny-ingress")

	if len(dd.Spec.PodSelector.MatchLabels) != 0 || len(dd.Spec.PodSelector.MatchExpressions) != 0 {
		t.Error("default-deny must select ALL pods (empty podSelector)")
	}
	if !hasPolicyType(dd, networkingv1.PolicyTypeIngress) {
		t.Error("default-deny must declare the Ingress policy type")
	}
	if len(dd.Spec.Ingress) != 0 {
		t.Error("default-deny must have NO ingress rules (deny all)")
	}
}

// TestAllowRcon enforces the AND-semantics control-plane peer on port 25575.
func TestAllowRcon(t *testing.T) {
	nps := MinecraftNetworkPolicies(testParams())
	rcon := npByName(t, nps, "felis-allow-rcon-from-control-plane")

	// Selects server pods, not all pods.
	if rcon.Spec.PodSelector.MatchLabels[v1alpha1.LabelComponent] != operator.ComponentValue {
		t.Error("rcon policy must select server pods")
	}
	if len(rcon.Spec.Ingress) != 1 {
		t.Fatalf("rcon policy must have exactly 1 ingress rule, got %d", len(rcon.Spec.Ingress))
	}
	rule := rcon.Spec.Ingress[0]

	// Exactly one peer, combining BOTH selectors (intersection = AND), never two
	// peers (which would be a union/OR — far wider).
	if len(rule.From) != 1 {
		t.Fatalf("rcon peer count = %d, want 1 (AND-combined); 2 would be OR semantics", len(rule.From))
	}
	peer := rule.From[0]
	if peer.NamespaceSelector == nil || peer.PodSelector == nil {
		t.Fatal("rcon peer must set BOTH namespaceSelector AND podSelector")
	}
	if got := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "felis" {
		t.Errorf("rcon namespaceSelector = %q, want control ns felis", got)
	}
	if peer.PodSelector.MatchLabels[LabelPartOf] != controlPlanePartOf {
		t.Error("rcon podSelector must require part-of=felis-control-plane")
	}

	// Component must be In {api, operator} — and NOT include reaper.
	var compReq *metav1.LabelSelectorRequirement
	for i := range peer.PodSelector.MatchExpressions {
		if peer.PodSelector.MatchExpressions[i].Key == LabelComponent {
			compReq = &peer.PodSelector.MatchExpressions[i]
		}
	}
	if compReq == nil {
		t.Fatal("rcon podSelector must constrain the component label")
	}
	if compReq.Operator != metav1.LabelSelectorOpIn {
		t.Errorf("component requirement operator = %q, want In", compReq.Operator)
	}
	if !contains(compReq.Values, ComponentAPI) || !contains(compReq.Values, ComponentOperator) {
		t.Errorf("component values = %v, want both api and operator", compReq.Values)
	}
	if contains(compReq.Values, ComponentReaper) {
		t.Error("reaper must NOT be allowed to reach RCON")
	}

	// Port = the operator's default RCON port (single source of truth, so the
	// prober can reach a default-port server through the fence). Pinned to the
	// concrete 25575 too, guarding an accidental change to the operator default.
	if operator.DefaultRconPort != 25575 {
		t.Errorf("operator.DefaultRconPort = %d, want 25575 (conventional RCON port)", operator.DefaultRconPort)
	}
	assertSinglePort(t, rule.Ports, int(operator.DefaultRconPort))
}

// TestAllowGame_WithCIDRs renders the ipBlock allow path on port 25565.
func TestAllowGame_WithCIDRs(t *testing.T) {
	p := testParams()
	p.VelocityCIDRs = []string{"10.0.0.5/32", "10.0.0.6/32"}
	game := npByName(t, MinecraftNetworkPolicies(p), "felis-allow-game-from-velocity")

	if len(game.Spec.Ingress) != 1 {
		t.Fatalf("game policy must have 1 ingress rule, got %d", len(game.Spec.Ingress))
	}
	rule := game.Spec.Ingress[0]
	if len(rule.From) != 2 {
		t.Fatalf("game peers = %d, want 2 ipBlocks", len(rule.From))
	}
	for i, peer := range rule.From {
		if peer.IPBlock == nil {
			t.Errorf("game peer %d must be an ipBlock (Velocity is off-cluster, not a pod)", i)
		}
		if peer.PodSelector != nil || peer.NamespaceSelector != nil {
			t.Errorf("game peer %d must NOT use pod/namespace selectors", i)
		}
	}
	if rule.From[0].IPBlock.CIDR != "10.0.0.5/32" {
		t.Errorf("game cidr[0] = %q", rule.From[0].IPBlock.CIDR)
	}
	if game.Spec.PodSelector.MatchLabels[v1alpha1.LabelComponent] != operator.ComponentValue {
		t.Error("game policy must select server pods")
	}
	assertSinglePort(t, rule.Ports, int(operator.GamePort))
}

// TestAllowGame_FailsClosed is the footgun guard: no VelocityCIDRs ⇒ a policy that
// admits NOBODY (no ingress rule), never an empty-From rule (which K8s reads as
// allow-all).
func TestAllowGame_FailsClosed(t *testing.T) {
	p := testParams()
	p.VelocityCIDRs = nil
	game := npByName(t, MinecraftNetworkPolicies(p), "felis-allow-game-from-velocity")

	if len(game.Spec.Ingress) != 0 {
		t.Fatalf("game policy with no CIDRs must have ZERO ingress rules (fail-closed), got %d", len(game.Spec.Ingress))
	}
	// Still a valid, selecting policy (so it actively denies, paired with default-deny).
	if game.Spec.PodSelector.MatchLabels[v1alpha1.LabelComponent] != operator.ComponentValue {
		t.Error("game policy must still select server pods even when fail-closed")
	}
	if !hasPolicyType(game, networkingv1.PolicyTypeIngress) {
		t.Error("game policy must declare Ingress policy type")
	}
}

func hasPolicyType(np *networkingv1.NetworkPolicy, pt networkingv1.PolicyType) bool {
	for _, t := range np.Spec.PolicyTypes {
		if t == pt {
			return true
		}
	}
	return false
}

func assertSinglePort(t *testing.T, ports []networkingv1.NetworkPolicyPort, want int) {
	t.Helper()
	if len(ports) != 1 {
		t.Fatalf("expected exactly 1 port, got %d", len(ports))
	}
	if ports[0].Port == nil || ports[0].Port.IntValue() != want {
		t.Errorf("port = %v, want %d", ports[0].Port, want)
	}
	if ports[0].Protocol == nil || *ports[0].Protocol != "TCP" {
		t.Errorf("protocol = %v, want TCP", ports[0].Protocol)
	}
}
