package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/platform"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// cmdConverge is the explicit convergence pass over already-installed system
// servers (#1), plus the idle-stop default for user servers that predate it. Provisioning is create-if-absent, so a field the desired spec
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

	outcomes = append(outcomes, convergeUserServerIdle(context.Background(), cl, cfg.K8s.Namespace)...)

	fmt.Fprintln(stdout, "felis converge: filling fields an installed server predates (operator-set values are never overwritten):")
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

// convergeUserServerIdle gives every user server that predates the idle default
// (spec.idle entirely unset) the default idle stop. A server whose idle stop was
// turned off keeps a duration on its spec, so it is not "unset" and is left
// alone; system servers never idle out and are skipped. Servers that already
// carry a value produce no line, so a converged fleet prints nothing here.
func convergeUserServerIdle(ctx context.Context, cl client.Client, namespace string) []systemServerOutcome {
	var list v1alpha1.MinecraftServerList
	if err := cl.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return []systemServerOutcome{{name: "user servers", err: fmt.Errorf("list servers: %w", err)}}
	}
	var out []systemServerOutcome
	for i := range list.Items {
		ms := &list.Items[i]
		if ms.Labels[v1alpha1.LabelSystemRole] != "" || ms.Spec.Idle != (v1alpha1.IdleSpec{}) {
			continue
		}
		patch := client.MergeFrom(ms.DeepCopy())
		ms.Spec.Idle = v1alpha1.DefaultIdle()
		if err := cl.Patch(ctx, ms, patch); err != nil {
			out = append(out, systemServerOutcome{name: ms.Name, err: fmt.Errorf("converge %s: %w", ms.Name, err)})
			continue
		}
		out = append(out, systemServerOutcome{name: ms.Name, available: true, updated: true,
			changes: []string{fmt.Sprintf("spec.idle (stop after %ds empty)", v1alpha1.DefaultEmptySecondsBeforeStop)}})
	}
	return out
}
