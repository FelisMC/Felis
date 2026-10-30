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
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/platform"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Explicit root-only convergence fills missing defaults while preserving customization.
func cmdConverge(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("converge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultSetupConfigPath, "path to felis.toml")
	userRcon := fs.Bool("user-rcon", false, "also turn RCON on for user servers created before it was the default")
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
	buildNS := cfg.Registry.BuildNamespace
	if buildNS == "" {
		buildNS = platform.DefaultBuildNamespace
	}
	// A system server this pass creates mounts the Secrets setup replicates into the
	// game namespace; only the replicas that changed or are unusable are reported.
	var outcomes []systemServerOutcome
	for _, o := range provisionSecretReplicas(context.Background(), cl, controlNS, cfg.K8s.Namespace, buildNS) {
		if o.created || o.updated || o.err != nil || !o.available {
			outcomes = append(outcomes, o)
		}
	}
	outcomes = append(outcomes, convergeSystemServers(context.Background(), cl, cfg.K8s.Namespace,
		systemImagesFrom(cfg.Velocity),
		platform.InternalAPIBaseURL(controlNS), cfg.Server.RootDomain,
		defaultPanelHostname(cfg.Server.RootDomain, cfg.Auth.PanelHostname))...)

	outcomes = append(outcomes, convergeUserServerIdle(context.Background(), cl, cfg.K8s.Namespace)...)
	outcomes = append(outcomes, convergeUserServerRcon(context.Background(), cl, cfg.K8s.Namespace, *userRcon)...)

	fmt.Fprintln(stdout, "felis converge: filling fields an installed server predates (operator-set values are never overwritten):")
	exit := 0
	for _, o := range outcomes {
		switch {
		case o.err != nil:
			fmt.Fprintf(stdout, "  - %s: ERROR %v\n", o.name, o.err)
			exit = 1
		case o.created:
			fmt.Fprintf(stdout, "  - %s: created (it was missing)\n", o.name)
		case o.updated:
			fmt.Fprintf(stdout, "  - %s: refreshed from the control namespace\n", o.name)
		case len(o.changes) > 0:
			fmt.Fprintf(stdout, "  - %s: updated (%s)\n", o.name, strings.Join(o.changes, ", "))
		default:
			fmt.Fprintf(stdout, "  - %s: %s\n", o.name, o.skipped)
		}
	}
	return exit
}

// Apply idle defaults only to entirely unset user policies; preserve explicit disablement.
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
		// Re-checked on the copy each attempt reads: an idle setting the panel saved
		// meanwhile is the user's, and the default must not land over it.
		changed, err := patchOnConflictRetry(ctx, cl, ms, func() bool {
			if ms.Spec.Idle != (v1alpha1.IdleSpec{}) {
				return false
			}
			ms.Spec.Idle = v1alpha1.DefaultIdle()
			return true
		})
		if err != nil {
			out = append(out, systemServerOutcome{name: ms.Name, err: fmt.Errorf("converge %s: %w", ms.Name, err)})
			continue
		}
		if !changed {
			continue
		}
		out = append(out, systemServerOutcome{name: ms.Name, available: true, updated: true,
			changes: []string{fmt.Sprintf("spec.idle (stop after %ds empty)", v1alpha1.DefaultEmptySecondsBeforeStop)}})
	}
	return out
}

// RCON fill is opt-in because custom images may not expose the expected listener.
func convergeUserServerRcon(ctx context.Context, cl client.Client, namespace string, fill bool) []systemServerOutcome {
	var list v1alpha1.MinecraftServerList
	if err := cl.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return []systemServerOutcome{{name: "user servers", err: fmt.Errorf("list servers: %w", err)}}
	}
	var out []systemServerOutcome
	for i := range list.Items {
		ms := &list.Items[i]
		if ms.Labels[v1alpha1.LabelSystemRole] != "" || ms.Spec.Rcon != (v1alpha1.RconSpec{}) {
			continue
		}
		if !fill {
			out = append(out, systemServerOutcome{name: ms.Name, available: true,
				skipped: "no RCON (console, online count and idle stop are off); once its image serves RCON, sudo felis converge -user-rcon turns it on"})
			continue
		}
		changed, err := patchOnConflictRetry(ctx, cl, ms, func() bool {
			if ms.Spec.Rcon != (v1alpha1.RconSpec{}) {
				return false
			}
			ms.Spec.Rcon = v1alpha1.RconSpec{
				Enabled:   true,
				SecretRef: v1alpha1.SecretKeyRef{Name: naming.RconSecretName(ms.Name), Key: naming.RconSecretKey},
			}
			return true
		})
		if err != nil {
			out = append(out, systemServerOutcome{name: ms.Name, err: fmt.Errorf("converge %s: %w", ms.Name, err)})
			continue
		}
		if changed {
			out = append(out, systemServerOutcome{name: ms.Name, available: true, updated: true, changes: []string{"spec.rcon"}})
		}
	}
	return out
}
