package main

import (
	"context"

	"felis.lolicon.best/internal/nodecontrol"
	"felis.lolicon.best/internal/placement"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// freeMemory reports what each online, schedulable node can still promise a new server.
func (e nodeExecutor) freeMemory(ctx context.Context) ([]nodecontrol.NodeMemory, error) {
	nodes, err := e.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	pods, err := e.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return unrequestedMemory(nodes.Items, pods.Items), nil
}

// unrequestedMemory subtracts what the live pods bound to each node request from its allocatable memory, the sum the scheduler admits a pod against.
func unrequestedMemory(nodes []corev1.Node, pods []corev1.Pod) []nodecontrol.NodeMemory {
	requested := map[string]int64{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		requested[p.Spec.NodeName] += podMemoryRequest(p)
	}
	out := []nodecontrol.NodeMemory{}
	for i := range nodes {
		n := &nodes[i]
		if n.Spec.Unschedulable || !placement.Online(n) {
			continue
		}
		out = append(out, nodecontrol.NodeMemory{Node: n.Name, Bytes: max(n.Status.Allocatable.Memory().Value()-requested[n.Name], 0)})
	}
	return out
}

// podMemoryRequest is the larger of the containers' summed requests and any single init container's, plus the pod overhead.
func podMemoryRequest(p *corev1.Pod) int64 {
	var sum int64
	for _, c := range p.Spec.Containers {
		sum += c.Resources.Requests.Memory().Value()
	}
	for _, c := range p.Spec.InitContainers {
		sum = max(sum, c.Resources.Requests.Memory().Value())
	}
	return sum + p.Spec.Overhead.Memory().Value()
}
