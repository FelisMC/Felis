package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/archivetransfer"
	"felis.lolicon.best/internal/operator"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/platform"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func approveNode(ctx context.Context, cl client.Client, cs kubernetes.Interface, ns, controlNS, name, image, remote string, stdout io.Writer) error {
	var n corev1.Node
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		return err
	}
	_, controlPlane := n.Labels["node-role.kubernetes.io/control-plane"]
	if !placement.Online(&n) || controlPlane || n.Labels[placement.LabelRole] == placement.RoleController {
		return fmt.Errorf("candidate must be an online agent")
	}
	var nodes corev1.NodeList
	if err := cl.List(ctx, &nodes); err != nil {
		return err
	}
	var controller *corev1.Node
	var peers []string
	var denied []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Labels[placement.LabelRole] == placement.RoleController {
			if controller != nil {
				return fmt.Errorf("multiple controllers found")
			}
			controller = node
		}
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP || addr.Type == corev1.NodeExternalIP {
				cidr, err := exactCIDR(addr.Address)
				if err != nil {
					return err
				}
				peers = append(peers, cidr)
				for _, p := range []string{"6443", "10250", "5000", "30443"} {
					denied = append(denied, net.JoinHostPort(addr.Address, p))
				}
			}
		}
	}
	if controller == nil || controller.Name == n.Name || controller.Status.NodeInfo.Architecture != n.Status.NodeInfo.Architecture {
		return fmt.Errorf("candidate architecture must match the sole controller")
	}
	if n.Status.NodeInfo.KubeletVersion != controller.Status.NodeInfo.KubeletVersion {
		return fmt.Errorf("candidate k3s version must match A")
	}
	var apiSvc, registrySvc, archiveSvc, kubernetesSvc corev1.Service
	for _, svc := range []struct {
		obj      *corev1.Service
		ns, name string
	}{{&kubernetesSvc, "default", "kubernetes"}, {&apiSvc, controlNS, platform.SAAPI}, {&registrySvc, controlNS, "registry"}, {&archiveSvc, ns, platform.ArchiveName}} {
		if err := cl.Get(ctx, types.NamespacedName{Namespace: svc.ns, Name: svc.name}, svc.obj); err != nil {
			return err
		}
	}
	if len(apiSvc.Spec.Ports) == 0 || apiSvc.Spec.Ports[0].NodePort == 0 {
		return fmt.Errorf("controller API NodePort is absent")
	}
	// A's exact interface addresses let the probe observe DNAT/SNAT before the production policy is adjusted.
	var aCIDRs []string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ip, _, err := net.ParseCIDR(a.String())
			if err == nil && !ip.IsLoopback() {
				cidr, _ := exactCIDR(ip.String())
				aCIDRs = append(aCIDRs, cidr)
			}
		}
	}
	if len(aCIDRs) == 0 {
		return fmt.Errorf("cannot determine A's exact interface addresses")
	}
	onController := false
	for _, addr := range controller.Status.Addresses {
		cidr, err := exactCIDR(addr.Address)
		if err != nil {
			continue
		}
		for _, local := range aCIDRs {
			if cidr == local {
				onController = true
			}
		}
	}
	if !onController || !placement.Online(controller) {
		return fmt.Errorf("run admission on the online controller A")
	}
	// Quarantine first. Approval is the final write, after all checks have succeeded.
	before := n.DeepCopy()
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	delete(n.Labels, placement.LabelApproved)
	found := false
	for _, t := range n.Spec.Taints {
		if t.Key == "felis.lolicon.best/unapproved" {
			found = true
		}
	}
	if !found {
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: "felis.lolicon.best/unapproved", Value: "true", Effect: corev1.TaintEffectNoSchedule})
	}
	if err := cl.Patch(ctx, &n, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	if err := denyNodeAddresses(ctx, cl, ns, nodes.Items); err != nil {
		return err
	}
	// The host's Service dial may be masqueraded to its bridge gateway. Permit exact host addresses only.
	pullSources := append([]string{}, peers...)
	if ip, network, err := net.ParseCIDR(n.Spec.PodCIDR); err == nil && ip.To4() != nil {
		v := append(net.IP(nil), network.IP.To4()...)
		pullSources = append(pullSources, v.String()+"/32")
		v[3]++
		pullSources = append(pullSources, v.String()+"/32")
	}
	var registryNP networkingv1.NetworkPolicy
	if err := cl.Get(ctx, types.NamespacedName{Namespace: controlNS, Name: "felis-registry-ingress"}, &registryNP); err != nil {
		return err
	}
	if len(registryNP.Spec.Ingress) == 0 {
		return fmt.Errorf("registry ingress policy is not configured")
	}
	prev := registryNP.DeepCopy()
	for _, cidr := range pullSources {
		registryNP.Spec.Ingress[0].From = append(registryNP.Spec.Ingress[0].From, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	if err := cl.Patch(ctx, &registryNP, client.MergeFrom(prev)); err != nil {
		return err
	}
	ctrlIP := ""
	for _, a := range controller.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			ctrlIP = a.Address
			break
		}
	}
	if ctrlIP == "" {
		return fmt.Errorf("controller has no address")
	}
	script := "set -euo pipefail\numask 077\n" +
		"systemctl is-active --quiet k3s-agent\n! systemctl is-active --quiet k3s\ntest ! -f /etc/rancher/k3s/k3s.yaml\n" +
		"ip -d link show flannel-wg | grep -q wireguard\n" +
		"/usr/local/bin/felis node firewall --peers " + shellQuote(strings.Join(peers, ",")) + " --controller-ip " + shellQuote(ctrlIP) + " --api-service-ip " + shellQuote(kubernetesSvc.Spec.ClusterIP) + " --node-port " + strconv.Itoa(int(apiSvc.Spec.Ports[0].NodePort)) + " --namespace " + shellQuote(ns) + " --control-namespace " + shellQuote(controlNS) + "\n" +
		"kubeconfig=/var/lib/rancher/k3s/agent/kubelet.kubeconfig\n" +
		"/usr/local/bin/k3s kubectl --kubeconfig \"$kubeconfig\" get node " + shellQuote(name) + " -o name >/dev/null\n" +
		"proof=$(mktemp); trap 'rm -f \"$proof\"' EXIT\n" +
		"if /usr/local/bin/k3s kubectl --kubeconfig \"$kubeconfig\" label node " + shellQuote(name) + " felis.node-restriction.kubernetes.io/probe=controller --overwrite 2>\"$proof\"; then echo 'NodeRestriction failed' >&2;exit 1;fi\ngrep -qi forbidden \"$proof\"\n" +
		"image=" + shellQuote(image) + "\nif [ -n \"$(/usr/local/bin/k3s crictl images -q \"$image\")\" ]; then /usr/local/bin/k3s crictl rmi \"$image\" >/dev/null; fi\ntest -z \"$(/usr/local/bin/k3s crictl images -q \"$image\")\"\n/usr/local/bin/k3s crictl pull \"$image\" >/dev/null\n"
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10", "--", remote, "if [ \"$(id -u)\" = 0 ]; then bash -s; else sudo -n bash -s; fi")
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("worker host, NodeRestriction or uncached registry pull check failed: %w", err)
	}
	id := "felis-probe-" + archivetransfer.ID()[:12]
	var objects []client.Object
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for i := len(objects) - 1; i >= 0; i-- {
			cl.Delete(cleanup, objects[i])
		}
	}()
	create := func(o client.Object) error {
		if err := cl.Create(ctx, o); err != nil {
			return err
		}
		objects = append(objects, o)
		return nil
	}
	var endpoints []string
	var echoPods []*corev1.Pod
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !placement.Online(node) || (node.Name != name && node.Name != controller.Name && node.Labels[placement.LabelApproved] != "true") {
			continue
		}
		echoName := fmt.Sprintf("%s-%d", id, i)
		p := probePod(echoName, ns, node.Name, image, []string{"--listen", ":25565"}, true)
		p.Labels["felis.lolicon.best/probe-echo"] = id
		if err := create(p); err != nil {
			return err
		}
		echoPods = append(echoPods, p)
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: echoName, Namespace: ns}, Spec: corev1.ServiceSpec{Selector: map[string]string{"felis.lolicon.best/probe-name": echoName}, Ports: []corev1.ServicePort{{Port: 25565, TargetPort: intstr.FromInt32(25565)}}}}
		if err := create(svc); err != nil {
			return err
		}
		endpoints = append(endpoints, net.JoinHostPort(svc.Spec.ClusterIP, "25565"))
	}
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(25565)
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: ns}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"felis.lolicon.best/probe-echo": id}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"felis.lolicon.best/probe-source": id}}}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}}}}}
	for _, cidr := range aCIDRs {
		policy.Spec.Ingress[0].From = append(policy.Spec.Ingress[0].From, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	if err := create(policy); err != nil {
		return err
	}
	for _, p := range echoPods {
		if err := waitProbePod(ctx, cl, p); err != nil {
			return err
		}
	}
	open := append([]string{}, endpoints...)
	open = append(open, net.JoinHostPort(apiSvc.Spec.ClusterIP, "443"), net.JoinHostPort(registrySvc.Spec.ClusterIP, "5000"), net.JoinHostPort(archiveSvc.Spec.ClusterIP, "8090"))
	// Positive probes need temporary, narrowly scoped ingress grants where the
	// production fence admits only the controller or archive-transfer jobs.
	for _, svc := range []*corev1.Service{&apiSvc, &registrySvc, &archiveSvc} {
		if len(svc.Spec.Selector) == 0 || len(svc.Spec.Ports) == 0 {
			return fmt.Errorf("probe Service %s has no backend", svc.Name)
		}
		targetPort := svc.Spec.Ports[0].TargetPort
		if targetPort.IntVal == 0 && targetPort.StrVal == "" {
			targetPort = intstr.FromInt32(svc.Spec.Ports[0].Port)
		}
		allow := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: id + "-" + svc.Name, Namespace: svc.Namespace}, Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: svc.Spec.Selector}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"felis.lolicon.best/probe-source": id}},
			}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &targetPort}}}},
		}}
		if err := create(allow); err != nil {
			return err
		}
	}
	positive := probeJob(id+"-positive", ns, name, image, probeArgs("--open", open), false)
	positive.Spec.Template.Labels["felis.lolicon.best/probe-source"] = id
	if err := create(positive); err != nil {
		return err
	}
	if err := waitProbeJob(ctx, cl, positive); err != nil {
		return err
	}
	denied = append(denied, open...)
	denied = append(denied, "169.254.169.254:80", "169.254.170.2:80")
	negative := probeJob(id+"-negative", ns, name, image, probeArgs("--closed", denied), true)
	negative.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "egress-gate", Image: image, Command: []string{"/usr/local/bin/felis", "egress-gate", "--probe", net.JoinHostPort(apiSvc.Spec.ClusterIP, "443"), "--wait", "2m"}, SecurityContext: negative.Spec.Template.Spec.Containers[0].SecurityContext}}
	if err := create(negative); err != nil {
		return err
	}
	if err := waitProbeJob(ctx, cl, negative); err != nil {
		return err
	}
	// Dial from Velocity's host namespace and record the address actually observed inside each backend.
	for _, endpoint := range endpoints {
		c, err := net.DialTimeout("tcp", endpoint, 5*time.Second)
		if err != nil {
			return fmt.Errorf("velocity host cannot dial backend Service %s: %w", endpoint, err)
		}
		c.Close()
	}
	var observed []string
	time.Sleep(500 * time.Millisecond)
	for _, p := range echoPods {
		foundA := false
		stream, err := cs.CoreV1().Pods(ns).GetLogs(p.Name, &corev1.PodLogOptions{}).Stream(ctx)
		if err != nil {
			return err
		}
		scan := bufio.NewScanner(stream)
		for scan.Scan() {
			line := scan.Text()
			if strings.HasPrefix(line, "felis-probe-source ") {
				host, _, err := net.SplitHostPort(strings.TrimPrefix(line, "felis-probe-source "))
				if err == nil {
					cidr, _ := exactCIDR(host)
					for _, a := range aCIDRs {
						if cidr == a {
							observed = append(observed, cidr)
							foundA = true
						}
					}
				}
			}
		}
		if !foundA {
			stream.Close()
			return fmt.Errorf("backend %s did not observe A as an exact source", p.Name)
		}
		err = scan.Err()
		stream.Close()
		if err != nil {
			return err
		}
	}
	if len(observed) < len(echoPods) {
		return fmt.Errorf("backend observed a source outside A's exact interfaces")
	}
	var game networkingv1.NetworkPolicy
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: "felis-allow-game-from-velocity"}, &game); err != nil {
		return err
	}
	if len(game.Spec.Ingress) == 0 {
		return fmt.Errorf("velocity ingress policy is not configured")
	}
	prevGame := game.DeepCopy()
	for _, cidr := range observed {
		exists := false
		for _, peer := range game.Spec.Ingress[0].From {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == cidr {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		game.Spec.Ingress[0].From = append(game.Spec.Ingress[0].From, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	if err := cl.Patch(ctx, &game, client.MergeFrom(prevGame)); err != nil {
		return err
	}
	// Read fresh resourceVersion so concurrent node health writes cannot be overwritten.
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		return err
	}
	if !placement.Online(&n) {
		return fmt.Errorf("worker became offline during validation")
	}
	before = n.DeepCopy()
	n.Labels[placement.LabelIdentity] = name
	n.Labels[placement.LabelRole] = placement.RoleWorker
	n.Labels[placement.LabelApproved] = "true"
	taints := n.Spec.Taints[:0]
	for _, t := range n.Spec.Taints {
		if t.Key != "felis.lolicon.best/unapproved" {
			taints = append(taints, t)
		}
	}
	n.Spec.Taints = taints
	if err := cl.Patch(ctx, &n, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "approved worker", name, "Velocity sources", strings.Join(observed, ","))
	return nil
}

func probeArgs(flag string, addresses []string) []string {
	var args []string
	for _, a := range addresses {
		args = append(args, flag, a)
	}
	return args
}
func probePod(name, ns, node, image string, args []string, game bool) *corev1.Pod {
	no, yes := false, true
	uid := int64(1000)
	labels := map[string]string{"felis.lolicon.best/probe-name": name}
	if game {
		labels[v1alpha1.LabelManagedBy] = operator.ManagedByValue
		labels[v1alpha1.LabelComponent] = operator.ComponentValue
		labels[v1alpha1.LabelServer] = name
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}, Spec: corev1.PodSpec{NodeName: node, RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: &no, SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &yes, RunAsUser: &uid, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}, Containers: []corev1.Container{{Name: "probe", Image: image, ImagePullPolicy: corev1.PullAlways, Command: []string{"/usr/local/bin/felis", "node-probe"}, Args: args, Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi"), corev1.ResourceCPU: resource.MustParse("200m")}}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}}}}
}
func probeJob(name, ns, node, image string, args []string, game bool) *batchv1.Job {
	p := probePod(name, ns, node, image, args, game)
	zero := int32(0)
	deadline := int64(600)
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: batchv1.JobSpec{BackoffLimit: &zero, ActiveDeadlineSeconds: &deadline, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: p.Labels}, Spec: p.Spec}}}
}
func waitProbePod(ctx context.Context, cl client.Client, p *corev1.Pod) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if err := cl.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
			return err
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return nil
			}
		}
		if p.Status.Phase == corev1.PodFailed {
			return fmt.Errorf("probe Pod %s failed", p.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
func waitProbeJob(ctx context.Context, cl client.Client, j *batchv1.Job) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if err := cl.Get(ctx, client.ObjectKeyFromObject(j), j); err != nil {
			return err
		}
		for _, c := range j.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				continue
			}
			if c.Type == batchv1.JobComplete {
				return nil
			}
			if c.Type == batchv1.JobFailed {
				return fmt.Errorf("admission probe Job %s failed; inspect its Pod log", j.Name)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func denyNodeAddresses(ctx context.Context, cl client.Client, ns string, nodes []corev1.Node) error {
	var np networkingv1.NetworkPolicy
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: "felis-server-egress"}, &np); err != nil {
		return err
	}
	before := np.DeepCopy()
	for _, n := range nodes {
		for _, a := range n.Status.Addresses {
			if a.Type != corev1.NodeInternalIP && a.Type != corev1.NodeExternalIP {
				continue
			}
			cidr, err := exactCIDR(a.Address)
			if err != nil {
				return err
			}
			for i := range np.Spec.Egress {
				for k := range np.Spec.Egress[i].To {
					block := np.Spec.Egress[i].To[k].IPBlock
					if block != nil && ((strings.Contains(cidr, ":") && block.CIDR == "::/0") || (!strings.Contains(cidr, ":") && block.CIDR == "0.0.0.0/0")) {
						block.Except = append(block.Except, cidr)
					}
				}
			}
		}
	}
	return cl.Patch(ctx, &np, client.MergeFrom(before))
}
