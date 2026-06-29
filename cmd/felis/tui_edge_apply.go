package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"felis.lolicon.best/internal/cfsetup"
	"felis.lolicon.best/internal/config"

	"github.com/BurntSushi/toml"
)

const podSetupConfigPath = "/etc/felis/felis.pod.toml"
const cloudflaredFelisUnit = "/etc/systemd/system/cloudflared-felis.service"

func applyCloudflareEdge(ctx context.Context, result *cfsetup.Result, panelHost, adminHost, cloudflaredBin string) error {
	if result == nil || result.AccessAud == "" {
		return fmt.Errorf("edge result did not include an Access audience")
	}
	if adminHost == "" {
		return fmt.Errorf("admin hostname is required")
	}
	if err := writeConnectionConfig(panelHost, adminHost, result.AccessAud); err != nil {
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
	return kubectl(ctx, "-n", "felis", "rollout", "status", "deployment/felis-api", "--timeout=180s")
}

// applyReverseProxy records the operator's chosen public hostnames and rolls the
// API so the panel serves them. No Access audience is set: the admin console is
// gated by the Owner's local-password session, and the operator's own reverse
// proxy (Caddy/nginx/Traefik/…) terminates TLS in front of the NodePort origin.
func applyReverseProxy(ctx context.Context, panelHost, adminHost string) error {
	if adminHost == "" {
		return fmt.Errorf("admin hostname is required")
	}
	if err := writeConnectionConfig(panelHost, adminHost, ""); err != nil {
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
func writeConnectionConfig(panelHost, adminHost, aud string) error {
	for _, path := range []string{hostSetupConfigPath, podSetupConfigPath} {
		if err := updateAuthConfig(path, panelHost, adminHost, aud); err != nil {
			return err
		}
	}
	return nil
}

func updateAuthConfig(path, panelHost, adminHost, aud string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if panelHost != "" {
		cfg.Auth.PanelHostname = panelHost
	}
	cfg.Auth.AdminHostname = adminHost
	cfg.Auth.AccessJWTAud = aud
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

func applyFelisConfigSecret(ctx context.Context) error {
	out, err := kubectlOutput(ctx,
		"-n", "felis", "create", "secret", "generic", "felis-config",
		"--from-file=felis.toml="+podSetupConfigPath,
		"--dry-run=client", "-o", "yaml",
	)
	if err != nil {
		return err
	}
	return kubectlWithInput(ctx, out, "apply", "-f", "-")
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
