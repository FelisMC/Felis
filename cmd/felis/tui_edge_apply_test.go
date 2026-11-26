package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"felis.lolicon.best/internal/config"
)

func TestOriginFenceRuleset(t *testing.T) {
	const port = 30443
	rs := originFenceRuleset(port)

	// The private table so the fence adds/removes atomically without touching other
	// host rules, and declares then deletes it first so `nft -f` is idempotent.
	if !strings.Contains(rs, "table inet "+felisEdgeTable) {
		t.Errorf("ruleset missing dedicated inet table %q:\n%s", felisEdgeTable, rs)
	}
	delIdx := strings.Index(rs, "delete table inet "+felisEdgeTable)
	bodyIdx := strings.Index(rs, "table inet "+felisEdgeTable+" {")
	if delIdx < 0 || bodyIdx < 0 || delIdx > bodyIdx {
		t.Errorf("ruleset must delete table inet %s before recreating it for idempotent nft -f:\n%s", felisEdgeTable, rs)
	}
	// inet family (not ip) so a public IPv6 NodePort is fenced too.
	if strings.Contains(rs, "table ip "+felisEdgeTable) {
		t.Errorf("ruleset must use the inet family to cover IPv6, not ip:\n%s", rs)
	}
	// Filter before NodePort DNAT; INPUT rules miss forwarded packets.
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
	// The drop must come after the loopback accept, or loopback would be dropped too.
	loIdx := strings.Index(rs, `iif "lo" accept`)
	dropIdx := strings.Index(rs, "tcp dport 30443 drop")
	if loIdx < 0 || dropIdx < 0 || loIdx > dropIdx {
		t.Errorf("loopback accept must precede the port drop:\n%s", rs)
	}
}

func TestOriginFenceRulesetHonorsPort(t *testing.T) {
	rs := originFenceRuleset(30500)
	if !strings.Contains(rs, "tcp dport 30500 drop") {
		t.Errorf("ruleset must fence the configured port 30500:\n%s", rs)
	}
	if strings.Contains(rs, "30443") {
		t.Errorf("ruleset must not carry the default 30443 when a different port is configured:\n%s", rs)
	}
}

func TestEdgeFenceInstallAndRemove(t *testing.T) {
	dir := t.TempDir()
	rulesPath := filepath.Join(dir, "etc", "edge-fence.nft")
	unitPath := filepath.Join(dir, "systemd", edgeFenceUnitName)

	var calls []string
	fakeSystemctl := func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}

	if err := installEdgeFence(context.Background(), 30443, "/usr/sbin/nft", rulesPath, unitPath, fakeSystemctl); err != nil {
		t.Fatalf("installEdgeFence: %v", err)
	}

	info, err := os.Stat(rulesPath)
	if err != nil {
		t.Fatalf("stat rulesPath: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("rulesPath mode = %o, want 0600", perm)
	}
	rulesRaw, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("read rulesPath: %v", err)
	}
	if string(rulesRaw) != originFenceRuleset(30443) {
		t.Errorf("rules file mismatch:\n%s", string(rulesRaw))
	}

	unitRaw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unitPath: %v", err)
	}
	unitStr := string(unitRaw)
	for _, want := range []string{
		"After=network-pre.target firewalld.service nftables.service",
		"Before=k3s.service",
		"ExecStart=/usr/sbin/nft -f " + rulesPath,
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unitStr, want) {
			t.Errorf("unit lacks %q:\n%s", want, unitStr)
		}
	}

	wantInstallCalls := []string{
		"daemon-reload",
		"enable " + edgeFenceUnitName,
		"restart " + edgeFenceUnitName,
	}
	if strings.Join(calls, "|") != strings.Join(wantInstallCalls, "|") {
		t.Errorf("install systemctl calls = %v, want %v", calls, wantInstallCalls)
	}

	calls = nil
	if err := removeEdgeFence(context.Background(), rulesPath, unitPath, fakeSystemctl); err != nil {
		t.Fatalf("removeEdgeFence: %v", err)
	}
	wantRemoveCalls := []string{
		"disable --now " + edgeFenceUnitName,
		"daemon-reload",
	}
	if strings.Join(calls, "|") != strings.Join(wantRemoveCalls, "|") {
		t.Errorf("remove systemctl calls = %v, want %v", calls, wantRemoveCalls)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("unitPath still exists after remove: %v", err)
	}
	if _, err := os.Stat(rulesPath); !os.IsNotExist(err) {
		t.Errorf("rulesPath still exists after remove: %v", err)
	}

	// Idempotent when already removed.
	calls = nil
	if err := removeEdgeFence(context.Background(), rulesPath, unitPath, fakeSystemctl); err != nil {
		t.Fatalf("second removeEdgeFence: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("second removeEdgeFence invoked systemctl unexpectedly: %v", calls)
	}
}

func TestProxyPanelPortFollowsAccessMethod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "felis-link.properties")
	if err := os.WriteFile(path, []byte("service-token=secret\npanel-hostname=console.example.org\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	nodePort := func() int { return 30443 }
	apply := func(auth config.AuthConfig) string {
		t.Helper()
		if err := syncProxyPanelPort(path, panelLinkPort(auth, nodePort)); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	if got := apply(config.AuthConfig{}); !strings.Contains(got, "\npanel-port=30443\n") || !strings.HasPrefix(got, "service-token=secret\n") {
		t.Fatalf("direct access must give the proxy the NodePort and keep its token:\n%s", got)
	}
	if got := apply(config.AuthConfig{ClientIPHeader: "X-Forwarded-For"}); !strings.Contains(got, "\npanel-port=\n") {
		t.Fatalf("a reverse proxy serves the panel on 443, so links carry no port:\n%s", got)
	}
	if got := apply(config.AuthConfig{AccessJWTAud: "aud"}); !strings.Contains(got, "\npanel-port=\n") {
		t.Fatalf("a Cloudflare Tunnel serves the panel on 443, so links carry no port:\n%s", got)
	}
	if err := syncProxyPanelPort(filepath.Join(t.TempDir(), "absent.properties"), 30443); err != nil {
		t.Fatalf("a proxy on another host must be skipped: %v", err)
	}
}

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
