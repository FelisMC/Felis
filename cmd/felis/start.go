package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Started before everything else.
var clusterServices = []string{"k3s.service", "k3s-agent.service"}

// Long-running units in dependency order.
var hostStartServices = []string{
	"iscsid.service",
	"keepalived.service",
	"felis-node-control.service",
	"felis-velocity.service",
	"felis-nano.service",
	"cloudflared-felis.service",
}

type startEnv struct {
	unitDir   string
	run       func(ctx context.Context, name string, args ...string) ([]byte, error)
	cluster   func() (client.Client, error)
	controlNS string
	poll      time.Duration
}

func cmdStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	unitDir := fs.String("systemd-dir", systemdUnitDir, "where the installer's systemd units are")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the cluster and the control plane")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis start: run as root (sudo felis start): it starts systemd units")
		return 1
	}
	w, _, err := watchdogUnitFlags(filepath.Join(*unitDir, "felis-watchdog.service"))
	if err != nil {
		fmt.Fprintf(stderr, "felis start: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	env := startEnv{unitDir: *unitDir, run: hostCommand, cluster: buildSystemServerClient, controlNS: w.controlNS, poll: 2 * time.Second}
	return runStart(ctx, env, stdout)
}

func runStart(ctx context.Context, env startEnv, out io.Writer) int {
	enabled := func(units []string) []string {
		var on []string
		for _, u := range units {
			if state, _ := env.run(ctx, "systemctl", "is-enabled", u); strings.TrimSpace(string(state)) == "enabled" {
				on = append(on, u)
			}
		}
		return on
	}
	var failed []string
	report := func(units []string) {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, u := range units {
			state := unitActiveState(ctx, doctorEnv{run: env.run}, u)
			if state != "active" {
				failed = append(failed, u)
			}
			fmt.Fprintf(tw, "  %s\t%s\n", u, state)
		}
		tw.Flush()
	}

	cluster := enabled(clusterServices)
	if len(cluster) > 0 {
		fmt.Fprintln(out, "cluster:")
		_, _ = env.run(ctx, "systemctl", "start", cluster[0])
		report(cluster[:1])
	}
	controller := len(cluster) > 0 && cluster[0] == "k3s.service" && len(failed) == 0
	var cl client.Client
	if controller {
		var err error
		if cl, err = waitForNode(ctx, env, out); err != nil {
			fmt.Fprintf(out, "  the Kubernetes API did not come up: %v\n", err)
			failed = append(failed, "kube-api")
			controller = false
		}
	}

	timers, _ := filepath.Glob(filepath.Join(env.unitDir, "felis-*.timer"))
	for i, t := range timers {
		timers[i] = filepath.Base(t)
	}
	sort.Strings(timers)
	units := append(enabled(hostStartServices), enabled(timers)...)
	if len(units) > 0 {
		fmt.Fprintln(out, "\nservices and timers:")
		_, _ = env.run(ctx, "systemctl", append([]string{"start"}, units...)...)
		report(units)
	}
	if len(cluster) == 0 && len(units) == 0 {
		fmt.Fprintln(out, "felis start: no Felis unit is set to start on this host; is Felis installed here?")
		return 1
	}

	if controller {
		fmt.Fprintf(out, "\ncontrol plane (namespace %s):\n", env.controlNS)
		failed = append(failed, waitForControlPlane(ctx, env, cl, out)...)
	}

	if len(failed) > 0 {
		fmt.Fprintf(out, "\n%d part(s) did not come up:\n", len(failed))
		for _, f := range failed {
			fmt.Fprintf(out, "  %s → %s\n", f, startHint(f, env.controlNS))
		}
		fmt.Fprintln(out, "sudo felis doctor checks the rest of the platform.")
		return 1
	}
	fmt.Fprintln(out, "\nFelis is up. sudo felis status shows the whole platform.")
	return 0
}

func startHint(part, ns string) string {
	switch {
	case part == "kube-api":
		return "systemctl status k3s; journalctl -u k3s -n 200 --no-pager"
	case part == "control-plane":
		return fmt.Sprintf("k3s kubectl -n %s get deployments,pods", ns)
	case strings.HasPrefix(part, "deployment/"):
		name := strings.TrimPrefix(part, "deployment/")
		return fmt.Sprintf("k3s kubectl -n %s describe deployment %s; k3s kubectl -n %s logs deployment/%s --tail=100", ns, name, ns, name)
	}
	return fmt.Sprintf("journalctl -u %s -n 100 --no-pager", part)
}

func waitForNode(ctx context.Context, env startEnv, out io.Writer) (client.Client, error) {
	began := time.Now()
	var last error
	for {
		cl, err := env.cluster()
		if err == nil {
			var nodes corev1.NodeList
			if err = cl.List(ctx, &nodes); err == nil {
				for _, n := range nodes.Items {
					if nodeReady(n) {
						fmt.Fprintf(out, "  node %s Ready after %s\n", n.Name, time.Since(began).Round(time.Second))
						return cl, nil
					}
				}
				err = errors.New("no node is Ready yet")
			}
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, last
		case <-time.After(env.poll):
		}
	}
}

func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// waitForControlPlane returns the Deployments that never became ready.
func waitForControlPlane(ctx context.Context, env startEnv, cl client.Client, out io.Writer) []string {
	var deps appsv1.DeploymentList
	for {
		err := cl.List(ctx, &deps, client.InNamespace(env.controlNS))
		if err == nil && len(deps.Items) > 0 && len(unready(deps.Items)) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			if err != nil {
				fmt.Fprintf(out, "  cannot list it: %v\n", err)
				return []string{"kube-api"}
			}
			printDeployments(out, deps.Items)
			if len(deps.Items) == 0 {
				fmt.Fprintln(out, "  no Deployment found")
				return []string{"control-plane"}
			}
			var failed []string
			for _, d := range unready(deps.Items) {
				failed = append(failed, "deployment/"+d)
			}
			return failed
		case <-time.After(env.poll):
		}
	}
	printDeployments(out, deps.Items)
	return nil
}

func unready(deps []appsv1.Deployment) []string {
	var out []string
	for _, d := range deps {
		if d.Status.ReadyReplicas < wantReplicas(d) {
			out = append(out, d.Name)
		}
	}
	return out
}

func wantReplicas(d appsv1.Deployment) int32 {
	if d.Spec.Replicas != nil {
		return *d.Spec.Replicas
	}
	return 1
}

func printDeployments(out io.Writer, deps []appsv1.Deployment) {
	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, d := range deps {
		fmt.Fprintf(tw, "  %s\t%d/%d ready\n", d.Name, d.Status.ReadyReplicas, wantReplicas(d))
	}
	tw.Flush()
}
