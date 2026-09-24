package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/platform"
)

// cmdConverge is the explicit convergence pass over already-installed system
// servers (#1). Provisioning is create-if-absent, so a field the desired spec
// gained after an install (spec.rcon, spec.startup.healthHTTPPort, a derived env
// key) never reaches the existing CR — and nothing says so. This command fills
// exactly those zero-value fields; see convergeSystemServers for the full contract
// and why it is a separate, operator-timed step rather than part of setup.
//
// It reads the same host config as setup (the control plane's felis.toml) and
// talks to the cluster with the local kubeconfig, so it must run as root on the
// control-plane host.
func cmdConverge(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("converge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultSetupConfigPath, "path to felis.toml")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis converge: refused — converging needs the cluster credentials, so it must run as root (try: sudo felis converge)")
		return 1
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis converge: %v\n", err)
		fmt.Fprintln(stderr, "If this host was never installed, run `sudo felis setup` first.")
		return 1
	}
	cl, err := buildSystemServerClient()
	if err != nil {
		fmt.Fprintf(stderr, "felis converge: %v\n", err)
		return 1
	}

	controlNS := platform.DefaultControlNamespace
	outcomes := convergeSystemServers(context.Background(), cl, cfg.K8s.Namespace,
		cfg.Velocity.LoginImage, cfg.Velocity.LobbyImage,
		platform.InternalAPIBaseURL(controlNS), cfg.Server.RootDomain,
		defaultPanelHostname(cfg.Server.RootDomain, cfg.Auth.PanelHostname))

	fmt.Fprintln(stdout, "felis converge: filling fields an installed system server predates (operator-set values are never overwritten):")
	exit := 0
	for _, o := range outcomes {
		switch {
		case o.err != nil:
			fmt.Fprintf(stdout, "  - %s: ERROR %v\n", o.name, o.err)
			exit = 1
		case len(o.changes) > 0:
			fmt.Fprintf(stdout, "  - %s: updated (%s)\n", o.name, strings.Join(o.changes, ", "))
		default:
			fmt.Fprintf(stdout, "  - %s: %s\n", o.name, o.skipped)
		}
	}
	return exit
}
