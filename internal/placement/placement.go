// Package placement resolves worlds and checks the protected node admission labels.
package placement

import (
	"context"
	"fmt"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	LabelRole      = "felis.node-restriction.kubernetes.io/role"
	LabelIdentity  = "felis.node-restriction.kubernetes.io/identity"
	LabelApproved  = "felis.node-restriction.kubernetes.io/approved"
	RoleController = "controller"
	RoleWorker     = "worker"
)

type World struct{ Claim, Node string }
type Resolver func(context.Context, string) (World, error)

func Online(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue && n.DeletionTimestamp == nil
		}
	}
	return false
}

// Admitted accepts only A's protected controller identity or an approved worker.
func Admitted(n *corev1.Node, controller string) bool {
	if !Online(n) || n.Labels[LabelIdentity] != n.Name {
		return false
	}
	if n.Name == controller {
		return n.Labels[LabelRole] == RoleController
	}
	return n.Labels[LabelRole] == RoleWorker && n.Labels[LabelApproved] == "true"
}

func Worker(ctx context.Context, r client.Reader, name string) error {
	var n corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		return err
	}
	if !Admitted(&n, "") || n.Spec.Unschedulable {
		return fmt.Errorf("node %q is not an online, approved worker", name)
	}
	return nil
}

func Resolve(r client.Reader, namespace string, controller ...string) Resolver {
	return func(ctx context.Context, name string) (World, error) {
		var s v1alpha1.MinecraftServer
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &s); err != nil {
			return World{}, err
		}
		node := s.Spec.NodeName
		if node == "" {
			node = s.Status.NodeName
		}
		if node == "" && len(controller) > 0 {
			node = controller[0]
		}
		if node != "" {
			var n corev1.Node
			if err := r.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
				return World{}, err
			}
			a := ""
			if len(controller) > 0 {
				a = controller[0]
			}
			if !Admitted(&n, a) {
				return World{}, fmt.Errorf("node %q is offline", node)
			}
		}
		return World{Claim: s.WorldPVC(), Node: node}, nil
	}
}
