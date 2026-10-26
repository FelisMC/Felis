package platform

import (
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/operator"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

	// One peer combining both selectors (AND); two peers would be an OR and far wider.
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

	// Component must be api or operator; reaper is excluded.
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

	// Default RCON port, pinned to 25575 so a change to the operator default fails here.
	if operator.DefaultRconPort != 25575 {
		t.Errorf("operator.DefaultRconPort = %d, want 25575 (conventional RCON port)", operator.DefaultRconPort)
	}
	assertSinglePort(t, rule.Ports, int(operator.DefaultRconPort))
}

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

func TestServerEgress_OnlyDNSAndPublicInternet(t *testing.T) {
	p := testParams()
	p.ServerEgressDenyCIDRs = []string{"203.0.113.7/32", "2001:db8::1/128"}
	p.ServerEgressAllowCIDRs = []string{"10.9.8.0/24"}
	eg := npByName(t, ServerEgressPolicies(p), "felis-server-egress")

	if !hasPolicyType(eg, networkingv1.PolicyTypeEgress) || hasPolicyType(eg, networkingv1.PolicyTypeIngress) {
		t.Fatalf("server egress policy types = %v, want Egress only (ingress stays with the default-deny set)", eg.Spec.PolicyTypes)
	}
	if !selectorEquals(eg.Spec.PodSelector, serverPodSelector()) {
		t.Errorf("server egress selects %v, want every server pod", eg.Spec.PodSelector)
	}
	if len(eg.Spec.Egress) != 3 {
		t.Fatalf("egress rules = %d, want DNS + internet + the artifact cache", len(eg.Spec.Egress))
	}
	cache := eg.Spec.Egress[2]
	if len(cache.To) != 1 || cache.To[0].IPBlock != nil ||
		cache.To[0].NamespaceSelector == nil || cache.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != p.withDefaults().ControlNamespace ||
		cache.To[0].PodSelector == nil || cache.To[0].PodSelector.MatchLabels[LabelComponent] != ComponentArtifacts {
		t.Errorf("cache rule = %+v, want only the artifact cache's pods in the control namespace", cache)
	}
	assertSinglePort(t, cache.Ports, int(ArtifactsPort))
	dns := eg.Spec.Egress[0]
	if len(dns.To) != 1 || dns.To[0].PodSelector == nil || dns.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" || len(dns.Ports) != 2 {
		t.Errorf("DNS rule = %+v, want port 53 udp+tcp to the cluster DNS pods", dns)
	}
	for _, port := range dns.Ports {
		if port.Port == nil || port.Port.IntValue() != 53 {
			t.Errorf("DNS rule port = %v, want 53", port.Port)
		}
	}

	net := eg.Spec.Egress[1]
	if len(net.Ports) != 0 {
		t.Errorf("internet rule must not be port-restricted, got %v", net.Ports)
	}
	blocks := map[string][]string{}
	for _, peer := range net.To {
		if peer.IPBlock == nil || peer.PodSelector != nil || peer.NamespaceSelector != nil {
			t.Fatalf("internet rule peer %+v must be a bare ipBlock", peer)
		}
		blocks[peer.IPBlock.CIDR] = peer.IPBlock.Except
	}
	for _, want := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "127.0.0.0/8", "203.0.113.7/32"} {
		if !contains(blocks["0.0.0.0/0"], want) {
			t.Errorf("0.0.0.0/0 except = %v, missing %s", blocks["0.0.0.0/0"], want)
		}
	}
	for _, want := range []string{"fc00::/7", "fe80::/10", "::1/128", "2001:db8::1/128"} {
		if !contains(blocks["::/0"], want) {
			t.Errorf("::/0 except = %v, missing %s", blocks["::/0"], want)
		}
	}
	if except, ok := blocks["10.9.8.0/24"]; !ok || len(except) != 0 {
		t.Errorf("allow CIDR 10.9.8.0/24 must be its own peer, blocks=%v", blocks)
	}
	if len(blocks) != 3 {
		t.Errorf("internet rule peers = %v, want v4 + v6 + one allow CIDR", blocks)
	}
}

func TestLoginToInternalAPI_SelectsOnlyTheSystemLoginPod(t *testing.T) {
	p := testParams().withDefaults()
	np := npByName(t, ServerEgressPolicies(p), "felis-login-to-internal-api")
	sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
	if err != nil {
		t.Fatal(err)
	}
	server := map[string]string{
		v1alpha1.LabelManagedBy: operator.ManagedByValue,
		v1alpha1.LabelComponent: operator.ComponentValue,
	}
	with := func(extra map[string]string) labels.Set {
		m := labels.Set{}
		for k, v := range server {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	if !sel.Matches(with(map[string]string{v1alpha1.LabelServer: "login", v1alpha1.LabelSystemRole: "login"})) {
		t.Error("the system login pod must match")
	}
	for name, l := range map[string]map[string]string{
		"user server":                  {v1alpha1.LabelServer: "survival"},
		"login name without the role":  {v1alpha1.LabelServer: "login"},
		"role label on another server": {v1alpha1.LabelServer: "survival", v1alpha1.LabelSystemRole: "login"},
		"lobby":                        {v1alpha1.LabelServer: "lobby", v1alpha1.LabelSystemRole: "lobby"},
	} {
		if sel.Matches(with(l)) {
			t.Errorf("%s must not reach felis-api's internal face", name)
		}
	}

	if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].To) != 1 {
		t.Fatalf("login egress shape = %+v, want one rule with one peer", np.Spec.Egress)
	}
	rule := np.Spec.Egress[0]
	assertSinglePort(t, rule.Ports, int(apiInternalPort))
	peer := rule.To[0]
	if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != p.ControlNamespace {
		t.Errorf("login egress namespace = %v, want %s", peer.NamespaceSelector, p.ControlNamespace)
	}
	if peer.PodSelector == nil || !mapSelectorMatches(peer.PodSelector.MatchLabels, APIDeployment(p).Spec.Template.Labels) {
		t.Errorf("login egress pod selector %v must select the api pod", peer.PodSelector)
	}
	if mapSelectorMatches(peer.PodSelector.MatchLabels, OperatorDeployment(p).Spec.Template.Labels) {
		t.Error("login egress must not reach the operator")
	}
}

func TestRegistryIngress_BuildNamespaceAndAPI(t *testing.T) {
	p := testParams().withDefaults()
	np := RegistryIngressPolicy(p)
	if np.Namespace != p.RegistryNamespace {
		t.Errorf("registry ingress namespace = %s, want %s", np.Namespace, p.RegistryNamespace)
	}
	if !mapSelectorMatches(np.Spec.PodSelector.MatchLabels, registryDeployment(p).Spec.Template.Labels) {
		t.Errorf("registry ingress selector %v does not select the registry pod", np.Spec.PodSelector)
	}
	if mapSelectorMatches(np.Spec.PodSelector.MatchLabels, APIDeployment(p).Spec.Template.Labels) {
		t.Error("registry ingress must not also fence the api pod")
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) != 2 {
		t.Fatalf("registry ingress shape = %+v, want one rule, two peers", np.Spec.Ingress)
	}
	peer := np.Spec.Ingress[0].From[0]
	if peer.PodSelector != nil || peer.IPBlock != nil || peer.NamespaceSelector == nil ||
		peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != p.BuildNamespace {
		t.Errorf("registry ingress peer = %+v, want the whole %s namespace", peer, p.BuildNamespace)
	}
	// The second peer is felis-api alone: it selects the api pod and not the
	// operator's, both of which live in the control namespace.
	api := np.Spec.Ingress[0].From[1]
	if api.IPBlock != nil || api.NamespaceSelector == nil || api.PodSelector == nil ||
		api.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != p.ControlNamespace {
		t.Fatalf("registry ingress api peer = %+v, want pods in %s", api, p.ControlNamespace)
	}
	if !mapSelectorMatches(api.PodSelector.MatchLabels, APIDeployment(p).Spec.Template.Labels) {
		t.Errorf("registry ingress api peer %v does not select the api pod", api.PodSelector)
	}
	if mapSelectorMatches(api.PodSelector.MatchLabels, OperatorDeployment(p).Spec.Template.Labels) {
		t.Errorf("registry ingress api peer %v also selects the operator pod", api.PodSelector)
	}
	assertSinglePort(t, np.Spec.Ingress[0].Ports, int(p.RegistryPort))
}

func selectorEquals(a, b metav1.LabelSelector) bool {
	if len(a.MatchLabels) != len(b.MatchLabels) || len(a.MatchExpressions)+len(b.MatchExpressions) != 0 {
		return false
	}
	for k, v := range a.MatchLabels {
		if b.MatchLabels[k] != v {
			return false
		}
	}
	return true
}

func TestArtifactsNetworkPolicies(t *testing.T) {
	p := testParams()
	pols := ArtifactsNetworkPolicies(p)
	in := npByName(t, pols, "felis-artifacts-ingress")
	if !hasPolicyType(in, networkingv1.PolicyTypeIngress) || len(in.Spec.Ingress) != 1 || len(in.Spec.Ingress[0].From) != 1 {
		t.Fatalf("ingress = %+v", in.Spec)
	}
	from := in.Spec.Ingress[0].From[0]
	if from.NamespaceSelector == nil || from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != p.withDefaults().MinecraftNamespace ||
		from.PodSelector == nil || !selectorEquals(*from.PodSelector, serverPodSelector()) {
		t.Errorf("ingress peer = %+v, want game server pods in the minecraft namespace", from)
	}
	assertSinglePort(t, in.Spec.Ingress[0].Ports, int(ArtifactsPort))

	eg := npByName(t, pols, "felis-artifacts-egress")
	if !hasPolicyType(eg, networkingv1.PolicyTypeEgress) || len(eg.Spec.Egress) != 2 {
		t.Fatalf("egress = %+v, want DNS + internet", eg.Spec)
	}
	for _, peer := range eg.Spec.Egress[1].To {
		if peer.IPBlock == nil {
			t.Fatalf("internet peer %+v is not an ipBlock", peer)
		}
		for _, private := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "127.0.0.0/8", "fc00::/7"} {
			if strings.Contains(private, ":") != strings.Contains(peer.IPBlock.CIDR, ":") {
				continue
			}
			if !contains(peer.IPBlock.Except, private) {
				t.Errorf("%s except = %v, missing %s", peer.IPBlock.CIDR, peer.IPBlock.Except, private)
			}
		}
	}
	if ArtifactCacheURL("felis") != "http://felis-artifacts.felis.svc:8090" {
		t.Errorf("cache URL = %s", ArtifactCacheURL("felis"))
	}
}
