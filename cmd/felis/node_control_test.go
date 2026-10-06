package main

import (
	"context"
	"strings"
	"testing"

	"felis.lolicon.best/internal/nodecontrol"
	"felis.lolicon.best/internal/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNodeControlTopologyAndBootstrapID(t *testing.T) {
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "controller", Labels: map[string]string{"node-role.kubernetes.io/control-plane": "true"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.1"}}}}
	controller, peers, err := nodeController([]corev1.Node{node}, "192.0.2.2", []string{"192.0.2.1/32"})
	if err != nil || controller.Name != "controller" || strings.Join(peers, ",") != "192.0.2.1/32,192.0.2.2/32" {
		t.Fatal(controller, peers, err)
	}
	duplicate := node
	duplicate.Name = "second"
	if _, _, err = nodeController([]corev1.Node{node, duplicate}, "", nil); err == nil {
		t.Fatal("multiple controllers accepted")
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
