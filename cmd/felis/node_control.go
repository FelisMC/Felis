package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	felis "felis.lolicon.best"
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/nodecontrol"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/platform"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func cmdNodeControl(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node-control", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", nodecontrol.Socket, "local API-only Unix socket")
	dir := fs.String("state", "/var/lib/felis/node-control", "root-owned persistent task state")
	kubeconfig := fs.String("kubeconfig", "/etc/rancher/k3s/k3s.yaml", "controller kubeconfig")
	config := fs.String("config", "/etc/felis/felis.host.toml", "host configuration")
	ns := fs.String("namespace", platform.DefaultMinecraftNamespace, "world namespace")
	controlNS := fs.String("control-namespace", platform.DefaultControlNamespace, "control namespace")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "node-control requires root")
		return 1
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	execute := nodeExecutor{cs: cs, binary: exe, kubeconfig: *kubeconfig, config: *config, namespace: *ns, controlNamespace: *controlNS, stateDir: *dir}
	socketDir := filepath.Dir(*socket)
	if err = os.MkdirAll(socketDir, 0750); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err = os.Chown(socketDir, 0, 65532); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err = os.Chmod(socketDir, 0750); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	if existing, err := os.Lstat(*socket); err == nil {
		if existing.Mode()&os.ModeSocket == 0 {
			fmt.Fprintln(stderr, "refusing to replace non-socket path")
			return 1
		}
		conn, err := net.DialTimeout("unix", *socket, time.Second)
		if err == nil {
			conn.Close()
			fmt.Fprintln(stderr, "node-control is already running")
			return 1
		}
		if err = os.Remove(*socket); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer listener.Close()
	if err = os.Chown(*socket, 0, 65532); err == nil {
		err = os.Chmod(*socket, 0660)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// /run is recreated at boot; label both the directory and the new socket inode.
	if os.Getenv("FELIS_NODE_CONTROL_SELINUX") == "1" {
		if err := exec.Command("chcon", "-R", "-t", "felis_node_control_socket_t", socketDir).Run(); err != nil {
			fmt.Fprintln(stderr, "node-control socket labeling failed:", err)
			return 1
		}
	}
	manager, err := nodecontrol.Open(ctx, *dir, execute.run)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	server := &http.Server{Handler: manager.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	go func() { <-ctx.Done(); server.Close() }()
	fmt.Fprintln(stdout, "node-control listening on", *socket)
	err = server.Serve(listener)
	cancel()
	manager.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

type nodeExecutor struct {
	cs                                                                kubernetes.Interface
	binary, kubeconfig, config, namespace, controlNamespace, stateDir string
}

func (e nodeExecutor) run(ctx context.Context, r nodecontrol.Request, stage func(string) error, out io.Writer) (resultErr error) {
	// Host-wide changes and cache-removing admission probes are serialized by the manager.
	if r.Action == "approve" {
		node, err := e.cs.CoreV1().Nodes().Get(ctx, r.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if node.Labels[placement.LabelRole] != placement.RoleWorker {
			return errors.New("only worker nodes can be approved")
		}
		patch := []byte(`{"spec":{"unschedulable":true}}`)
		if !node.Spec.Unschedulable {
			patch = []byte(`{"spec":{"unschedulable":true},"metadata":{"annotations":{"felis.lolicon.best/node-control-cordon":"true"}}}`)
		}
		if _, err = e.cs.CoreV1().Nodes().Patch(ctx, r.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return err
		}
	}
	if err := e.requireStopped(ctx, r); err != nil {
		return err
	}
	nodes, err := e.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	controller, peers, err := nodeController(nodes.Items, r.ExternalIP, r.Peers)
	if err != nil {
		return err
	}
	deployment, err := e.cs.AppsV1().Deployments(e.controlNamespace).Get(ctx, platform.SAAPI, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if len(deployment.Spec.Template.Spec.Containers) == 0 {
		return errors.New("the Felis API image is unavailable")
	}
	image := deployment.Spec.Template.Spec.Containers[0].Image
	if r.Action == "enable" {
		if err := stage("pause_operator"); err != nil {
			return err
		}
		scale, err := e.cs.AppsV1().Deployments(e.controlNamespace).GetScale(ctx, platform.SAOperator, metav1.GetOptions{})
		if err != nil {
			return err
		}
		replicas := scale.Spec.Replicas
		if replicas < 1 {
			replicas = 1
		}
		scale.Spec.Replicas = 0
		if _, err = e.cs.AppsV1().Deployments(e.controlNamespace).UpdateScale(ctx, platform.SAOperator, scale, metav1.UpdateOptions{}); err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			latest, err := e.cs.AppsV1().Deployments(e.controlNamespace).GetScale(cleanup, platform.SAOperator, metav1.GetOptions{})
			if err == nil {
				latest.Spec.Replicas = replicas
				_, err = e.cs.AppsV1().Deployments(e.controlNamespace).UpdateScale(cleanup, platform.SAOperator, latest, metav1.UpdateOptions{})
			}
			if err != nil {
				fmt.Fprintln(out, "Operator restoration failed:", err)
				resultErr = fmt.Errorf("operator restoration failed: %w", err)
			}
		}()
		// Drain the old reconciler before the second stopped-state check.
		for {
			pods, err := e.cs.CoreV1().Pods(e.controlNamespace).List(ctx, metav1.ListOptions{LabelSelector: platform.LabelComponent + "=" + platform.ComponentOperator})
			if err != nil {
				return err
			}
			if len(pods.Items) == 0 {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		if err = e.requireStopped(ctx, r); err != nil {
			return err
		}
		return e.enable(ctx, r, controller, peers, stage, out)
	}
	if controller.Labels[placement.LabelRole] != placement.RoleController {
		return errors.New("distributed mode is not configured on the controller")
	}
	if r.Name == controller.Name {
		return errors.New("worker name must differ from the controller")
	}
	// SSH trust and keys are configured on A; no password, key or bootstrap token crosses the API.
	if err := stage("ssh_check"); err != nil {
		return err
	}
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if arch == "" {
		return errors.New("unsupported host architecture")
	}
	check := "set -eu\n[ \"$(uname -s)\" = Linux ] || { echo 'worker requires Linux' >&2; exit 1; }\n[ \"$(uname -m)\" = " + shellQuote(arch) + " ] || { echo 'worker architecture differs from controller' >&2; exit 1; }\n"
	if r.Action == "join" {
		check += "[ ! -e /etc/rancher/k3s/k3s.yaml ] && [ ! -e /var/lib/rancher/k3s/agent ] || { echo 'k3s is already installed; enrollment will not overwrite an existing node' >&2; exit 1; }\n"
		check += "ip -o address show | awk '{print $4}' | cut -d/ -f1 | grep -Fx -- " + shellQuote(r.ExternalIP) + " >/dev/null || { echo 'fixed node IP is not assigned to the worker' >&2; exit 1; }\n"
	}

	if err = e.ssh(ctx, r.SSHTarget, "if [ \"$(id -u)\" = 0 ]; then bash -s; else sudo -n bash -s; fi", strings.NewReader(check), out); err != nil {
		return fmt.Errorf("worker preflight or SSH connection failed: %w", err)
	}
	if r.Action == "join" {
		for _, n := range nodes.Items {
			if n.Name == r.Name {
				return errors.New("node already exists; use approval instead of reinstalling it")
			}
		}
		if err = e.join(ctx, r, controller, peers, stage, out); err != nil {
			return err
		}
	}
	if err = stage("approval"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, e.binary, "node", "approve", "--name", r.Name, "--ssh-target", r.SSHTarget, "--image", image, "--kubeconfig", e.kubeconfig, "--namespace", e.namespace, "--control-namespace", e.controlNamespace)
	cmd.Stdout = out
	cmd.Stderr = out
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("node approval failed; node remains quarantined: %w", err)
	}
	// Admission succeeded; release only the task's scheduling quarantine.
	admitted, err := e.cs.CoreV1().Nodes().Get(ctx, r.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if admitted.Annotations["felis.lolicon.best/node-control-cordon"] == "true" {
		if _, err = e.cs.CoreV1().Nodes().Patch(ctx, r.Name, types.MergePatchType, []byte(`{"spec":{"unschedulable":false},"metadata":{"annotations":{"felis.lolicon.best/node-control-cordon":null}}}`), metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("node approved but scheduling remains disabled: %w", err)
		}
	}
	return stage("complete")
}
func (e nodeExecutor) requireStopped(ctx context.Context, r nodecontrol.Request) error {
	// Even pre-existing worker names may contain worlds. Never run installer/admission on an occupied worker.
	ss, err := e.cs.AppsV1().StatefulSets(e.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, s := range ss.Items {
		if r.Action != "enable" && s.Spec.Template.Spec.NodeSelector[placement.LabelIdentity] != r.Name {
			continue
		}
		if s.Spec.Replicas != nil && *s.Spec.Replicas > 0 {
			return fmt.Errorf("StatefulSet %s still requests %d replicas; stop the server before changing nodes", s.Name, *s.Spec.Replicas)
		}
	}
	pods, err := e.cs.CoreV1().Pods(e.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		if r.Action != "enable" && p.Spec.NodeName != r.Name && p.Spec.NodeSelector[placement.LabelIdentity] != r.Name {
			continue
		}
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			return fmt.Errorf("pod %s has not exited (phase %s); wait for game and maintenance workloads to stop", p.Name, p.Status.Phase)
		}
	}
	// Prevent a pending wake intent racing admission or an installation.
	raw, err := e.cs.CoreV1().RESTClient().Get().AbsPath("/apis/" + v1alpha1.GroupVersion.Group + "/" + v1alpha1.GroupVersion.Version + "/namespaces/" + e.namespace + "/minecraftservers").DoRaw(ctx)
	if err != nil {
		return err
	}
	var servers v1alpha1.MinecraftServerList
	if err = json.Unmarshal(raw, &servers); err != nil {
		return err
	}
	for _, s := range servers.Items {
		if r.Action != "enable" && s.Spec.NodeName != r.Name && s.Status.NodeName != r.Name {
			continue
		}
		if s.Spec.DesiredState != "" && string(s.Spec.DesiredState) != "Stopped" {
			return fmt.Errorf("server %s must have stopped intent", s.Name)
		}
	}
	return nil
}

func nodeController(nodes []corev1.Node, ip string, extra []string) (*corev1.Node, []string, error) {
	var controller *corev1.Node
	peers := append([]string{}, extra...)
	for i := range nodes {
		n := &nodes[i]
		_, controlPlane := n.Labels["node-role.kubernetes.io/control-plane"]
		if controlPlane || n.Labels[placement.LabelRole] == placement.RoleController {
			if controller != nil {
				return nil, nil, errors.New("exactly one controller is required")
			}
			controller = n
		}
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeExternalIP || a.Type == corev1.NodeInternalIP {
				cidr, err := exactCIDR(a.Address)
				if err != nil {
					return nil, nil, err
				}
				peers = append(peers, cidr)
			}
		}
	}
	if controller == nil || !placement.Online(controller) {
		return nil, nil, errors.New("the sole controller must be online")
	}
	if ip != "" {
		cidr, err := exactCIDR(ip)
		if err != nil {
			return nil, nil, err
		}
		peers = append(peers, cidr)
	}
	slices.Sort(peers)
	peers = slices.Compact(peers)
	return controller, peers, nil
}
func controllerIP(n *corev1.Node) string {
	for _, kind := range []corev1.NodeAddressType{corev1.NodeExternalIP, corev1.NodeInternalIP} {
		for _, a := range n.Status.Addresses {
			if a.Type == kind {
				return a.Address
			}
		}
	}
	return ""
}
func (e nodeExecutor) ssh(ctx context.Context, target, command string, in io.Reader, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10", "--", target, command)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}
func (e nodeExecutor) join(ctx context.Context, r nodecontrol.Request, controller *corev1.Node, peers []string, stage func(string) error, out io.Writer) error {
	registry, err := e.cs.CoreV1().Services(e.controlNamespace).Get(ctx, "registry", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err = stage("firewall"); err != nil {
		return err
	}
	// All peers must be updated before the new agent is admitted. Existing worker SSH targets must be configured by stable node name.
	list, err := e.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, n := range list.Items {
		if n.Name == controller.Name || n.Name == r.Name {
			continue
		}
		if n.Labels[placement.LabelRole] != placement.RoleWorker {
			return fmt.Errorf("node %s has an unknown role", n.Name)
		}
		script := shellQuote(e.binary) + " node firewall --peers " + shellQuote(strings.Join(peers, ",")) + " --controller-ip " + shellQuote(controllerIP(controller))
		script += " --namespace " + shellQuote(e.namespace) + " --control-namespace " + shellQuote(e.controlNamespace)
		if err = e.ssh(ctx, n.Name, "if [ \"$(id -u)\" = 0 ]; then "+script+"; else sudo -n "+script+"; fi", nil, out); err != nil {
			return fmt.Errorf("update peer firewall on %s: %w", n.Name, err)
		}
	}
	cmd := exec.CommandContext(ctx, e.binary, "node", "firewall", "--controller", "--controller-ip", controllerIP(controller), "--peers", strings.Join(peers, ","), "--namespace", e.namespace, "--control-namespace", e.controlNamespace)
	cmd.Stdout = out
	cmd.Stderr = out
	if err = cmd.Run(); err != nil {
		return err
	}
	if err = stage("bootstrap_token"); err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "felis-node-join-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	tokenPath := filepath.Join(temp, "bootstrap")
	tokenCmd := exec.CommandContext(ctx, e.binary, "node", "token", "--name", r.Name, "--ttl", "1h", "--out", tokenPath)
	tokenCmd.Stdout = out
	tokenCmd.Stderr = out
	if err = tokenCmd.Run(); err != nil {
		return err
	}
	// Revoke even on failure. The worker exchanges bootstrap credentials for its node identity.
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		exec.CommandContext(cleanup, "k3s", "token", "delete", bootstrapTokenID(string(token))).Run()
	}()
	remoteDir := "/var/tmp/felis-node-" + filepath.Base(temp)
	if err = stage("transfer"); err != nil {
		return err
	}
	script := "set -eu; umask 077; mkdir " + shellQuote(remoteDir) + "; cat > " + shellQuote(remoteDir+"/bootstrap")
	rootCommand := "if [ \"$(id -u)\" = 0 ]; then bash -s; else sudo -n bash -s; fi"
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		e.ssh(cleanup, r.SSHTarget, rootCommand, strings.NewReader("rm -rf -- "+shellQuote(remoteDir)), io.Discard)
	}()
	// stdin carries the token; it is never in command arguments, task records or logs.
	if err = e.ssh(ctx, r.SSHTarget, "if [ \"$(id -u)\" = 0 ]; then "+script+"; else sudo -n sh -c "+shellQuote(script)+"; fi", strings.NewReader(string(token)), out); err != nil {
		return err
	}

	binary, err := os.Open(e.binary)
	if err != nil {
		return err
	}
	defer binary.Close()
	script = "set -eu; cat > " + shellQuote(remoteDir+"/felis") + "; chmod 0700 " + shellQuote(remoteDir+"/felis")
	if err = e.ssh(ctx, r.SSHTarget, "if [ \"$(id -u)\" = 0 ]; then "+script+"; else sudo -n sh -c "+shellQuote(script)+"; fi", binary, out); err != nil {
		return err
	}
	if err = stage("install_worker"); err != nil {
		return err
	}
	script = "set -eu\nexport FELIS_K3S_VERSION=" + shellQuote(controller.Status.NodeInfo.KubeletVersion) + "\n" + shellQuote(remoteDir+"/felis") + " node join --name " + shellQuote(r.Name) + " --server " + shellQuote("https://"+net.JoinHostPort(controllerIP(controller), "6443")) + " --external-ip " + shellQuote(r.ExternalIP) + " --token-file " + shellQuote(remoteDir+"/bootstrap") + " --registry-ip " + shellQuote(registry.Spec.ClusterIP) + " --peers " + shellQuote(strings.Join(peers, ",")) + "\n"
	if err = e.ssh(ctx, r.SSHTarget, rootCommand, strings.NewReader(script), out); err != nil {
		return fmt.Errorf("worker installation failed: %w", err)
	}
	if err = stage("wait_ready"); err != nil {
		return err
	}
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		n, err := e.cs.CoreV1().Nodes().Get(ctx, r.Name, metav1.GetOptions{})
		if err == nil && placement.Online(n) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("worker did not become Ready within five minutes")
		case <-ticker.C:
		}
	}
}
func (e nodeExecutor) enable(ctx context.Context, r nodecontrol.Request, controller *corev1.Node, peers []string, stage func(string) error, out io.Writer) error {
	if controllerIP(controller) != r.ExternalIP {
		return errors.New("controller IP must match the registered fixed node address")
	}
	if len(peers) == 0 {
		return errors.New("peer addresses are required")
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
	if err := e.backupCluster(ctx, out); err != nil {
		return err
	}
	if err := stage("configure_controller"); err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s","%s":"controller"}}}`, placement.LabelIdentity, controller.Name, placement.LabelRole)
	if _, err := e.cs.CoreV1().Nodes().Patch(ctx, controller.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return err
	}
	for _, name := range []string{platform.SAAPI, platform.SAOperator, platform.PostgresName, "registry"} {
		body := fmt.Sprintf(`{"spec":{"template":{"spec":{"nodeSelector":{"%s":"%s"}}}}}`, placement.LabelIdentity, controller.Name)
		if _, err := e.cs.AppsV1().Deployments(e.controlNamespace).Patch(ctx, name, types.MergePatchType, []byte(body), metav1.PatchOptions{}); err != nil {
			return err
		}
	}
	if err := stage("install_controller"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "bash", "-s")
	cmd.Stdin = strings.NewReader(felis.BootstrapScript())
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = append(os.Environ(), "FELIS_DISTRIBUTED=1", "FELIS_NODE_EXTERNAL_IP="+r.ExternalIP, "FELIS_PEER_CIDRS="+strings.Join(peers, ","), "FELIS_BOOTSTRAP_FROM_TUI=1", "FELIS_BOOTSTRAP_BINARY="+e.binary, "FELIS_NO_SETUP=1", "FELIS_NODE_CONTROL_TASK=1", "FELIS_INSTALL_MODE=full")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("controller installation failed; retain the database backup: %w", err)
	}
	return stage("complete")
}

func bootstrapTokenID(token string) string {
	token = strings.TrimSpace(token)
	if _, short, ok := strings.Cut(token, "::"); ok {
		token = short
	}
	id, _, _ := strings.Cut(token, ".")
	return id
}

func (e nodeExecutor) backupCluster(ctx context.Context, out io.Writer) (err error) {
	dir := filepath.Join(filepath.Dir(e.stateDir), "cluster-backups")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, "pre-distributed-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".tar")
	stop := exec.CommandContext(ctx, "systemctl", "stop", "k3s")
	stop.Stdout = out
	stop.Stderr = out
	if err = stop.Run(); err != nil {
		return err
	}
	defer func() {
		restart, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(restart, "systemctl", "start", "k3s")
		cmd.Stdout = out
		cmd.Stderr = out
		if restartErr := cmd.Run(); restartErr != nil {
			err = fmt.Errorf("k3s restart failed after snapshot: %w", restartErr)
		}
	}()
	cmd := exec.CommandContext(ctx, "tar", "-cf", path, "-C", "/var/lib/rancher/k3s/server", "db", "token", "tls")
	cmd.Stdout = out
	cmd.Stderr = out
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("cluster snapshot failed: %w", err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	fmt.Fprintln(out, "Cluster snapshot:", path)
	return nil
}
