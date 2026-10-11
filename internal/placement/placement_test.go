package placement

import (
	"context"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func node(name, role string, ready bool, labels ...string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{LabelIdentity: name}}}
	if role != "" {
		n.Labels[LabelRole] = role
	}
	for i := 0; i+1 < len(labels); i += 2 {
		n.Labels[labels[i]] = labels[i+1]
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	return n
}

// TestAdmitted pins who may hold worlds and control-plane pods: any online controller
// (several with high availability), an approved worker, and nobody whose identity label
// is missing or another node's, or who is named the controller without its role.
func TestAdmitted(t *testing.T) {
	spoofed := node("b", RoleWorker, true, LabelApproved, "true")
	spoofed.Labels[LabelIdentity] = "a"
	for name, tc := range map[string]struct {
		n    *corev1.Node
		want bool
	}{
		"the controller":                   {node("a", RoleController, true), true},
		"another controller":               {node("c", RoleController, true), true},
		"an offline controller":            {node("c", RoleController, false), false},
		"an approved worker":               {node("w", RoleWorker, true, LabelApproved, "true"), true},
		"a worker awaiting approval":       {node("w", RoleWorker, true), false},
		"the controller's name, no role":   {node("a", "", true), false},
		"a worker named as the controller": {node("a", RoleWorker, true, LabelApproved, "true"), false},
		"someone else's identity":          {spoofed, false},
		"no role at all":                   {node("x", "", true), false},
	} {
		if got := Admitted(tc.n, "a"); got != tc.want {
			t.Errorf("%s: Admitted = %v, want %v", name, got, tc.want)
		}
	}
	gone := node("c", RoleController, true)
	now := metav1.Now()
	gone.DeletionTimestamp = &now
	if Admitted(gone, "a") {
		t.Error("a node being deleted is admitted")
	}
}

// TestWorkerRefusesControllers pins that a migration's worker check takes workers only:
// a controller is a target through its own check.
func TestWorkerRefusesControllers(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cordoned := node("z", RoleWorker, true, LabelApproved, "true")
	cordoned.Spec.Unschedulable = true
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node("w", RoleWorker, true, LabelApproved, "true"), node("c", RoleController, true), cordoned).Build()
	ctx := context.Background()
	if err := Worker(ctx, cl, "w"); err != nil {
		t.Errorf("approved worker: %v", err)
	}
	for _, name := range []string{"c", "z", "missing"} {
		if err := Worker(ctx, cl, name); err == nil {
			t.Errorf("%s passed as a worker", name)
		}
	}
}

// TestResolveFindsTheWorldsNode pins Resolve: the server's own node, else where it last
// ran, else the controller; refused when that node cannot take it.
func TestResolveFindsTheWorldsNode(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	pinned := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "pinned", Namespace: "mc"}, Spec: v1alpha1.MinecraftServerSpec{NodeName: "w"}}
	ran := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "ran", Namespace: "mc"}, Status: v1alpha1.MinecraftServerStatus{NodeName: "c"}}
	home := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "home", Namespace: "mc"}}
	lost := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "lost", Namespace: "mc"}, Spec: v1alpha1.MinecraftServerSpec{NodeName: "gone"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pinned, ran, home, lost,
		node("a", RoleController, true), node("c", RoleController, true), node("w", RoleWorker, true, LabelApproved, "true"), node("gone", RoleWorker, false, LabelApproved, "true")).Build()
	resolve := Resolve(cl, "mc", "a")
	ctx := context.Background()
	for server, want := range map[string]string{"pinned": "w", "ran": "c", "home": "a"} {
		w, err := resolve(ctx, server)
		if err != nil || w.Node != want || w.Claim == "" {
			t.Errorf("%s = %+v (%v), want node %s", server, w, err, want)
		}
	}
	if _, err := resolve(ctx, "lost"); err == nil {
		t.Error("a world on an offline node resolved")
	}
}
