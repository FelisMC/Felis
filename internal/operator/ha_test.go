package operator_test

import (
	"context"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/placement"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func haNode(name string, ready bool) *corev1.Node {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionUnknown
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{placement.LabelIdentity: name, placement.LabelRole: placement.RoleController}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}}}
}

// TestSystemServerFloatsWithControllerHA pins controller HA in the operator: with the home
// controller gone, a system server nobody pinned still starts, on any controller; a user
// server stays bound to its node and waits for it; without HA the system server waits too.
func TestSystemServerFloatsWithControllerHA(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ha, system   bool
		wantFloating bool
	}{
		{"system server with HA", true, true, true},
		{"system server without HA", false, true, false},
		{"user server with HA", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := runningServer()
			if tc.system {
				server.Labels = map[string]string{v1alpha1.LabelSystemRole: "lobby"}
			}
			r, c := newReconciler(t, fakeProber{}, server, rconSecret(), haNode("a", false), haNode("c", true))
			r.Nodes, r.ControllerNode, r.ControllerHA = c, "a", tc.ha
			reconcile(t, r, "survival")
			var sts appsv1.StatefulSet
			err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &sts)
			s := getServer(t, c, "survival")
			cond := meta.FindStatusCondition(s.Status.Conditions, v1alpha1.ConditionReady)
			if !tc.wantFloating {
				if !apierrors.IsNotFound(err) || cond == nil || cond.Reason != "NodeUnavailable" {
					t.Fatalf("pinned to the lost node: StatefulSet err %v, condition %+v", err, cond)
				}
				return
			}
			if err != nil {
				t.Fatalf("no StatefulSet for a floating system server: %v", err)
			}
			if sel := sts.Spec.Template.Spec.NodeSelector; len(sel) != 1 || sel[placement.LabelRole] != placement.RoleController {
				t.Fatalf("selector = %v, want any controller", sel)
			}
			if cond != nil && cond.Reason == "NodeUnavailable" {
				t.Fatalf("a floating server waited for the lost node: %+v", cond)
			}
		})
	}
}
