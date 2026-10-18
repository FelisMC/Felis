package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"felis.lolicon.best/internal/cfsetup"
	"felis.lolicon.best/internal/config"

	"github.com/BurntSushi/toml"
)

const podSetupConfigPath = "/etc/felis/felis.pod.toml"
const cloudflaredFelisUnit = "/etc/systemd/system/cloudflared-felis.service"

// Use a dedicated nftables table to replace or remove the fence without touching unrelated rules.
const felisEdgeTable = "felis_edge"

func applyCloudflareEdge(ctx context.Context, result *cfsetup.Result, panelHost, adminHost, cloudflaredBin string) error {
	if result == nil || result.AccessAud == "" {
		return fmt.Errorf("edge result did not include an Access audience")
	}
	if adminHost == "" {
		return fmt.Errorf("admin hostname is required")
	}
	// cloudflared is the only way in once the NodePort is fenced, so the
	// visitor address it writes can key the sign-in rate limit.
	if err := writeConnectionConfig(panelHost, adminHost, result.AccessAud, "CF-Connecting-IP"); err != nil {
		return err
	}
	if err := applyFelisConfigSecret(ctx); err != nil {
		return err
	}
	if err := kubectl(ctx, "-n", "felis", "rollout", "restart", "deployment/felis-api"); err != nil {
		return err
	}
	if err := installCloudflaredService(ctx, cloudflaredBin, result.ConfigPath); err != nil {
		return err
	}
	if err := kubectl(ctx, "-n", "felis", "rollout", "status", "deployment/felis-api", "--timeout=180s"); err != nil {
		return err
	}
	// Fence the public NodePort only after the connector and origin are serving to prevent Access bypass.
	// Leave the port open if the tunnel cannot be verified; host recovery can remove the fence.
	nodePort := int32(setupPanelNodePort())
	if err := verifyConnectorServing(ctx, cloudflaredBin, result.TunnelID); err != nil {
		return fmt.Errorf("edge origin NOT fenced: tunnel connector not confirmed serving, so NodePort %d stays publicly reachable; verify the tunnel then re-run: %w", nodePort, err)
	}
	if err := fenceOriginNodePort(ctx, nodePort); err != nil {
		return fmt.Errorf("edge fence origin NodePort %d: %w", nodePort, err)
	}
	return nil
}

// Reverse-proxy mode clears the Access audience and uses the owner's local session.
func applyReverseProxy(ctx context.Context, panelHost, adminHost string) error {
	if adminHost == "" {
		return fmt.Errorf("admin hostname is required")
	}
	// Caddy, nginx and Traefik all append the peer they saw to X-Forwarded-For.
	if err := writeConnectionConfig(panelHost, adminHost, "", "X-Forwarded-For"); err != nil {
		return err
	}
	if err := applyFelisConfigSecret(ctx); err != nil {
		return err
	}
	if err := kubectl(ctx, "-n", "felis", "rollout", "restart", "deployment/felis-api"); err != nil {
		return err
	}
	return kubectl(ctx, "-n", "felis", "rollout", "status", "deployment/felis-api", "--timeout=180s")
}

// An empty audience clears the old Cloudflare configuration in both host and Pod files.
func writeConnectionConfig(panelHost, adminHost, aud, clientIPHeader string) error {
	for _, path := range []string{hostSetupConfigPath, podSetupConfigPath} {
		if err := updateAuthConfig(path, panelHost, adminHost, aud, clientIPHeader); err != nil {
			return err
		}
	}
	return nil
}

func updateAuthConfig(path, panelHost, adminHost, aud, clientIPHeader string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if panelHost != "" {
		cfg.Auth.PanelHostname = panelHost
	}
	cfg.Auth.AdminHostname = adminHost
	cfg.Auth.AccessJWTAud = aud
	cfg.Auth.ClientIPHeader = clientIPHeader
	return writeConfig(path, cfg)
}

func writeConfig(path string, cfg *config.Config) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".felis-*.toml")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := toml.NewEncoder(tmp).Encode(cfg); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// Refresh workload configuration replicas after updating the control-plane Secret.
func applyFelisConfigSecret(ctx context.Context) error {
	out, err := kubectlOutput(ctx,
		"-n", "felis", "create", "secret", "generic", "felis-config",
		"--from-file=felis.toml="+podSetupConfigPath,
		"--dry-run=client", "-o", "yaml",
	)
	if err != nil {
		return err
	}
	if err := kubectlWithInput(ctx, out, "apply", "-f", "-"); err != nil {
		return err
	}
	if err := replicateFelisConfigToWorkloadNamespace(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "felis setup: warning: the control-plane config is applied, but the workload-namespace mirror could not be refreshed (%v); re-run felis setup once that is fixed\n", err)
	}
	return nil
}

// Replace the workload Secret rather than retaining stale settings; skip identical or unset namespaces.
func replicateFelisConfigToWorkloadNamespace(ctx context.Context) error {
	cfg, err := config.Load(hostSetupConfigPath)
	if err != nil {
		return err
	}
	ns := cfg.K8s.Namespace
	if ns == "" || ns == "felis" {
		return nil
	}
	manifest, err := kubectlOutput(ctx,
		"-n", ns, "create", "secret", "generic", "felis-config",
		"--from-file=felis.toml="+podSetupConfigPath,
		"--dry-run=client", "-o", "yaml",
	)
	if err != nil {
		return fmt.Errorf("render felis-config for %s: %w", ns, err)
	}
	if err := kubectlWithInput(ctx, manifest, "-n", ns, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("replicate felis-config to %s: %w", ns, err)
	}
	return nil
}

func installCloudflaredService(ctx context.Context, cloudflaredBin, configPath string) error {
	if cloudflaredBin == "" {
		return fmt.Errorf("cloudflared binary path is empty")
	}
	if configPath == "" {
		return fmt.Errorf("cloudflared config path is empty")
	}
	unit := fmt.Sprintf(`[Unit]
Description=Felis Cloudflare Tunnel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --config %s tunnel run
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
`, cloudflaredBin, configPath)
	if err := os.WriteFile(cloudflaredFelisUnit, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return systemctl(ctx, "enable", "--now", "cloudflared-felis.service")
}

func kubectl(ctx context.Context, args ...string) error {
	_, err := kubectlOutput(ctx, args...)
	return err
}

func kubectlWithInput(ctx context.Context, input []byte, args ...string) error {
	_, err := runK3sKubectl(ctx, input, args...)
	return err
}

func kubectlOutput(ctx context.Context, args ...string) ([]byte, error) {
	return runK3sKubectl(ctx, nil, args...)
}

func runK3sKubectl(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	fullArgs := append([]string{"kubectl"}, args...)
	cmd := exec.CommandContext(ctx, "k3s", fullArgs...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+hostBootstrapKubeconfigPath)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("k3s %v: %w: %s", fullArgs, err, string(out))
	}
	return out, nil
}

func systemctl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %w: %s", args, err, string(out))
	}
	return nil
}

// Filter the loopback-only NodePort before kube-proxy DNAT at priority -300.
func originFenceRuleset(nodePort int32) string {
	return fmt.Sprintf(`table inet %s {
	chain prerouting {
		type filter hook prerouting priority -300; policy accept;
		iif "lo" accept
		tcp dport %d drop
	}
}
`, felisEdgeTable, nodePort)
}

// Requires nftables; a firewalld reload can flush the standalone table.
func fenceOriginNodePort(ctx context.Context, nodePort int32) error {
	if nodePort <= 0 {
		return fmt.Errorf("fence origin: invalid node port %d", nodePort)
	}
	if _, err := exec.LookPath("nft"); err != nil {
		return fmt.Errorf("fence origin: `nft` not found — cannot close public access to NodePort %d; install nftables or restrict the port manually: %w", nodePort, err)
	}
	// Idempotent pre-clean: drop any stale felis_edge table (no-op on first run).
	_ = unfenceOriginNodePort(ctx)
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(originFenceRuleset(nodePort))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fence origin: nft -f -: %w: %s", err, string(out))
	}
	return nil
}

// unfenceOriginNodePort removes the fence, reopening the NodePort on all interfaces. It is the on-host break-glass if the tunnel dies, and it is idempotent. Integration-only.
func unfenceOriginNodePort(ctx context.Context) error {
	if _, err := exec.LookPath("nft"); err != nil {
		return fmt.Errorf("unfence origin: `nft` not found: %w", err)
	}
	cmd := exec.CommandContext(ctx, "nft", "delete", "table", "inet", felisEdgeTable)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.ToLower(string(out))
		// An absent table is the already-open state, not a failure.
		if strings.Contains(msg, "no such file") || strings.Contains(msg, "does not exist") {
			return nil
		}
		return fmt.Errorf("unfence origin: nft delete table inet %s: %w: %s", felisEdgeTable, err, string(out))
	}
	return nil
}

// Verify active edge connections before fencing the direct NodePort.
// Zero connections is retried, then surfaced, so a real failure is loud rather than silently skipping the fence.
func verifyConnectorServing(ctx context.Context, cloudflaredBin, tunnelID string) error {
	if strings.TrimSpace(tunnelID) == "" {
		return fmt.Errorf("tunnel id is required to verify the connector")
	}
	if cloudflaredBin == "" {
		cloudflaredBin = "cloudflared"
	}
	var lastErr error
	for attempt := range 10 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		cmd := exec.CommandContext(ctx, cloudflaredBin, "tunnel", "info", "--output", "json", tunnelID)
		out, err := cmd.CombinedOutput()
		if err != nil {
			lastErr = fmt.Errorf("cloudflared tunnel info: %w: %s", err, string(out))
			continue
		}
		if connectorConnCount(out) > 0 {
			return nil
		}
		lastErr = fmt.Errorf("tunnel %s reports no active connector connections yet", tunnelID)
	}
	return lastErr
}

// Accept both top-level conns and per-connector arrays; invalid JSON counts as zero connections.
func connectorConnCount(jsonOut []byte) int {
	var info struct {
		Conns      []json.RawMessage `json:"conns"`
		Connectors []struct {
			Conns []json.RawMessage `json:"conns"`
		} `json:"connectors"`
	}
	if err := json.Unmarshal(jsonOut, &info); err != nil {
		return 0
	}
	n := len(info.Conns)
	for _, c := range info.Connectors {
		n += len(c.Conns)
	}
	return n
}
