package main

import (
	"strings"
	"testing"
)

// TestOriginFenceRuleset pins the security-relevant shape of the nftables fence that
// closes the panel NodePort to the public interface after the Cloudflare tunnel is
// up. The real `nft` apply is INTEGRATION-ONLY; what MUST hold regardless of the host
// is the ruleset itself, so it is asserted here.
func TestOriginFenceRuleset(t *testing.T) {
	const port = 30443
	rs := originFenceRuleset(port)

	// The private table so the fence adds/removes atomically without touching other
	// host rules.
	if !strings.Contains(rs, "table inet "+felisEdgeTable) {
		t.Errorf("ruleset missing dedicated inet table %q:\n%s", felisEdgeTable, rs)
	}
	// inet family (not ip) so a public IPv6 NodePort is fenced too.
	if strings.Contains(rs, "table ip "+felisEdgeTable) {
		t.Errorf("ruleset must use the inet family to cover IPv6, not ip:\n%s", rs)
	}
	// prerouting hook at priority -300 (raw), which runs BEFORE kube-proxy's NodePort
	// DNAT (dstnat, -100). A filter/INPUT rule would miss the DNAT'd, then-FORWARDed
	// NodePort packet; this ordering is what makes the fence actually catch it.
	if !strings.Contains(rs, "hook prerouting priority -300") {
		t.Errorf("ruleset must hook prerouting at priority -300 (before kube-proxy dstnat):\n%s", rs)
	}
	// Loopback is accepted first so the connector's 127.0.0.1 origin hop is never cut.
	if !strings.Contains(rs, `iif "lo" accept`) {
		t.Errorf("ruleset must accept loopback before dropping, or it cuts the connector origin hop:\n%s", rs)
	}
	// The port itself is dropped.
	if !strings.Contains(rs, "tcp dport 30443 drop") {
		t.Errorf("ruleset must drop tcp dport 30443:\n%s", rs)
	}
	// The drop must come AFTER the loopback accept, or loopback would be dropped too.
	loIdx := strings.Index(rs, `iif "lo" accept`)
	dropIdx := strings.Index(rs, "tcp dport 30443 drop")
	if loIdx < 0 || dropIdx < 0 || loIdx > dropIdx {
		t.Errorf("loopback accept must precede the port drop:\n%s", rs)
	}
}

// TestOriginFenceRulesetHonorsPort proves the rule targets the configured NodePort,
// not a hardcoded 30443 — a deployment that overrode FELIS_PANEL_NODEPORT must fence
// the port it actually exposed.
func TestOriginFenceRulesetHonorsPort(t *testing.T) {
	rs := originFenceRuleset(30500)
	if !strings.Contains(rs, "tcp dport 30500 drop") {
		t.Errorf("ruleset must fence the configured port 30500:\n%s", rs)
	}
	if strings.Contains(rs, "30443") {
		t.Errorf("ruleset must not carry the default 30443 when a different port is configured:\n%s", rs)
	}
}

// TestConnectorConnCount covers the two JSON shapes cloudflared has emitted for
// `tunnel info --output json`, plus the safe-zero fallbacks — the gate that stops the
// fence from closing 30443 while the tunnel is dead.
func TestConnectorConnCount(t *testing.T) {
	cases := []struct {
		name string
		json string
		want int
	}{
		{"top-level conns", `{"id":"t","conns":[{"colo_name":"sin08"},{"colo_name":"sin09"}]}`, 2},
		{"nested connectors", `{"id":"t","connectors":[{"id":"c","conns":[{"colo_name":"sin08"}]}]}`, 1},
		{"both shapes summed", `{"conns":[{}],"connectors":[{"conns":[{}]},{"conns":[{}]}]}`, 3},
		{"healthy-but-empty", `{"id":"t","conns":[],"connectors":[]}`, 0},
		{"no connections field", `{"id":"t","name":"felis"}`, 0},
		{"garbage is not a healthy tunnel", `not json`, 0},
		{"empty", ``, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := connectorConnCount([]byte(c.json)); got != c.want {
				t.Errorf("connectorConnCount(%s) = %d, want %d", c.json, got, c.want)
			}
		})
	}
}
