package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"felis.lolicon.best/internal/nodecontrol"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestServerMigrateWaitsForNodeMaintenance pins that the CLI refuses migration changes
// while a host node operation runs, as the API does, and that a host without the
// node-control service is not blocked.
func TestServerMigrateWaitsForNodeMaintenance(t *testing.T) {
	// A short directory: a Unix socket path is capped near 104 bytes, and macOS temp dirs are long.
	dir, err := os.MkdirTemp("/tmp", "nc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := nodeMaintenanceBusy(context.Background(), filepath.Join(dir, "absent.sock")); err != nil {
		t.Fatalf("a host without node-control is blocked: %v", err)
	}

	socket := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	// Closed, it leaves the socket file behind, as a crashed service does.
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	state := "running"
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]nodecontrol.Task{{ID: "t1", State: state}})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	if err := nodeMaintenanceBusy(context.Background(), socket); err == nil {
		t.Error("a running node operation did not block the migration")
	}
	state = "succeeded"
	if err := nodeMaintenanceBusy(context.Background(), socket); err != nil {
		t.Errorf("a finished node operation still blocks: %v", err)
	}
	_ = server.Close()
	if err := nodeMaintenanceBusy(context.Background(), socket); err == nil {
		t.Error("an unanswering node-control service did not block the migration")
	}
}

// TestHomeControllerReadsTheOperator pins which controller the CLI treats as home: the
// operator's FELIS_CONTROLLER_NODE, else the only controller, and none when several are
// labelled and nothing names one.
func TestHomeControllerReadsTheOperator(t *testing.T) {
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	ctl := func(name string) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{placement.LabelRole: placement.RoleController}}}
	}
	op := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: platform.SAOperator, Namespace: "felis"}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "operator", Env: []corev1.EnvVar{{Name: "FELIS_CONTROLLER_NODE", Value: "b"}}}}}}}}
	ctx := context.Background()
	two := []corev1.Node{ctl("a"), ctl("b")}
	if got := homeController(ctx, fake.NewClientBuilder().WithScheme(scheme).WithObjects(op).Build(), "felis", two); got != "b" {
		t.Errorf("with the operator = %q, want b", got)
	}
	empty := fake.NewClientBuilder().WithScheme(scheme).Build()
	if got := homeController(ctx, empty, "felis", []corev1.Node{ctl("a")}); got != "a" {
		t.Errorf("one controller = %q", got)
	}
	if got := homeController(ctx, empty, "felis", two); got != "" {
		t.Errorf("a guess among several = %q", got)
	}
}
