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
	"sort"
	"strings"
	"syscall"
	"time"

	felis "felis.lolicon.best"
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/distributed"
	"felis.lolicon.best/internal/nodecontrol"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/platform"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
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
	manager.ArtifactDirs = []string{supportBundleDir}
	manager.Host = execute.overview
	manager.OffsiteKey = offsiteKey
	quiet := "/run/felis/watchdog-quiet-until"
	if w, found, err := watchdogUnitFlags(filepath.Join(systemdUnitDir, "felis-watchdog.service")); err == nil && found {
		quiet = w.quietPath
	}
	manager.Quiet = hostQuiet(quiet)
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

// nodeControlUncordon lifts the scheduling quarantine an approval task put on a node.
const nodeControlUncordon = `{"spec":{"unschedulable":false},"metadata":{"annotations":{"felis.lolicon.best/node-control-cordon":null}}}`

type nodeExecutor struct {
	cs                                                                kubernetes.Interface
	binary, kubeconfig, config, namespace, controlNamespace, stateDir string
	// Overrides of the host paths the HA tasks read (node_control_ha.go); empty means
	// the real ones.
	serverToken, agentToken, etcDir, etcdDir string
	// self is this host's node name; empty means selfNodeName's answer.
	self string
	// Overrides for the host tasks (host_tasks.go): the shell the installer runs in, the
	// host settings file and the support bundle directory; empty means the real ones.
	shell, settingsFile, supportDir string
}

func (e nodeExecutor) selfName() string {
	if e.self != "" {
		return e.self
	}
	return selfNodeName(k3sFelisDropin)
}

func (e nodeExecutor) run(ctx context.Context, r nodecontrol.Request, stage func(string) error, out io.Writer) (resultErr error) {
	// Retain task-added quarantine only after admission failure; earlier refusal restores the prior node state.
	if !nodecontrol.NodeAction(r.Action) {
		return e.hostTask(ctx, r, stage, out)
	}
	if r.Action == "fence" {
		return e.fence(ctx, r.Name, stage, out)
	}
	if r.Action == "unfence" {
		return e.unfence(ctx, r.Name, stage, out)
	}
	admitting := false
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
		if err := e.liftFence(ctx, r.Name, out); err != nil {
			return err
		}
		if !node.Spec.Unschedulable {
			defer func() {
				if resultErr == nil || admitting {
					return
				}
				cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := e.cs.CoreV1().Nodes().Patch(cleanup, r.Name, types.MergePatchType, []byte(nodeControlUncordon), metav1.PatchOptions{}); err != nil {
					fmt.Fprintln(out, "Scheduling could not be re-enabled:", err)
				}
			}()
		}
	}
	// Adding a controller or enabling HA moves no world: servers may keep running.
	if r.Action != "enable-ha" && r.Action != "join-controller" {
		if err := e.requireStopped(ctx, r); err != nil {
			return err
		}
	}
	nodes, err := e.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	controller, peers, err := nodeController(nodes.Items, r.ExternalIP, r.Peers, e.selfName())
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
	if r.Action == "enable-ha" {
		return e.enableHA(ctx, r, controller, peers, stage, out)
	}
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
	if r.Action == "join" || r.Action == "join-controller" {
		check += "[ ! -e /etc/rancher/k3s/k3s.yaml ] && [ ! -e /var/lib/rancher/k3s/agent ] || { echo 'k3s is already installed; enrollment will not overwrite an existing node' >&2; exit 1; }\n"
		check += "ip -o address show | awk '{print $4}' | cut -d/ -f1 | grep -Fx -- " + shellQuote(r.ExternalIP) + " >/dev/null || { echo 'fixed node IP is not assigned to the worker' >&2; exit 1; }\n"
	}
	if r.Action == "upgrade" {
		check += "[ -d /var/lib/rancher/k3s/agent ] && [ ! -e /var/lib/rancher/k3s/server ] || { echo 'this host is not a Felis worker' >&2; exit 1; }\n"
	}

	if err = e.ssh(ctx, r.SSHTarget, "if [ \"$(id -u)\" = 0 ]; then bash -s; else sudo -n bash -s; fi", strings.NewReader(check), out); err != nil {
		return fmt.Errorf("worker preflight or SSH connection failed: %w", err)
	}
	if r.Action == "upgrade" {
		return e.upgrade(ctx, r, nodes.Items, controller, peers, stage, out)
	}
	if r.Action == "join-controller" {
		return e.joinController(ctx, r, nodes.Items, controller, peers, stage, out)
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
	admitting = true
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
		if _, err = e.cs.CoreV1().Nodes().Patch(ctx, r.Name, types.MergePatchType, []byte(nodeControlUncordon), metav1.PatchOptions{}); err != nil {
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

// Prefer this controller for node tasks, otherwise the first online controller by name.
func nodeController(nodes []corev1.Node, ip string, extra []string, self string) (*corev1.Node, []string, error) {
	var controller *corev1.Node
	peers := append([]string{}, extra...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	for i := range nodes {
		n := &nodes[i]
		_, controlPlane := n.Labels["node-role.kubernetes.io/control-plane"]
		if (controlPlane || n.Labels[placement.LabelRole] == placement.RoleController) && placement.Online(n) {
			if controller == nil || n.Name == self {
				controller = n
			}
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
	if controller == nil {
		return nil, nil, errors.New("no controller is online")
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
	if err = e.refreshFirewalls(ctx, list.Items, controller, peers, controllerCIDRs(list.Items), out); err != nil {
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
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		e.ssh(cleanup, r.SSHTarget, rootCommand, strings.NewReader("rm -rf -- "+shellQuote(remoteDir)), io.Discard)
	}()
	// stdin carries the token; it is never in command arguments, task records or logs.
	if err = e.ssh(ctx, r.SSHTarget, "if [ \"$(id -u)\" = 0 ]; then "+script+"; else sudo -n sh -c "+shellQuote(script)+"; fi", strings.NewReader(string(token)), out); err != nil {
		return err
	}

	if err = e.pushBinary(ctx, r.SSHTarget, remoteDir, out); err != nil {
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
	if err = e.waitReady(ctx, r.Name); err != nil {
		return err
	}
	return e.recordFelisVersion(ctx, r.Name)
}

// rootCommand runs the script on stdin as root over SSH, through sudo -n for a non-root
// login.
const rootCommand = "if [ \"$(id -u)\" = 0 ]; then bash -s; else sudo -n bash -s; fi"

// pushBinary copies this felis binary to remoteDir/felis on the worker, root-only.
func (e nodeExecutor) pushBinary(ctx context.Context, target, remoteDir string, out io.Writer) error {
	binary, err := os.Open(e.binary)
	if err != nil {
		return err
	}
	defer binary.Close()
	script := "set -eu; cat > " + shellQuote(remoteDir+"/felis") + "; chmod 0700 " + shellQuote(remoteDir+"/felis")
	return e.ssh(ctx, target, "if [ \"$(id -u)\" = 0 ]; then "+script+"; else sudo -n sh -c "+shellQuote(script)+"; fi", binary, out)
}

func (e nodeExecutor) recordFelisVersion(ctx context.Context, name string) error {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{placement.AnnotationFelisVersion: version}}})
	_, err := e.cs.CoreV1().Nodes().Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// Require stopped worker servers before upgrading k3s and rebuilding peer firewall rules.
func (e nodeExecutor) upgrade(ctx context.Context, r nodecontrol.Request, nodes []corev1.Node, controller *corev1.Node, peers []string, stage func(string) error, out io.Writer) error {
	found := false
	for _, n := range nodes {
		if n.Name == r.Name {
			if n.Labels[placement.LabelRole] != placement.RoleWorker {
				return errors.New("only worker nodes can be updated this way")
			}
			found = true
		}
	}
	if !found {
		return errors.New("no such node; join it first")
	}
	if err := stage("transfer"); err != nil {
		return err
	}
	remoteDir := "/var/tmp/felis-node-upgrade-" + strings.ReplaceAll(r.Name, ".", "-")
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		e.ssh(cleanup, r.SSHTarget, rootCommand, strings.NewReader("rm -rf -- "+shellQuote(remoteDir)), io.Discard)
	}()
	if err := e.ssh(ctx, r.SSHTarget, rootCommand, strings.NewReader("set -eu; umask 077; rm -rf -- "+shellQuote(remoteDir)+"; mkdir "+shellQuote(remoteDir)), out); err != nil {
		return err
	}
	if err := e.pushBinary(ctx, r.SSHTarget, remoteDir, out); err != nil {
		return err
	}
	if err := stage("upgrade_worker"); err != nil {
		return err
	}
	script := "set -eu\nexport FELIS_K3S_VERSION=" + shellQuote(controller.Status.NodeInfo.KubeletVersion) + "\n" + shellQuote(remoteDir+"/felis") + " node upgrade --name " + shellQuote(r.Name) + " --server " + shellQuote("https://"+net.JoinHostPort(controllerIP(controller), "6443")) + " --peers " + shellQuote(strings.Join(peers, ",")) + "\n"
	if err := e.ssh(ctx, r.SSHTarget, rootCommand, strings.NewReader(script), out); err != nil {
		return fmt.Errorf("worker update failed: %w", err)
	}
	if err := stage("wait_ready"); err != nil {
		return err
	}
	if err := e.waitReady(ctx, r.Name); err != nil {
		return err
	}
	if err := e.recordFelisVersion(ctx, r.Name); err != nil {
		return err
	}
	return stage("complete")
}

// waitReady waits up to five minutes for the node to report Ready.
func (e nodeExecutor) waitReady(ctx context.Context, name string) error {
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		n, err := e.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
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
	if err := e.backupCluster(ctx, "pre-distributed", out); err != nil {
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

func (e nodeExecutor) backupCluster(ctx context.Context, label string, out io.Writer) (err error) {
	dir := filepath.Join(filepath.Dir(e.stateDir), "cluster-backups")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, label+"-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".tar")
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

// Fence only lost nodes; controller fencing requires a surviving peer and repeated calls clean up remaining Pods.
func (e nodeExecutor) fence(ctx context.Context, name string, stage func(string) error, out io.Writer) error {
	if err := stage("fence"); err != nil {
		return err
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := e.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if placement.Online(node) {
			return errors.New("the node is online: stop or move its servers instead of fencing it")
		}
		switch node.Labels[placement.LabelRole] {
		case placement.RoleWorker:
		case placement.RoleController:
			// With controller high availability a lost controller is fenced from another
			// one, never from itself and never the last one standing.
			if name == e.selfName() {
				return errors.New("a controller cannot fence itself")
			}
			nodes, err := e.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			others := false
			for i := range nodes.Items {
				n := &nodes.Items[i]
				if n.Name != name && n.Labels[placement.LabelRole] == placement.RoleController && placement.Online(n) {
					others = true
				}
			}
			if !others {
				return errors.New("the last controller cannot be fenced")
			}
		default:
			return errors.New("only a worker or, with controller high availability, a controller can be fenced")
		}
		node.Spec.Unschedulable = true
		delete(node.Labels, placement.LabelApproved)
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}
		now := metav1.Now()
		if node.Annotations[distributed.AnnotationFenced] == "" {
			node.Annotations[distributed.AnnotationFenced] = now.UTC().Format(time.RFC3339)
		}
		if !slices.ContainsFunc(node.Spec.Taints, isOutOfService) {
			node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: distributed.TaintOutOfService, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute, TimeAdded: &now})
		}
		_, err = e.cs.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Node %s is cordoned, no longer approved, and out of service.\n", name)
	if err := stage("release_pods"); err != nil {
		return err
	}
	pods, err := e.cs.CoreV1().Pods(e.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	zero := int64(0)
	for _, p := range pods.Items {
		if p.Spec.NodeName != name {
			continue
		}
		if err := e.cs.CoreV1().Pods(e.namespace).Delete(ctx, p.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		fmt.Fprintf(out, "Released pod %s.\n", p.Name)
	}
	return stage("complete")
}

// Remove Kubernetes' persistent out-of-service taint when readmitting a worker.
func (e nodeExecutor) liftFence(ctx context.Context, name string, out io.Writer) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := e.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		fenced := slices.ContainsFunc(node.Spec.Taints, isOutOfService)
		if !fenced && node.Annotations[distributed.AnnotationFenced] == "" {
			return nil
		}
		if !placement.Online(node) {
			return errors.New("the fenced node is still offline; approve it once it is back")
		}
		node.Spec.Taints = slices.DeleteFunc(node.Spec.Taints, isOutOfService)
		delete(node.Annotations, distributed.AnnotationFenced)
		if _, err := e.cs.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
			return err
		}
		fmt.Fprintf(out, "Node %s is back in service.\n", name)
		return nil
	})
}

// Controllers rejoin through unfence; workers require approval.
func (e nodeExecutor) unfence(ctx context.Context, name string, stage func(string) error, out io.Writer) error {
	if err := stage("unfence"); err != nil {
		return err
	}
	node, err := e.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if node.Labels[placement.LabelRole] != placement.RoleController {
		return errors.New("a fenced worker returns through approval, which checks it again")
	}
	if !slices.ContainsFunc(node.Spec.Taints, isOutOfService) && node.Annotations[distributed.AnnotationFenced] == "" {
		return errors.New("the node is not fenced")
	}
	if !placement.Online(node) {
		return errors.New("the fenced controller is still offline")
	}
	if err := e.liftFence(ctx, name, out); err != nil {
		return err
	}
	if _, err := e.cs.CoreV1().Nodes().Patch(ctx, name, types.MergePatchType, []byte(nodeControlUncordon), metav1.PatchOptions{}); err != nil {
		return err
	}
	return stage("complete")
}

func isOutOfService(t corev1.Taint) bool {
	return t.Key == distributed.TaintOutOfService && t.Effect == corev1.TaintEffectNoExecute
}
