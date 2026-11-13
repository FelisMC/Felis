package main

import (
	"testing"

	"felis.lolicon.best/internal/nodecontrol"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUnrequestedMemoryOnASmallHost(t *testing.T) {
	node := func(name string, ready, cordoned bool) corev1.Node {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       corev1.NodeSpec{Unschedulable: cordoned},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			},
		}
	}
	request := func(q string) corev1.Container {
		return corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(q)}}}
	}
	pod := func(node string, phase corev1.PodPhase, containers ...corev1.Container) corev1.Pod {
		return corev1.Pod{Spec: corev1.PodSpec{NodeName: node, Containers: containers}, Status: corev1.PodStatus{Phase: phase}}
	}
	starting := pod("host", corev1.PodPending, request("64Mi"))
	starting.Spec.InitContainers = []corev1.Container{request("256Mi")}

	got := unrequestedMemory(
		[]corev1.Node{node("host", true, false), node("cordoned", true, true), node("lost", false, false)},
		[]corev1.Pod{
			pod("host", corev1.PodRunning, request("512Mi")),                   // login space
			pod("host", corev1.PodRunning, request("1Gi")),                     // lobby
			pod("host", corev1.PodRunning, request("256Mi"), request("128Mi")), // control plane
			starting,
			pod("host", corev1.PodSucceeded, request("1Gi")),
			pod("", corev1.PodPending, request("4Gi")),
		},
	)
	// The scheduler counts the larger of a pod's containers and its biggest init container: 4096 - 512 - 1024 - 384 - 256 MiB.
	want := []nodecontrol.NodeMemory{{Node: "host", Bytes: 1920 << 20}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("unrequestedMemory = %+v, want %+v (only the online, schedulable node)", got, want)
	}
}
