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

// felisEdgeTable is the dedicated nftables table Felis owns for edge hardening. A
// private table lets the whole fence be added and removed atomically without ever
// touching other rules on the host.
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
	// Final step: with the connector installed and the origin rolled out, close the
	// direct public path to the panel NodePort so the origin is reachable ONLY via
	// Cloudflare (the Zero-Trust edge the operator just built). Without this, anyone
	// who hits https://<node-ip>:<nodeport>/ with the right Host header bypasses
	// Cloudflare Access entirely.
	//
	// It is GATED on the connector actually serving: fencing a dead tunnel would sever
	// the only web path to a still-up origin. If we cannot confirm the tunnel is
	// serving we do NOT fence — leaving the port reachable (its pre-tunnel state) is
	// the fail-safe choice, and the failure is surfaced loudly so the operator knows
	// the port is still open and can re-run once the tunnel is healthy. On-host
	// break-glass (SSH + unfenceOriginNodePort / `nft delete table inet felis_edge`)
	// is the recovery path if the tunnel later dies.
	nodePort := int32(setupPanelNodePort())
	if err := verifyConnectorServing(ctx, cloudflaredBin, result.TunnelID); err != nil {
		return fmt.Errorf("edge origin NOT fenced: tunnel connector not confirmed serving, so NodePort %d stays publicly reachable; verify the tunnel then re-run: %w", nodePort, err)
	}
	if err := fenceOriginNodePort(ctx, nodePort); err != nil {
		return fmt.Errorf("edge fence origin NodePort %d: %w", nodePort, err)
	}
	return nil
}

// applyReverseProxy records the operator's chosen public hostnames and rolls the
// API so the panel serves them. No Access audience is set: the admin console is
// gated by the Owner's local session (passwordless sign-in), and the operator's own reverse
// proxy (Caddy/nginx/Traefik/…) terminates TLS in front of the NodePort origin.
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

// writeConnectionConfig stamps the chosen hostnames (and optional Access audience)
// into both the host and pod config files. An empty aud clears any prior
// Cloudflare audience, which is correct when switching to a non-Access front.
// clientIPHeader is the header that front writes the visitor address into.
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

// applyFelisConfigSecret applies the rendered config to the control namespace and
// then converges the workload-namespace mirror best-effort. The mirror feeds the
// backup/restore/fileedit Jobs and the reaper; without this refresh a reconfigure
// here would leave those readers on the previous render until the next `felis
// setup` run (startup pass) or installer re-run.
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

// replicateFelisConfigToWorkloadNamespace overwrites the workload-namespace
// felis-config mirror with the freshly rendered pod config. Deliberately a full
// replace, not create-if-absent: a stale mirror is exactly what silently hands
// the Jobs that mount it old settings after a reconfigure. No-op when the
// workload namespace is unset or is the control namespace itself.
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

// originFenceRuleset renders the nftables ruleset that fences the panel NodePort so
// the origin is reachable only over loopback — the hop the host-side cloudflared
// connector uses (it dials https://127.0.0.1:<nodePort>) — and never from a public
// interface.
//
// The chain hooks prerouting at priority -300 ("raw"), which runs BEFORE kube-proxy
// programs its NodePort DNAT (the dstnat hook at priority -100). That ordering is the
// whole trick: an external packet to <node-ip>:<nodePort> is seen here with its
// ORIGINAL destination port before DNAT rewrites it to a pod IP, so a plain
// filter/INPUT rule (which the DNAT'd, then-FORWARDed packet never traverses) would
// miss it, but this one catches it. Loopback is accepted first, so the connector's
// 127.0.0.1 origin hop — DNAT'd in the OUTPUT path, not prerouting — is never
// affected. The `inet` family covers both IPv4 and IPv6, closing a public v6 NodePort
// too. This function is pure so the security-relevant shape is unit-verifiable; the
// side-effecting apply lives in fenceOriginNodePort.
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

// fenceOriginNodePort installs the nftables fence (originFenceRuleset) so the panel
// NodePort is closed to the public interface while staying open on loopback for the
// tunnel connector. It is idempotent — any prior felis_edge table is removed before
// the fresh ruleset is loaded, so re-running the edge setup re-applies cleanly.
//
// INTEGRATION-ONLY: it mutates the host firewall via `nft`. KNOWN-LIMITATION: it
// targets nftables (the default on modern distros, incl. the bootstrap's Ubuntu/RPM
// targets). If the `nft` binary is absent it fails LOUD rather than silently leaving
// the port open — a false sense of security is worse than a clear error. On firewalld
// hosts the bootstrap opens this port in firewalld's zone and a firewalld reload can
// flush this standalone table; firewalld-native coordination is not yet handled and
// is tracked here honestly.
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

// unfenceOriginNodePort removes the fence, reopening the NodePort on all interfaces.
// It is the on-host break-glass recovery: if the tunnel dies, the operator SSHes in
// and reopens the direct panel origin. Idempotent — reopening an already-open port
// (no felis_edge table) succeeds. INTEGRATION-ONLY.
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

// verifyConnectorServing polls `cloudflared tunnel info` until the tunnel reports at
// least one active edge connection — proof the connector is really serving, not just
// a started-but-disconnected service — before the direct NodePort is fenced.
// INTEGRATION-ONLY: it shells out to the real cloudflared against the operator's
// account. It tolerates cloudflared's two known JSON shapes (see connectorConnCount);
// finding zero is treated as not-yet-connected and retried, then finally surfaced so
// a real failure is loud rather than silently skipping the fence.
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

// connectorConnCount counts the active connections in `cloudflared tunnel info
// --output json` output. cloudflared has used two shapes over its versions — a
// top-level "conns" array and a per-"connectors" one — so this counts both and is
// pure/unit-verifiable. A parse failure counts as zero (treated as not-yet-serving).
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
