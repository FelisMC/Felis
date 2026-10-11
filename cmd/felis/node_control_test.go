package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"felis.lolicon.best/internal/distributed"
	"felis.lolicon.best/internal/nodecontrol"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNodeControlTopologyAndBootstrapID(t *testing.T) {
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "controller", Labels: map[string]string{"node-role.kubernetes.io/control-plane": "true"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.1"}}}}
	controller, peers, err := nodeController([]corev1.Node{node}, "192.0.2.2", []string{"192.0.2.1/32"}, "elsewhere")
	if err != nil || controller.Name != "controller" || strings.Join(peers, ",") != "192.0.2.1/32,192.0.2.2/32" {
		t.Fatal(controller, peers, err)
	}
	// With controller high availability a task acts from the controller it runs on, else
	// from the first one online; one is needed online.
	second := *node.DeepCopy()
	second.Name = "second"
	second.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.3"}}
	if c, peers, err := nodeController([]corev1.Node{node, second}, "", nil, "second"); err != nil || c.Name != "second" || len(peers) != 2 {
		t.Fatalf("from second = %v %v (%v)", c, peers, err)
	}
	if c, _, err := nodeController([]corev1.Node{second, node}, "", nil, "worker-x"); err != nil || c.Name != "controller" {
		t.Fatalf("from a worker = %v (%v)", c, err)
	}
	offline := *node.DeepCopy()
	offline.Status.Conditions = nil
	if c, _, err := nodeController([]corev1.Node{offline, second}, "", nil, "controller"); err != nil || c.Name != "second" {
		t.Fatalf("with this controller offline = %v (%v)", c, err)
	}
	if _, _, err := nodeController([]corev1.Node{offline}, "", nil, "controller"); err == nil {
		t.Fatal("no controller online was accepted")
	}
	for _, token := range []string{"abcdef.0123456789abcdef", "K10hash::abcdef.0123456789abcdef\n"} {
		if bootstrapTokenID(token) != "abcdef" {
			t.Fatal("token secret passed to revocation")
		}
	}
}
func TestNodeControlRejectsActiveWorkloadsBeforeHostCommands(t *testing.T) {
	replicas := int32(1)
	set := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft"}, Spec: appsv1.StatefulSetSpec{Replicas: &replicas}}
	executor := nodeExecutor{cs: fake.NewSimpleClientset(set), namespace: platform.DefaultMinecraftNamespace}
	if err := executor.requireStopped(context.Background(), nodecontrol.Request{Action: "enable"}); err == nil {
		t.Fatal("host changes permitted with active worlds")
	}
}

// TestNodeControlRefusedApprovalLeavesSchedulingAlone pins that an approval refused before
// admission runs (here: a server still on the node) lifts the quarantine it added, so the
// node's servers can still start; a node that was already cordoned stays cordoned.
func TestNodeControlRefusedApprovalLeavesSchedulingAlone(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cordoned    bool
		wantBlocked bool
	}{
		{"a schedulable node is schedulable again", false, false},
		{"an operator's cordon is kept", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replicas := int32(1)
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-b", Labels: map[string]string{placement.LabelRole: placement.RoleWorker}},
				Spec:       corev1.NodeSpec{Unschedulable: tc.cordoned},
			}
			set := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: platform.DefaultMinecraftNamespace},
				Spec: appsv1.StatefulSetSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{NodeSelector: map[string]string{placement.LabelIdentity: "worker-b"}},
				}},
			}
			cs := fake.NewSimpleClientset(node, set)
			executor := nodeExecutor{cs: cs, namespace: platform.DefaultMinecraftNamespace}
			err := executor.run(context.Background(), nodecontrol.Request{Action: "approve", Name: "worker-b"},
				func(string) error { return nil }, io.Discard)
			if err == nil {
				t.Fatal("approval accepted with a server still on the node")
			}
			got, err := cs.CoreV1().Nodes().Get(context.Background(), "worker-b", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Spec.Unschedulable != tc.wantBlocked {
				t.Errorf("unschedulable = %v, want %v", got.Spec.Unschedulable, tc.wantBlocked)
			}
			if _, ok := got.Annotations["felis.lolicon.best/node-control-cordon"]; ok {
				t.Error("the task's quarantine marker was left behind")
			}
		})
	}
}

// TestNodeControlUpgradeSendsTheControllersBuild pins what updating a worker does over SSH:
// the controller's own felis binary goes across, the worker is told the controller's k3s
// version, address and current peers, the scratch directory is removed afterwards, and the
// node records the felis build it now runs. A controller or unknown node is refused.
func TestNodeControlUpgradeSendsTheControllersBuild(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	stub := "#!/bin/sh\nprintf 'ARGS %s\\n' \"$*\" >> " + shellQuote(logPath) + "\ncat >> " + shellQuote(logPath) + "\nprintf '\\n' >> " + shellQuote(logPath) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	binary := filepath.Join(dir, "felis")
	if err := os.WriteFile(binary, []byte("FELIS-BINARY-BYTES"), 0o700); err != nil {
		t.Fatal(err)
	}
	ready := corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}
	controller := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a", Labels: map[string]string{placement.LabelRole: placement.RoleController, placement.LabelIdentity: "a"}}, Status: ready}
	controller.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "203.0.113.10"}}
	controller.Status.NodeInfo.KubeletVersion = "v1.36.4+k3s1"
	worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-b", Labels: map[string]string{placement.LabelRole: placement.RoleWorker, placement.LabelIdentity: "worker-b"}}, Status: ready}
	cs := fake.NewSimpleClientset(controller, worker)
	e := nodeExecutor{cs: cs, binary: binary}
	nodes := []corev1.Node{*controller, *worker}
	r := nodecontrol.Request{Action: "upgrade", Name: "worker-b", SSHTarget: "root@worker-b", ConfirmMaintenance: true}
	var stages []string
	stage := func(s string) error { stages = append(stages, s); return nil }

	if err := e.upgrade(context.Background(), r, nodes, controller, []string{"192.0.2.20/32", "203.0.113.10/32"}, stage, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	for _, want := range []string{
		"FELIS-BINARY-BYTES",
		"export FELIS_K3S_VERSION='v1.36.4+k3s1'",
		"node upgrade --name 'worker-b' --server 'https://203.0.113.10:6443' --peers '192.0.2.20/32,203.0.113.10/32'",
		"rm -rf -- '/var/tmp/felis-node-upgrade-worker-b'",
		"ARGS -o BatchMode=yes",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the SSH session lacks %q:\n%s", want, log)
		}
	}
	// The last session removes the scratch directory holding the binary.
	if !strings.HasSuffix(log, "rm -rf -- '/var/tmp/felis-node-upgrade-worker-b'\n") {
		t.Errorf("the scratch directory is not removed last:\n%s", log)
	}
	if got := strings.Join(stages, ","); got != "transfer,upgrade_worker,wait_ready,complete" {
		t.Errorf("stages = %s", got)
	}
	updated, err := cs.CoreV1().Nodes().Get(context.Background(), "worker-b", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Annotations[placement.AnnotationFelisVersion] != version {
		t.Errorf("the node records felis %q, want %q", updated.Annotations[placement.AnnotationFelisVersion], version)
	}

	for _, name := range []string{"a", "nobody"} {
		r.Name = name
		if err := e.upgrade(context.Background(), r, nodes, controller, nil, stage, io.Discard); err == nil {
			t.Errorf("an update of %s was accepted", name)
		}
	}
}

func fenceTestNode(name, role string, ready bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{placement.LabelRole: role, placement.LabelIdentity: name, placement.LabelApproved: "true"}}}
	if ready {
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	}
	return n
}

// TestNodeControlFenceReleasesALostWorker pins the fence: the lost worker is cordoned,
// loses its approval and gains the out-of-service taint once however often it runs, and
// the pods recorded on it are deleted at once while other nodes' pods stay. An online
// worker or a controller is refused untouched.
func TestNodeControlFenceReleasesALostWorker(t *testing.T) {
	ctx := context.Background()
	ns := platform.DefaultMinecraftNamespace
	onLost := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "alice-0", Namespace: ns}, Spec: corev1.PodSpec{NodeName: "worker-b"}}
	elsewhere := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "carol-0", Namespace: ns}, Spec: corev1.PodSpec{NodeName: "worker-c"}}
	lost := fenceTestNode("worker-b", placement.RoleWorker, false)
	// The same key with another effect is no fence.
	lost.Spec.Taints = []corev1.Taint{{Key: distributed.TaintOutOfService, Effect: corev1.TaintEffectNoSchedule}}
	cs := fake.NewSimpleClientset(lost, onLost, elsewhere)
	e := nodeExecutor{cs: cs, namespace: ns}
	var stages []string
	for range 2 {
		stages = nil
		if err := e.run(ctx, nodecontrol.Request{Action: "fence", Name: "worker-b"}, func(s string) error { stages = append(stages, s); return nil }, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(stages, ",") != "fence,release_pods,complete" {
		t.Errorf("stages = %v", stages)
	}
	node, err := cs.CoreV1().Nodes().Get(ctx, "worker-b", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	taints := 0
	for _, taint := range node.Spec.Taints {
		if taint.Key == distributed.TaintOutOfService && taint.Effect == corev1.TaintEffectNoExecute {
			taints++
		}
	}
	if !node.Spec.Unschedulable || node.Labels[placement.LabelApproved] != "" || taints != 1 || node.Annotations[distributed.AnnotationFenced] == "" || !distributed.Fenced(node) {
		t.Fatalf("fenced node = %+v", node)
	}
	pods, _ := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 1 || pods.Items[0].Name != "carol-0" {
		t.Fatalf("pods left = %v", pods.Items)
	}

	for name, n := range map[string]*corev1.Node{
		"online worker": fenceTestNode("worker-b", placement.RoleWorker, true),
		"controller":    fenceTestNode("worker-b", placement.RoleController, false),
	} {
		cs := fake.NewSimpleClientset(n, onLost.DeepCopy())
		e := nodeExecutor{cs: cs, namespace: ns}
		if err := e.run(ctx, nodecontrol.Request{Action: "fence", Name: "worker-b"}, func(string) error { return nil }, io.Discard); err == nil {
			t.Errorf("%s: fenced", name)
		}
		got, _ := cs.CoreV1().Nodes().Get(ctx, "worker-b", metav1.GetOptions{})
		if got.Spec.Unschedulable || len(got.Spec.Taints) != 0 || got.Labels[placement.LabelApproved] != "true" {
			t.Errorf("%s: a refused fence changed the node: %+v", name, got)
		}
		if pods, _ := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{}); len(pods.Items) != 1 {
			t.Errorf("%s: a refused fence deleted pods", name)
		}
	}
}

// TestNodeControlApprovalLiftsTheFence pins that approving a fenced worker back takes the
// out-of-service taint and the fence mark off once it is online, and refuses while it is
// still offline, leaving the fence in place.
func TestNodeControlApprovalLiftsTheFence(t *testing.T) {
	ctx := context.Background()
	fenced := func(ready bool) *corev1.Node {
		n := fenceTestNode("worker-b", placement.RoleWorker, ready)
		delete(n.Labels, placement.LabelApproved)
		n.Spec.Unschedulable = true
		n.Annotations = map[string]string{distributed.AnnotationFenced: "2026-10-12T08:00:00Z"}
		n.Spec.Taints = []corev1.Taint{{Key: "example.com/kept", Effect: corev1.TaintEffectNoSchedule}, {Key: distributed.TaintOutOfService, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute}}
		return n
	}
	replicas := int32(1)
	set := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: platform.DefaultMinecraftNamespace},
		Spec: appsv1.StatefulSetSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{NodeSelector: map[string]string{placement.LabelIdentity: "worker-b"}},
		}},
	}
	cs := fake.NewSimpleClientset(fenced(true), set)
	e := nodeExecutor{cs: cs, namespace: platform.DefaultMinecraftNamespace}
	// The approval itself is refused further on (a server still asks for the node): the
	// fence is lifted before that.
	_ = e.run(ctx, nodecontrol.Request{Action: "approve", Name: "worker-b"}, func(string) error { return nil }, io.Discard)
	node, _ := cs.CoreV1().Nodes().Get(ctx, "worker-b", metav1.GetOptions{})
	if len(node.Spec.Taints) != 1 || node.Spec.Taints[0].Key != "example.com/kept" || node.Annotations[distributed.AnnotationFenced] != "" {
		t.Fatalf("after approving an online fenced node: taints %v, annotations %v", node.Spec.Taints, node.Annotations)
	}
	if !node.Spec.Unschedulable {
		t.Error("lifting the fence uncordoned the node before it was approved")
	}

	cs = fake.NewSimpleClientset(fenced(false))
	e = nodeExecutor{cs: cs, namespace: platform.DefaultMinecraftNamespace}
	if err := e.run(ctx, nodecontrol.Request{Action: "approve", Name: "worker-b"}, func(string) error { return nil }, io.Discard); err == nil || !strings.Contains(err.Error(), "still offline") {
		t.Fatalf("approving an offline fenced node = %v", err)
	}
	if node, _ := cs.CoreV1().Nodes().Get(ctx, "worker-b", metav1.GetOptions{}); !distributed.Fenced(node) {
		t.Fatal("a refused approval lifted the fence")
	}
}

// TestNodeControlJoinControllerSendsTheClusterAndLabels pins adding a controller: every
// node's firewall learns it first (workers and the other controllers over SSH, this one in
// place), its secrets travel only over stdin, the installer's controller-join mode runs
// with the cluster's coordinates, and the ready node is labelled a controller and a home
// for Longhorn's replicas. Without embedded etcd, or for an existing node, it is refused.
func TestNodeControlJoinControllerSendsTheClusterAndLabels(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	stub := "#!/bin/sh\nprintf 'ARGS %s\\n' \"$*\" >> " + shellQuote(logPath) + "\ncat >> " + shellQuote(logPath) + "\nprintf '\\n' >> " + shellQuote(logPath) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	localLog := filepath.Join(dir, "local.log")
	binary := filepath.Join(dir, "felis")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'LOCAL %s\\n' \"$*\" >> "+shellQuote(localLog)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	serverToken, agentToken := write("server-token", "K10SERVER-TOKEN"), write("agent-token", "AGENT-TOKEN")
	write("etc/felis/felis.host.toml", "[database]\n")
	etcd := filepath.Join(dir, "etcd")
	ready := corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}
	mk := func(name, role, ip string) *corev1.Node {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{placement.LabelRole: role, placement.LabelIdentity: name}}, Status: *ready.DeepCopy()}
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: ip}}
		return n
	}
	local, other, worker := mk("a", placement.RoleController, "203.0.113.10"), mk("ctl-c", placement.RoleController, "203.0.113.12"), mk("worker-w", placement.RoleWorker, "192.0.2.20")
	local.Status.NodeInfo.KubeletVersion = "v1.36.4+k3s1"
	joined := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ctl-b"}, Status: *ready.DeepCopy()}
	registry := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: "felis"}, Spec: corev1.ServiceSpec{ClusterIP: "10.43.0.50"}}
	cs := fake.NewSimpleClientset(local, other, worker, joined, registry)
	e := nodeExecutor{cs: cs, binary: binary, namespace: "minecraft", controlNamespace: "felis", serverToken: serverToken, agentToken: agentToken, etcDir: filepath.Join(dir, "etc", "felis"), etcdDir: etcd}
	nodes := []corev1.Node{*local, *other, *worker}
	r := nodecontrol.Request{Action: "join-controller", Name: "ctl-b", SSHTarget: "root@ctl-b", ExternalIP: "203.0.113.11", ConfirmMaintenance: true}
	peers := []string{"192.0.2.20/32", "203.0.113.10/32", "203.0.113.11/32", "203.0.113.12/32"}
	stage := func(string) error { return nil }

	if err := e.joinController(context.Background(), r, nodes, local, peers, stage, io.Discard); err == nil || !strings.Contains(err.Error(), "etcd") {
		t.Fatalf("without etcd = %v", err)
	}
	if err := os.Mkdir(etcd, 0o700); err != nil {
		t.Fatal(err)
	}
	var stages []string
	stage = func(s string) error { stages = append(stages, s); return nil }
	if err := e.joinController(context.Background(), r, nodes, local, peers, stage, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(logPath)
	log := string(raw)
	controllers := "'203.0.113.10/32,203.0.113.11/32,203.0.113.12/32'"
	for _, want := range []string{
		"ARGS -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=10 -- worker-w",
		"node firewall --controller-ip '203.0.113.10' --peers '192.0.2.20/32,203.0.113.10/32,203.0.113.11/32,203.0.113.12/32' --controller-peers " + controllers,
		"-- ctl-c",
		"node firewall --controller --controller-ip '203.0.113.12'",
		"K10SERVER-TOKEN", "AGENT-TOKEN", "felis.host.toml",
		"node join-controller --name 'ctl-b' --server 'https://203.0.113.10:6443' --external-ip '203.0.113.11'",
		"--registry-ip '10.43.0.50'",
		"--controller-peers " + controllers,
		"export FELIS_K3S_VERSION='v1.36.4+k3s1'",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the SSH sessions lack %q", want)
		}
	}
	for _, secret := range []string{"K10SERVER-TOKEN", "AGENT-TOKEN"} {
		for _, line := range strings.Split(log, "\n") {
			if strings.HasPrefix(line, "ARGS ") && strings.Contains(line, secret) {
				t.Errorf("%s travelled in a command line: %s", secret, line)
			}
		}
	}
	if !strings.Contains(log[strings.LastIndex(log, "ARGS"):], "rm -rf -- '/var/tmp/felis-node-felis-controller-join-") {
		t.Error("the scratch directory is not removed last")
	}
	if rawLocal, _ := os.ReadFile(localLog); !strings.Contains(string(rawLocal), "LOCAL node firewall --controller --controller-ip 203.0.113.10") || !strings.Contains(string(rawLocal), "--controller-peers 203.0.113.10/32,203.0.113.11/32,203.0.113.12/32") {
		t.Errorf("this controller's firewall = %s", rawLocal)
	}
	if got := strings.Join(stages, ","); got != "firewall,transfer,install_controller,wait_ready,label,complete" {
		t.Errorf("stages = %s", got)
	}
	labelled, _ := cs.CoreV1().Nodes().Get(context.Background(), "ctl-b", metav1.GetOptions{})
	if labelled.Labels[placement.LabelRole] != placement.RoleController || labelled.Labels[placement.LabelIdentity] != "ctl-b" || labelled.Labels[longhornDiskLabel] != "true" || labelled.Annotations[placement.AnnotationFelisVersion] != version {
		t.Errorf("joined node = %+v", labelled.ObjectMeta)
	}

	r.Name = "ctl-c"
	if err := e.joinController(context.Background(), r, nodes, local, peers, stage, io.Discard); err == nil {
		t.Error("an existing node was joined again")
	}
}

// TestNodeControlFencesAControllerFromAnother pins fencing a lost controller: from another
// controller only, and never the last one standing.
func TestNodeControlFencesAControllerFromAnother(t *testing.T) {
	ctx := context.Background()
	run := func(e nodeExecutor) error {
		return e.run(ctx, nodecontrol.Request{Action: "fence", Name: "ctl-a"}, func(string) error { return nil }, io.Discard)
	}
	lost := fenceTestNode("ctl-a", placement.RoleController, false)
	cs := fake.NewSimpleClientset(lost, fenceTestNode("ctl-b", placement.RoleController, true))
	if err := run(nodeExecutor{cs: cs, namespace: "minecraft", self: "ctl-a"}); err == nil {
		t.Error("a controller fenced itself")
	}
	if err := run(nodeExecutor{cs: cs, namespace: "minecraft", self: "ctl-b"}); err != nil {
		t.Fatalf("fencing from ctl-b = %v", err)
	}
	if n, _ := cs.CoreV1().Nodes().Get(ctx, "ctl-a", metav1.GetOptions{}); !n.Spec.Unschedulable {
		t.Error("the lost controller was not fenced")
	}
	alone := fake.NewSimpleClientset(fenceTestNode("ctl-a", placement.RoleController, false), fenceTestNode("ctl-b", placement.RoleController, false), fenceTestNode("worker-w", placement.RoleWorker, true))
	if err := run(nodeExecutor{cs: alone, namespace: "minecraft", self: "ctl-c"}); err == nil || !strings.Contains(err.Error(), "last controller") {
		t.Errorf("fencing the last controller = %v", err)
	}
}

// TestNodeControlUnfencesAReturnedController pins unfence: a fenced controller that is
// online again gets its taint, mark and cordon lifted; a fenced worker is sent to approval,
// and a controller still offline or never fenced is refused, each left untouched.
func TestNodeControlUnfencesAReturnedController(t *testing.T) {
	ctx := context.Background()
	fenced := func(name, role string, ready bool) *corev1.Node {
		n := fenceTestNode(name, role, ready)
		n.Spec.Unschedulable = true
		n.Spec.Taints = []corev1.Taint{{Key: distributed.TaintOutOfService, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute}}
		n.Annotations = map[string]string{distributed.AnnotationFenced: "2026-10-01T00:00:00Z"}
		return n
	}
	cs := fake.NewSimpleClientset(fenced("ctl-a", placement.RoleController, true), fenced("ctl-down", placement.RoleController, false),
		fenced("worker-b", placement.RoleWorker, true), fenceTestNode("ctl-c", placement.RoleController, true))
	e := nodeExecutor{cs: cs, namespace: "minecraft", self: "ctl-c"}
	unfence := func(name string) error {
		return e.run(ctx, nodecontrol.Request{Action: "unfence", Name: name}, func(string) error { return nil }, io.Discard)
	}
	if err := unfence("ctl-a"); err != nil {
		t.Fatalf("unfence ctl-a = %v", err)
	}
	n, _ := cs.CoreV1().Nodes().Get(ctx, "ctl-a", metav1.GetOptions{})
	if n.Spec.Unschedulable || len(n.Spec.Taints) != 0 || n.Annotations[distributed.AnnotationFenced] != "" {
		t.Errorf("ctl-a after unfence: unschedulable=%v taints=%v annotations=%v", n.Spec.Unschedulable, n.Spec.Taints, n.Annotations)
	}
	for name, want := range map[string]string{"worker-b": "approval", "ctl-down": "controller is still offline", "ctl-c": "not fenced"} {
		before, _ := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err := unfence(name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("unfence %s = %v, want an error about %q", name, err, want)
		}
		if after, _ := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{}); !reflect.DeepEqual(before, after) {
			t.Errorf("unfence %s changed the node", name)
		}
	}
}

// TestNodeControlEnableHANeedsDistributedMode pins that HA is refused, before any backup
// or change, on a controller distributed mode has not labelled.
func TestNodeControlEnableHANeedsDistributedMode(t *testing.T) {
	unlabelled := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	var stages []string
	err := nodeExecutor{}.enableHA(context.Background(), nodecontrol.Request{Action: "enable-ha"}, unlabelled, nil, func(s string) error { stages = append(stages, s); return nil }, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "distributed mode first") || len(stages) != 0 {
		t.Fatalf("enable-ha on a plain controller = %v (stages %v)", err, stages)
	}
}

// TestLocalControllerPicksThisHost pins which controller admission acts as: the online one
// holding one of this host's addresses, whichever of several it is.
func TestLocalControllerPicksThisHost(t *testing.T) {
	mk := func(name, ip string, ready bool) *corev1.Node {
		n := fenceTestNode(name, placement.RoleController, ready)
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}
		return n
	}
	a, b, down := mk("a", "192.0.2.1", true), mk("b", "192.0.2.2", true), mk("c", "192.0.2.3", false)
	if got := localController([]*corev1.Node{a, b, down}, []string{"192.0.2.2/32", "10.42.0.1/32"}); got != b {
		t.Errorf("local = %v, want b", got)
	}
	if got := localController([]*corev1.Node{a, b, down}, []string{"192.0.2.3/32"}); got != nil {
		t.Errorf("an offline controller was picked: %v", got.Name)
	}
	if got := localController([]*corev1.Node{a, b}, []string{"198.51.100.9/32"}); got != nil {
		t.Errorf("a controller was picked on a host that is none: %v", got.Name)
	}
}
