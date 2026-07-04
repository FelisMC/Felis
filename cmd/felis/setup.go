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
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/store"
)

const defaultSetupConfigPath = "/etc/felis/felis.toml"
const hostSetupConfigPath = "/etc/felis/felis.host.toml"
const hostBootstrapDonePath = "/etc/felis/bootstrap.done"
const hostBootstrapBinPath = "/usr/local/bin/felis"
const hostBootstrapKubeconfigPath = "/etc/rancher/k3s/k3s.yaml"

var errHostBootstrapCancelled = errors.New("host bootstrap cancelled")

// channelName maps the --dev flag to the release channel deploy/bootstrap.sh
// understands. Release is the default so a bare `felis setup` is production.
func channelName(dev bool) string {
	if dev {
		return "dev"
	}
	return "release"
}

// cmdSetup is the normal first-run operator console. It is intentionally separate
// from breakGlass: setup creates the initial Owner and optional web edge; breakGlass
// is reserved for emergency local recovery/reset.
func cmdSetup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultSetupConfigPath, "path to felis.toml")
	dev := fs.Bool("dev", false, "install the dev channel (felis:dev, main HEAD) instead of the default release channel (felis:release, newest tag)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	// The channel governs which image tag/source ref the host bootstrap builds.
	// runBootstrap forwards the whole environment, so exporting it here is enough
	// to reach deploy/bootstrap.sh without threading a parameter through the TUI.
	if err := os.Setenv("FELIS_CHANNEL", channelName(*dev)); err != nil {
		fmt.Fprintf(stderr, "felis setup: %v\n", err)
		return 1
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
		fmt.Fprintf(stdout, "\nfelis setup: Owner account %q provisioned (passwordless).\n", res.username)
		fmt.Fprintf(stdout, "Recorded as %q (mode: %s, os user: %s).\n", res.accountable, res.mode, res.osUser)
		if res.setupTokenURL != "" {
			fmt.Fprintf(stdout, "Open this URL to complete passwordless login setup (verify email / enroll passkey):\n\n    %s\n\n", res.setupTokenURL)
		}
		if res.auditWarning != "" {
			fmt.Fprintf(stdout, "WARNING: the accountability audit row was NOT written: %s\n", res.auditWarning)
		}
		if panelURL != "" {
			fmt.Fprintf(stdout, "Admin console: %s\n", panelURL)
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

	// After a real setup pass (Owner provisioned and/or edge configured), make
	// sure the always-on login/lobby system services exist. This is idempotent
	// and best-effort — it never fails the setup that got this far.
	if res.provisioned || res.connectConfigured {
		provisionSystemServers(ctx, setup.cfg, stdout)
	}
	return 0
}

// provisionSystemServers ensures the login limbo and lobby system services exist
// after setup, then prints the off-cluster Velocity wiring the operator must
// apply by hand (Felis never writes the off-cluster proxy config). It is
// best-effort: unconfigured images or an unreachable cluster degrade to guidance
// rather than failing setup.
func provisionSystemServers(ctx context.Context, cfg *config.Config, out io.Writer) {
	if cfg.Velocity.LoginImage == "" && cfg.Velocity.LobbyImage == "" {
		fmt.Fprintln(out, "\nfelis setup: login/lobby system servers NOT provisioned — set [velocity] login_image "+
			"and lobby_image in felis.toml (build them from deploy/limbo and deploy/lobby), then re-run `sudo felis setup`.")
		return
	}
	cl, err := buildSystemServerClient()
	if err != nil {
		fmt.Fprintf(out, "\nfelis setup: could not reach the cluster to provision the login/lobby system servers: %v\n"+
			"Re-run `sudo felis setup` on the control-plane host once the cluster is reachable.\n", err)
		return
	}
	// The login limbo authenticates to the felis-api INTERNAL face, so it needs the
	// internal base URL, the root domain (to link players at the console), and the
	// service token. The first two are plain env baked into the pod here; the token
	// is a Secret the operator injects by reference — but a secretKeyRef is
	// namespace-local, so first replicate the token Secret from the control namespace
	// into the minecraft namespace where the login pod runs. The control namespace is
	// the platform default (there is no felis.toml override for it); a deployment that
	// renamed it must replicate the Secret by hand.
	controlNS := platform.DefaultControlNamespace
	apiBaseURL := platform.InternalAPIBaseURL(controlNS)
	tokenOutcome := ensureServiceTokenReplica(ctx, cl, controlNS, cfg.K8s.Namespace)
	outcomes := ensureSystemServers(ctx, cl, cfg.K8s.Namespace, cfg.Velocity.LoginImage, cfg.Velocity.LobbyImage, apiBaseURL, cfg.Server.RootDomain)
	outcomes = append([]systemServerOutcome{tokenOutcome}, outcomes...)
	fmt.Fprintln(out, "\nfelis setup: login/lobby system servers (always-on, reaper-exempt):")
	for _, o := range outcomes {
		switch {
		case o.err != nil:
			fmt.Fprintf(out, "  - %s: ERROR %v\n", o.name, o.err)
		case o.created:
			fmt.Fprintf(out, "  - %s: created (DesiredState=Running)\n", o.name)
		default:
			fmt.Fprintf(out, "  - %s: skipped (%s)\n", o.name, o.skipped)
		}
	}
	printVelocityWiringGuidance(out, cfg.Server.RootDomain)
}

// printVelocityWiringGuidance emits the manual off-cluster Velocity config that
// enforces the login-first topology. Felis auto-registers login/lobby as dynamic
// backends via /api/v1/servers, but the proxy's DEFAULT landing and waiting-park
// target live in velocity.toml on the off-cluster Java host, which Felis never
// writes. The one invariant: the default landing and the initial wait-park are
// BOTH the login gate — never the lobby — so no connection reaches the lobby (or
// any backend) without passing authentication first. The Paper lobby is reached
// only when the login gate transfers an authenticated player onward.
func printVelocityWiringGuidance(out io.Writer, rootDomain string) {
	fmt.Fprintln(out, "\nfelis setup: finish the login topology on the off-cluster Velocity host (velocity.toml):")
	fmt.Fprintln(out, "  1. Set the DEFAULT landing server to \"login\" so every fresh connection hits the")
	fmt.Fprintln(out, "     auth gate first (try = [\"login\"] under [servers], and the default forced-host).")
	fmt.Fprintln(out, "  2. Point the waiting-park target at the gate, NOT the lobby:")
	fmt.Fprintln(out, "     set FELIS_LOBBY_SERVER=login (or lobby-server=login). The limbo holds waiters")
	fmt.Fprintln(out, "     while their backend wakes, and a player is never parked past authentication.")
	fmt.Fprintln(out, "  3. Leave the Paper \"lobby\" OUT of the default/fallback paths — it is reached only")
	fmt.Fprintln(out, "     when the login gate transfers an authenticated player onward.")
	fmt.Fprintln(out, "  Rationale: rather refuse a connection when login is down than route a player past")
	fmt.Fprintln(out, "  the gate. Felis already refuses to give any server a fallback of \"lobby\".")
	if rootDomain != "" {
		fmt.Fprintf(out, "  (login is the front door for %s; per-server subdomains fall back to login while waking.)\n", rootDomain)
	}
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
