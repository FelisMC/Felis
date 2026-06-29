package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/store"
)

const defaultSetupConfigPath = "/etc/felis/felis.toml"
const hostSetupConfigPath = "/etc/felis/felis.host.toml"
const hostBootstrapDonePath = "/etc/felis/bootstrap.done"
const hostBootstrapBinPath = "/usr/local/bin/felis"
const hostBootstrapKubeconfigPath = "/etc/rancher/k3s/k3s.yaml"

var errHostBootstrapCancelled = errors.New("host bootstrap cancelled")

// cmdSetup is the normal first-run operator console. It is intentionally separate
// from breakGlass: setup creates the initial Owner and optional web edge; breakGlass
// is reserved for emergency local recovery/reset.
func cmdSetup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultSetupConfigPath, "path to felis.toml")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	configFlagSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			configFlagSet = true
		}
	})

	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis setup: refused — the setup console must run as root (try: sudo felis setup)")
		return 1
	}

	ctx := context.Background()
	bootstrapped := false
	if shouldRunHostBootstrapBeforeConfig(configFlagSet) {
		if err := runHostBootstrapForSetup(ctx); err != nil {
			return reportHostBootstrapError(err, stdout, stderr)
		}
		bootstrapped = true
	}
	if err := repairDefaultSetupConfig(configFlagSet); err != nil {
		fmt.Fprintf(stderr, "felis setup: repair default config: %v\n", err)
		return 1
	}
	effectiveCfgPath := setupConfigPath(*cfgPath, configFlagSet)
	setup, err := openConfiguredSetup(ctx, effectiveCfgPath)
	if err != nil {
		if bootstrapped || !shouldRunHostBootstrap(effectiveCfgPath, configFlagSet, err) {
			fmt.Fprintf(stderr, "felis setup: %v\n", err)
			return 1
		}
		if err := runHostBootstrapForSetup(ctx); err != nil {
			return reportHostBootstrapError(err, stdout, stderr)
		}
		bootstrapped = true
		if err := repairDefaultSetupConfig(configFlagSet); err != nil {
			fmt.Fprintf(stderr, "felis setup: repair default config: %v\n", err)
			return 1
		}
		effectiveCfgPath = setupConfigPath(*cfgPath, configFlagSet)
		setup, err = openConfiguredSetup(ctx, effectiveCfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "felis setup: after bootstrap: %v\n", err)
			return 1
		}
	}
	defer setup.drv.Close()

	res, err := runSetupTUI(ctx, setup.repo, setup.cfg.Database.URL, setup.cfg.Server.RootDomain, setup.cfg.Auth.AdminHostname, setup.cfg.Auth.PanelHostname, setup.cfg.Auth.AccessJWTAud, accountableOSUser(), setup.adminExists)
	if err != nil {
		fmt.Fprintf(stderr, "felis setup: %v\n", err)
		return 1
	}
	panelURL := res.panelURL
	if panelURL == "" {
		panelURL = localPanelURL(setup.cfg.Server.RootDomain)
	}

	if !res.provisioned && !res.connectConfigured {
		if bootstrapped {
			fmt.Fprintln(stdout, "felis setup: host bootstrap completed; Owner/connection setup skipped.")
			if panelURL != "" {
				fmt.Fprintf(stdout, "Panel: %s\n", panelURL)
				fmt.Fprintln(stdout, "The local HTTPS certificate is self-signed; your browser may ask for confirmation on first visit.")
			}
			return 0
		}
		fmt.Fprintln(stdout, "felis setup: cancelled — no changes made.")
		if panelURL != "" {
			fmt.Fprintf(stdout, "Panel: %s\n", panelURL)
		}
		return 0
	}

	if res.provisioned {
		fmt.Fprintf(stdout, "\nfelis setup: Owner account %q provisioned; local-password login is ENABLED.\n", res.username)
		fmt.Fprintf(stdout, "Recorded as %q (mode: %s, os user: %s).\n", res.accountable, res.mode, res.osUser)
		if res.displayPassword != "" {
			fmt.Fprintf(stdout, "One-time password (you MUST change it on first login):\n\n    %s\n\n", res.displayPassword)
		} else {
			fmt.Fprintln(stdout, "Log in with the password you just entered (you MUST change it on first login).")
		}
		if res.auditWarning != "" {
			fmt.Fprintf(stdout, "WARNING: the accountability audit row was NOT written: %s\n", res.auditWarning)
		}
		if panelURL != "" {
			fmt.Fprintf(stdout, "Log in at %s with that username and password.\n", panelURL)
			fmt.Fprintln(stdout, "The local HTTPS certificate is self-signed; your browser may ask for confirmation on first visit.")
		}
	}

	if res.connectConfigured {
		switch res.connectMethod {
		case connectCloudflare:
			fmt.Fprintf(stdout, "\nfelis setup: Cloudflare Tunnel + Access configured.\n")
			if len(res.edgeRoutedHosts) > 0 {
				fmt.Fprintf(stdout, "Routed web hostnames: %s\n", strings.Join(res.edgeRoutedHosts, ", "))
			}
			if res.edgeConfigPath != "" {
				fmt.Fprintf(stdout, "Wrote tunnel config: %s\n", res.edgeConfigPath)
			}
			fmt.Fprintln(stdout, "Felis config, Kubernetes Secret, API rollout and cloudflared service were updated.")
		case connectReverseProxy:
			fmt.Fprintf(stdout, "\nfelis setup: reverse-proxy front configured. Point your proxy at the origin:\n\n")
			fmt.Fprintln(stdout, res.reverseProxyGuide)
			fmt.Fprintln(stdout, "Felis config, Kubernetes Secret and API rollout were updated.")
		}
	}
	return 0
}

type configuredSetup struct {
	cfg         *config.Config
	drv         *store.PostgresDriver
	repo        *api.PGRepo
	adminExists bool
}

type setupOpenError struct {
	stage string
	err   error
}

func (e *setupOpenError) Error() string {
	return e.stage + ": " + e.err.Error()
}

func (e *setupOpenError) Unwrap() error {
	return e.err
}

func openConfiguredSetup(ctx context.Context, cfgPath string) (*configuredSetup, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, &setupOpenError{stage: "load config", err: err}
	}
	drv, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		return nil, &setupOpenError{stage: "open database", err: err}
	}
	repo := api.NewPGRepo(drv.DB())
	adminExists, err := repo.AdminExists(ctx)
	if err != nil {
		drv.Close()
		return nil, &setupOpenError{stage: "detect existing admin", err: err}
	}
	return &configuredSetup{cfg: cfg, drv: drv, repo: repo, adminExists: adminExists}, nil
}

func setupConfigPath(requested string, configFlagSet bool) string {
	return setupConfigPathFor(requested, hostSetupConfigPath, configFlagSet)
}

func setupConfigPathFor(requested, host string, configFlagSet bool) string {
	if configFlagSet {
		return requested
	}
	if _, err := os.Stat(host); err == nil {
		return host
	}
	return requested
}

func repairDefaultSetupConfig(configFlagSet bool) error {
	if configFlagSet {
		return nil
	}
	if _, err := os.Stat(hostSetupConfigPath); err != nil {
		return nil
	}
	return ensureDefaultConfigLink(defaultSetupConfigPath, hostSetupConfigPath)
}

func ensureDefaultConfigLink(target, host string) error {
	if link, err := os.Readlink(target); err == nil && link == host {
		return nil
	}
	if _, err := os.Lstat(target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.Symlink(host, target)
		}
		return err
	}
	backup := fmt.Sprintf("%s.bak.%s.%d", target, time.Now().UTC().Format("20060102150405"), os.Getpid())
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	return os.Symlink(host, target)
}

func shouldRunHostBootstrap(cfgPath string, configFlagSet bool, err error) bool {
	if configFlagSet {
		return false
	}
	var setupErr *setupOpenError
	if !errors.As(err, &setupErr) {
		return false
	}
	if setupErr.stage == "open database" {
		return true
	}
	if setupErr.stage != "load config" {
		return false
	}
	_, statErr := os.Stat(cfgPath)
	return errors.Is(statErr, os.ErrNotExist)
}

func shouldRunHostBootstrapBeforeConfig(configFlagSet bool) bool {
	if configFlagSet {
		return false
	}
	return !hostBootstrapReady(hostBootstrapDonePath, hostSetupConfigPath, hostBootstrapBinPath, hostBootstrapKubeconfigPath)
}

func hostBootstrapReady(marker, hostConfig, hostBin, kubeconfig string) bool {
	return fileExists(marker) && fileExists(hostConfig) && executableExists(hostBin) && fileExists(kubeconfig)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func executableExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func runHostBootstrapForSetup(ctx context.Context) error {
	completed, err := runHostBootstrapTUI(ctx)
	if err != nil {
		return err
	}
	if !completed {
		return errHostBootstrapCancelled
	}
	return nil
}

func reportHostBootstrapError(err error, stdout, stderr io.Writer) int {
	if errors.Is(err, errHostBootstrapCancelled) {
		fmt.Fprintln(stdout, "felis setup: cancelled — bootstrap not run.")
		return 0
	}
	fmt.Fprintf(stderr, "felis setup: bootstrap: %v\n", err)
	return 1
}
