package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	felis "felis.lolicon.best"
	"felis.lolicon.best/internal/nodecontrol"
	"felis.lolicon.best/internal/placement"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Host paths the HA tasks read. Defaults are the k3s and Felis ones; tests point them
// elsewhere.
const (
	k3sServerTokenPath = "/var/lib/rancher/k3s/server/token"
	k3sAgentTokenPath  = "/etc/rancher/k3s/felis-agent-token"
	k3sEtcdDir         = "/var/lib/rancher/k3s/server/db/etcd"
	felisEtcDir        = "/etc/felis"
	// longhornDiskLabel lets Longhorn keep replicas on the node (the installer sets
	// create-default-disk-labeled-nodes): controllers only.
	longhornDiskLabel = "node.longhorn.io/create-default-disk"
)

func (e nodeExecutor) path(override, def string) string {
	if override != "" {
		return override
	}
	return def
}

// controllerCIDRs are the exact addresses of every controller, plus extra.
func controllerCIDRs(nodes []corev1.Node, extra ...string) []string {
	out := append([]string{}, extra...)
	for i := range nodes {
		n := &nodes[i]
		if n.Labels[placement.LabelRole] != placement.RoleController {
			continue
		}
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeExternalIP || a.Type == corev1.NodeInternalIP {
				if cidr, err := exactCIDR(a.Address); err == nil {
					out = append(out, cidr)
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Refresh every existing node's peer firewall; joining nodes install their own rules.
func (e nodeExecutor) refreshFirewalls(ctx context.Context, nodes []corev1.Node, local *corev1.Node, peers, controllers []string, out io.Writer) error {
	common := " --peers " + shellQuote(strings.Join(peers, ",")) + " --controller-peers " + shellQuote(strings.Join(controllers, ",")) +
		" --namespace " + shellQuote(e.namespace) + " --control-namespace " + shellQuote(e.controlNamespace)
	for i := range nodes {
		n := &nodes[i]
		if n.Name == local.Name {
			continue
		}
		var script string
		switch n.Labels[placement.LabelRole] {
		case placement.RoleWorker:
			script = shellQuote(e.binary) + " node firewall --controller-ip " + shellQuote(controllerIP(local)) + common
		case placement.RoleController:
			script = shellQuote(e.binary) + " node firewall --controller --controller-ip " + shellQuote(controllerIP(n)) + common
		default:
			return fmt.Errorf("node %s has an unknown role", n.Name)
		}
		if err := e.ssh(ctx, n.Name, "if [ \"$(id -u)\" = 0 ]; then "+script+"; else sudo -n "+script+"; fi", nil, out); err != nil {
			return fmt.Errorf("update peer firewall on %s: %w", n.Name, err)
		}
	}
	cmd := exec.CommandContext(ctx, e.binary, "node", "firewall", "--controller", "--controller-ip", controllerIP(local), "--peers", strings.Join(peers, ","),
		"--controller-peers", strings.Join(controllers, ","), "--namespace", e.namespace, "--control-namespace", e.controlNamespace)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// Snapshot database and host state before rerunning the installer with controller HA enabled.
func (e nodeExecutor) enableHA(ctx context.Context, r nodecontrol.Request, controller *corev1.Node, peers []string, stage func(string) error, out io.Writer) error {
	if controller.Labels[placement.LabelRole] != placement.RoleController {
		return errors.New("enable distributed mode first: controller high availability builds on it")
	}
	if err := stage("database_backup"); err != nil {
		return err
	}
	backup := exec.CommandContext(ctx, e.binary, "db", "backup", "-config", e.config, "-dir", "/var/lib/felis/db-backups", "-label", "pre-migrate")
	backup.Stdout = out
	backup.Stderr = out
	if err := backup.Run(); err != nil {
		return err
	}
	if err := stage("cluster_backup"); err != nil {
		return err
	}
	if err := e.backupCluster(ctx, "pre-ha", out); err != nil {
		return err
	}
	if err := stage("install_ha"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "bash", "-s")
	cmd.Stdin = strings.NewReader(felis.BootstrapScript())
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = append(os.Environ(), "FELIS_DISTRIBUTED=1", "FELIS_CONTROLLER_HA=1", "FELIS_NODE_EXTERNAL_IP="+controllerIP(controller), "FELIS_PEER_CIDRS="+strings.Join(peers, ","),
		"FELIS_BOOTSTRAP_FROM_TUI=1", "FELIS_BOOTSTRAP_BINARY="+e.binary, "FELIS_NO_SETUP=1", "FELIS_NODE_CONTROL_TASK=1", "FELIS_INSTALL_MODE=full")
	// The floating address the panel named, as the installer takes it (FELIS_HA_VIP*).
	for param, env := range map[string]string{"vip": "FELIS_HA_VIP", "vipInterface": "FELIS_HA_VIP_INTERFACE", "vrid": "FELIS_HA_VRID"} {
		if v := r.Params[param]; v != "" {
			cmd.Env = append(cmd.Env, env+"="+v)
		}
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("controller high availability could not be enabled; retain the database backup: %w", err)
	}
	return stage("complete")
}

// Update peer firewalls before SSH credential transfer, then join etcd and admit the ready controller.
func (e nodeExecutor) joinController(ctx context.Context, r nodecontrol.Request, nodes []corev1.Node, controller *corev1.Node, peers []string, stage func(string) error, out io.Writer) error {
	for _, n := range nodes {
		if n.Name == r.Name {
			return errors.New("node already exists")
		}
	}
	if _, err := os.Stat(e.path(e.etcdDir, k3sEtcdDir)); err != nil {
		return errors.New("this cluster runs no embedded etcd: enable controller high availability first")
	}
	newIP, err := exactCIDR(r.ExternalIP)
	if err != nil {
		return err
	}
	controllers := controllerCIDRs(nodes, newIP)
	if err := stage("firewall"); err != nil {
		return err
	}
	if err := e.refreshFirewalls(ctx, nodes, controller, peers, controllers, out); err != nil {
		return err
	}
	if err := stage("transfer"); err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "felis-controller-join-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	etc := e.path(e.etcDir, felisEtcDir)
	bundle := filepath.Join(temp, "etc-felis.tar")
	tar := exec.CommandContext(ctx, "tar", "-C", filepath.Dir(etc), "-cf", bundle, filepath.Base(etc))
	tar.Stdout, tar.Stderr = out, out
	if err := tar.Run(); err != nil {
		return fmt.Errorf("pack %s: %w", etc, err)
	}
	remoteDir := "/var/tmp/felis-node-" + filepath.Base(temp)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		e.ssh(cleanup, r.SSHTarget, rootCommand, strings.NewReader("rm -rf -- "+shellQuote(remoteDir)), io.Discard)
	}()
	if err := e.ssh(ctx, r.SSHTarget, rootCommand, strings.NewReader("set -eu; umask 077; mkdir "+shellQuote(remoteDir)), out); err != nil {
		return err
	}
	// Each file goes over stdin, so no secret is in a command line, a task record or a log.
	for _, f := range []struct{ local, remote string }{
		{e.path(e.serverToken, k3sServerTokenPath), "server-token"},
		{e.path(e.agentToken, k3sAgentTokenPath), "agent-token"},
		{bundle, "etc-felis.tar"},
	} {
		in, err := os.Open(f.local)
		if err != nil {
			return err
		}
		script := "set -eu; umask 077; cat > " + shellQuote(remoteDir+"/"+f.remote)
		err = e.ssh(ctx, r.SSHTarget, "if [ \"$(id -u)\" = 0 ]; then "+script+"; else sudo -n sh -c "+shellQuote(script)+"; fi", in, out)
		in.Close()
		if err != nil {
			return err
		}
	}
	if err := e.pushBinary(ctx, r.SSHTarget, remoteDir, out); err != nil {
		return err
	}
	if err := stage("install_controller"); err != nil {
		return err
	}
	registry, err := e.cs.CoreV1().Services(e.controlNamespace).Get(ctx, "registry", metav1.GetOptions{})
	if err != nil {
		return err
	}
	script := "set -eu\nexport FELIS_K3S_VERSION=" + shellQuote(controller.Status.NodeInfo.KubeletVersion) + "\n" +
		shellQuote(remoteDir+"/felis") + " node join-controller --name " + shellQuote(r.Name) +
		" --server " + shellQuote("https://"+net.JoinHostPort(controllerIP(controller), "6443")) +
		" --external-ip " + shellQuote(r.ExternalIP) +
		" --server-token-file " + shellQuote(remoteDir+"/server-token") +
		" --agent-token-file " + shellQuote(remoteDir+"/agent-token") +
		" --etc-bundle " + shellQuote(remoteDir+"/etc-felis.tar") +
		" --registry-ip " + shellQuote(registry.Spec.ClusterIP) +
		" --peers " + shellQuote(strings.Join(peers, ",")) +
		" --controller-peers " + shellQuote(strings.Join(controllers, ",")) + "\n"
	if err := e.ssh(ctx, r.SSHTarget, rootCommand, strings.NewReader(script), out); err != nil {
		return fmt.Errorf("controller installation failed: %w", err)
	}
	if err := stage("wait_ready"); err != nil {
		return err
	}
	if err := e.waitReady(ctx, r.Name); err != nil {
		return err
	}
	if err := stage("label"); err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q,%q:%q,%q:"true"}}}`, placement.LabelRole, placement.RoleController, placement.LabelIdentity, r.Name, longhornDiskLabel)
	if _, err := e.cs.CoreV1().Nodes().Patch(ctx, r.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return err
	}
	if err := e.recordFelisVersion(ctx, r.Name); err != nil {
		return err
	}
	return stage("complete")
}
