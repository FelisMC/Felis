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
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/platform"
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
	dev := fs.Bool("dev", false, "rejected: the install channel is chosen by the bootstrap installer, not by setup")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	// setup cannot honour a channel, so it refuses rather than silently installing the
	// other one. It used to export FELIS_CHANNEL here, which nothing has ever read --
	// deploy/bootstrap.sh reads FELIS_VERSION_BOOTSTRAP -- so --dev was a silent no-op
	// that installed release. Renaming the variable would not fix it: on this path
	// bootstrap takes the bootstrap_from_tui arm, which re-images the host from the
	// binary setup is already running, and every reader of FELIS_VERSION_BOOTSTRAP
	// (use_release_binary, resolve_install_ref) is unreachable from there. Choosing a
	// channel means re-running the installer, which is what this points the operator at.
	if *dev {
		fmt.Fprintln(stderr, "felis setup: --dev is not supported here; setup re-images this host from the felis binary it is already running.")
		fmt.Fprintln(stderr, "To install a different channel, re-run the bootstrap installer with FELIS_VERSION_BOOTSTRAP=dev (see CONTRIBUTING.md).")
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

	// The wizard's first screen asks the operator to join the server and run /link:
	// the Owner IS the Minecraft account, so the login gate must be UP before we ask
	// for a link code. This used to run after the wizard, which is why setup asked
	// for a code from a server that had never been started. On a re-run the Owner
	// already exists, so provisioning stays best-effort and never blocks the
	// operator from reaching the status screen.
	if err := provisionSystemServers(ctx, setup.cfg, stdout, !setup.adminExists); err != nil {
		fmt.Fprintf(stderr, "felis setup: %v\n", err)
		fmt.Fprintln(stderr, "The Owner is bound by joining the login gate in-game, so setup cannot continue without it.")
		return 1
	}

	res, err := runSetupTUI(ctx, setup.repo, setup.cfg.Database.URL, setup.cfg.Server.RootDomain, setup.cfg.Auth.AdminHostname, setup.cfg.Auth.PanelHostname, setup.cfg.Auth.AccessJWTAud, setup.cfg.K8s.Namespace, accountableOSUser(), setup.adminExists)
	if err != nil {
		fmt.Fprintf(stderr, "felis setup: %v\n", err)
		return 1
	}
	panelURL := res.panelURL
	if panelURL == "" {
		panelURL = localPanelURL(setup.cfg.Server.RootDomain, setup.cfg.Auth.AdminHostname)
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
		// A re-run lands on the status screen, which changes nothing by design —
		// reporting that as "cancelled" reads as a failure the operator did not cause.
		msg := "felis setup: cancelled — no changes made."
		if res.alreadySetUp {
			msg = "felis setup: already set up — nothing to change."
		}
		fmt.Fprintln(stdout, msg)
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

	return 0
}

// provisionSystemServers ensures the login limbo and lobby system services exist,
// then prints the login-first Velocity wiring. deploy/bootstrap.sh writes this
// configuration for its host proxy; operators only need to mirror it when they
// deliberately run Velocity elsewhere.
//
// required is set on a first run, where the next screen asks the operator to join
// the server and run /link. There a gate that never comes up is not a degraded
// install, it is an impossible one — so every soft landing below becomes a hard
// error and we block until the gate reports Ready. On a re-run the Owner already
// exists and nothing downstream needs the gate, so unconfigured images or an
// unreachable cluster degrade to printed guidance exactly as before.
func provisionSystemServers(ctx context.Context, cfg *config.Config, out io.Writer, required bool) error {
	// fail is the one place the two modes diverge: fatal on a first run, guidance
	// on a re-run.
	fail := func(format string, args ...any) error {
		if required {
			return fmt.Errorf(format, args...)
		}
		fmt.Fprintf(out, "\nfelis setup: "+format+"\n", args...)
		return nil
	}
	if cfg.Velocity.LoginImage == "" && cfg.Velocity.LobbyImage == "" {
		return fail("login/lobby system servers NOT provisioned — set [velocity] login_image " +
			"and lobby_image in felis.toml (build them from deploy/limbo and deploy/lobby), then re-run `sudo felis setup`")
	}
	if required && cfg.Velocity.LoginImage == "" {
		return errors.New("the Owner binds by joining the login gate, but [velocity] login_image is not set in felis.toml " +
			"(build it from deploy/limbo), then re-run `sudo felis setup`")
	}
	cl, err := buildSystemServerClient()
	if err != nil {
		return fail("could not reach the cluster to provision the login/lobby system servers: %v\n"+
			"Re-run `sudo felis setup` on the control-plane host once the cluster is reachable", err)
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
	// These Secrets must land in the minecraft namespace before the pods that
	// mount them are created: the service token (login authenticates to felis-api
	// with it), the Velocity forwarding secret (every backend verifies the proxy's
	// signed handshake with it — without it the login gate would derive an OFFLINE
	// UUID and the Owner would bind the wrong Minecraft identity), and felis-config
	// (the on-demand BACKUP Job runs in the minecraft namespace and mounts it to
	// self-record its world_backups row; without the replica the Job's volume
	// mount fails and every backup request strands in the cluster).
	// An empty build_namespace means the build system's compiled-in default; the
	// replica must target the namespace the Jobs actually run in.
	buildNS := cfg.Registry.BuildNamespace
	if buildNS == "" {
		buildNS = platform.DefaultBuildNamespace
	}
	secretOutcomes := []systemServerOutcome{
		ensureSecretReplica(ctx, cl, controlNS, cfg.K8s.Namespace,
			naming.ServiceTokenSecretName, naming.ServiceTokenSecretKey, "service-token", "minecraft ns", false),
		ensureSecretReplica(ctx, cl, controlNS, cfg.K8s.Namespace,
			naming.ForwardingSecretName, naming.ForwardingSecretKey, "forwarding-secret", "minecraft ns", false),
		// refresh=true: felis-config is the rendered config, not a credential. The
		// backup/restore/fileedit Jobs and the reaper mount this copy, so a re-run
		// must update it when the control plane's render has moved on (a stale copy
		// e.g. keeps an old database URL after a credential rotation).
		ensureSecretReplica(ctx, cl, controlNS, cfg.K8s.Namespace,
			"felis-config", "felis.toml", "config", "minecraft ns", true),
		// The reaper's pre-reap warning emails authenticate with the same relay
		// password felis-api uses; the reaper pod runs in the minecraft namespace,
		// where a secretKeyRef resolves only against a local mirror. Skipped while
		// the relay is not configured yet — the "configure email" screen refreshes
		// both mirrors when it applies.
		ensureSecretReplica(ctx, cl, controlNS, cfg.K8s.Namespace,
			"felis-smtp", "password", "smtp", "minecraft ns", false),
		// The build namespace needs the same token: the build Job's fetch
		// initContainer reads the submission context from the internal face. Best
		// effort — a deployment that only installs the control plane simply never
		// builds a user submission.
		ensureSecretReplica(ctx, cl, controlNS, buildNS,
			naming.ServiceTokenSecretName, naming.ServiceTokenSecretKey, "service-token", "felis-build ns", false),
	}
	outcomes := ensureSystemServers(ctx, cl, cfg.K8s.Namespace, cfg.Velocity.LoginImage, cfg.Velocity.LobbyImage, apiBaseURL, cfg.Server.RootDomain, defaultPanelHostname(cfg.Server.RootDomain, cfg.Auth.PanelHostname))
	outcomes = append(secretOutcomes, outcomes...)
	fmt.Fprintln(out, "\nfelis setup: login/lobby system servers (always-on, reaper-exempt):")
	for _, o := range outcomes {
		switch {
		case o.err != nil:
			fmt.Fprintf(out, "  - %s: ERROR %v\n", o.name, o.err)
		case o.created:
			fmt.Fprintf(out, "  - %s: created (DesiredState=Running)\n", o.name)
		case o.updated:
			fmt.Fprintf(out, "  - %s: refreshed from the control namespace\n", o.name)
		default:
			fmt.Fprintf(out, "  - %s: skipped (%s)\n", o.name, o.skipped)
		}
	}
	if required {
		if err := requiredProvisioningError(outcomes); err != nil {
			return fmt.Errorf("required Minecraft provisioning failed: %w", err)
		}
		fmt.Fprintln(out, "\nfelis setup: waiting for the login gate to accept players…")
		err := awaitLoginGateReady(ctx, cl, cfg.K8s.Namespace, loginGateReadyTimeout, loginGatePollInterval, func(p v1alpha1.Phase) {
			fmt.Fprintf(out, "  login: %s\n", phaseOrPending(p))
		})
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "  login: Ready")
	}
	printVelocityWiringGuidance(out, cfg.Server.RootDomain)
	return nil
}

// printVelocityWiringGuidance records the login-first topology bootstrap applies to
// its host proxy and an external proxy must mirror. Felis auto-registers login/lobby
// as dynamic backends via /api/v1/servers, while velocity.toml owns the static
// login-only fallback. The invariant is stateful: every fresh connection lands on
// login; only login may release a linked player to the lobby; and the proxy may then
// redirect that release to the originally requested backend or park it in the lobby
// while the backend wakes.
func printVelocityWiringGuidance(out io.Writer, rootDomain string) {
	fmt.Fprintln(out, "\nfelis setup: Velocity login topology (bootstrap configured the host proxy automatically):")
	fmt.Fprintln(out, "  If Velocity runs on another host, mirror these settings there:")
	fmt.Fprintln(out, "  1. Set the DEFAULT landing server to \"login\" so every fresh connection hits the")
	fmt.Fprintln(out, "     auth gate first (try = [\"login\"] under [servers], and the default forced-host).")
	fmt.Fprintln(out, "  2. Keep the gate and post-auth lobby distinct:")
	fmt.Fprintln(out, "     set FELIS_LOGIN_SERVER=login and FELIS_LOBBY_SERVER=lobby")
	fmt.Fprintln(out, "     (or login-server=login / lobby-server=lobby).")
	fmt.Fprintln(out, "  3. Leave the Paper \"lobby\" OUT of every default/fallback path. The proxy accepts")
	fmt.Fprintln(out, "     it only as login's authenticated release target, then restores the requested route.")
	fmt.Fprintln(out, "  Rationale: rather refuse a connection when login is down than route a player past")
	fmt.Fprintln(out, "  the gate. Felis already refuses to give any server a fallback of \"lobby\".")
	if rootDomain != "" {
		fmt.Fprintf(out, "  (login is the front door for %s; linked players wait in lobby while a target wakes.)\n", rootDomain)
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
	// Pending migrations are the preflight's to apply; a newer schema is a rolled-back
	// binary, and nothing this console writes would match it.
	drv, err := openStore(ctx, cfg.Database.URL, true)
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
