package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	felis "felis.lolicon.best"
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/distributed"
	"felis.lolicon.best/internal/platform"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func cmdNode(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "felis node: list | token | join | approve | firewall")
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis node requires root/sudo")
		return 1
	}
	if args[0] == "firewall" {
		return nodeFirewall(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("node "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "stable worker node name")
	kubeconfig := fs.String("kubeconfig", "/etc/rancher/k3s/k3s.yaml", "A's local administrator kubeconfig")
	ns := fs.String("namespace", platform.DefaultMinecraftNamespace, "world namespace")
	controlNS := fs.String("control-namespace", platform.DefaultControlNamespace, "controller namespace")
	image := fs.String("image", "", "Felis image to re-pull and use for admission probes")
	remote := fs.String("ssh-target", "", "SSH target of the trusted worker (normal SSH host verification applies)")
	out := fs.String("out", "", "token output file; never printed to stdout")
	ttl := fs.Duration("ttl", 10*time.Minute, "bootstrap token lifetime (maximum 1h)")
	server := fs.String("server", "", "A's https://address:6443 endpoint for join")
	tokenFile := fs.String("token-file", "", "CA-pinned bootstrap token file for join")
	registry := fs.String("registry-ip", "", "registry ClusterIP for join")
	peers := fs.String("peers", "", "comma-separated exact peer CIDRs for host firewall")
	external := fs.String("external-ip", "", "this worker's fixed external IP")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	switch args[0] {
	case "token":
		if *name == "" || *out == "" || *ttl <= 0 || *ttl > time.Hour {
			fmt.Fprintln(stderr, "node token: name, output file and lifetime <=1h required")
			return 2
		}
		cmd := exec.CommandContext(ctx, "k3s", "token", "create", "--ttl", ttl.String(), "--description", "Felis worker "+*name)
		cmd.Stderr = stderr
		raw, err := cmd.Output()
		if err != nil {
			fmt.Fprintln(stderr, "node token: creation failed:", err)
			return 1
		}
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		_, err = f.Write(raw)
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "limited bootstrap token written to", *out)
		return 0
	case "join":
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		cmd := exec.CommandContext(ctx, "bash", "-s")
		cmd.Stdin = strings.NewReader(felis.BootstrapScript())
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		cmd.Env = append(os.Environ(), "FELIS_INSTALL_MODE=worker", "FELIS_BOOTSTRAP_FROM_TUI=1", "FELIS_BOOTSTRAP_BINARY="+exe, "FELIS_NODE_NAME="+*name, "FELIS_SERVER_URL="+*server, "FELIS_BOOTSTRAP_TOKEN_FILE="+*tokenFile, "FELIS_REGISTRY_CLUSTER_IP="+*registry, "FELIS_PEER_CIDRS="+*peers, "FELIS_NODE_EXTERNAL_IP="+*external)
		if err = cmd.Run(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "list", "approve":
	default:
		fmt.Fprintln(stderr, "unknown node operation")
		return 2
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	v1alpha1.AddToScheme(scheme)
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	m := &distributed.Manager{Client: cl, Namespace: *ns}
	if args[0] == "list" {
		nodes, err := m.Nodes(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		json.NewEncoder(stdout).Encode(nodes)
		return 0
	}
	if *name == "" || *image == "" || *remote == "" || strings.HasPrefix(*remote, "-") {
		fmt.Fprintln(stderr, "node approve requires name, image and SSH target")
		return 2
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err = approveNode(ctx, cl, cs, *ns, *controlNS, *name, *image, *remote, stdout); err != nil {
		fmt.Fprintln(stderr, "node remains quarantined:", err)
		return 1
	}
	return 0
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func exactCIDR(ip string) (string, error) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "", fmt.Errorf("invalid node address %q", ip)
	}
	if parsed.To4() != nil {
		return parsed.String() + "/32", nil
	}
	return parsed.String() + "/128", nil
}
