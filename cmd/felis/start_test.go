package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// startHost fakes systemctl.
type startHost struct {
	enabled map[string]bool
	broken  map[string]bool
	active  map[string]string
	started [][]string
}

func (h *startHost) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	switch args[0] {
	case "is-enabled":
		if h.enabled[args[1]] {
			return []byte("enabled\n"), nil
		}
		return []byte("disabled\n"), errors.New("exit status 1")
	case "start":
		h.started = append(h.started, args[1:])
		var err error
		for _, u := range args[1:] {
			h.active[u] = "active"
			if h.broken[u] {
				h.active[u] = "failed"
				err = errors.New("exit status 1")
			}
		}
		return nil, err
	case "is-active":
		state := h.active[args[1]]
		if state == "" {
			state = "inactive"
		}
		return []byte(state + "\n"), nil
	}
	return nil, errors.New("unexpected systemctl " + args[0])
}

func newStartHost(t *testing.T, timers []string, enabled ...string) (*startHost, string) {
	t.Helper()
	dir := t.TempDir()
	for _, timer := range timers {
		if err := os.WriteFile(filepath.Join(dir, timer), []byte("[Timer]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := &startHost{enabled: map[string]bool{}, broken: map[string]bool{}, active: map[string]string{}}
	for _, u := range enabled {
		h.enabled[u] = true
	}
	return h, dir
}

func controlPlane(t *testing.T, objs ...client.Object) func() (client.Client, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return func() (client.Client, error) { return cl, nil }
}

func noCluster(t *testing.T, why string) func() (client.Client, error) {
	return func() (client.Client, error) {
		t.Fatal(why)
		return nil, nil
	}
}

func readyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func deployment(name string, ready int32) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "felis"},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

func TestStartBringsTheClusterUpFirstThenWhatIsEnabled(t *testing.T) {
	timers := []string{"felis-db-backup.timer", "felis-offsite.timer", "felis-watchdog.timer"}
	h, dir := newStartHost(t, timers,
		"k3s.service", "felis-node-control.service", "felis-velocity.service", "felis-db-backup.timer", "felis-watchdog.timer")
	env := startEnv{
		unitDir: dir, run: h.run, controlNS: "felis", poll: time.Millisecond,
		cluster: controlPlane(t, readyNode("ctrl-1"), deployment("felis-api", 1), deployment("felis-operator", 1)),
	}
	var out bytes.Buffer
	if code := runStart(context.Background(), env, &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	want := [][]string{
		{"k3s.service"},
		{"felis-node-control.service", "felis-velocity.service", "felis-db-backup.timer", "felis-watchdog.timer"},
	}
	if !reflect.DeepEqual(h.started, want) {
		t.Fatalf("started %v, want %v", h.started, want)
	}
	for _, line := range []string{"node ctrl-1 Ready", "felis-api       1/1 ready", "felis-operator  1/1 ready", "Felis is up."} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("report lacks %q:\n%s", line, out.String())
		}
	}
}

func TestStartSaysWhatDidNotComeUpAndWhereToLook(t *testing.T) {
	h, dir := newStartHost(t, nil, "k3s.service", "felis-velocity.service")
	h.broken["felis-velocity.service"] = true
	env := startEnv{
		unitDir: dir, run: h.run, controlNS: "felis", poll: time.Millisecond,
		cluster: controlPlane(t, readyNode("ctrl-1"), deployment("felis-api", 1), deployment("felis-operator", 0)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if code := runStart(ctx, env, &out); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out.String())
	}
	for _, line := range []string{
		"felis-velocity.service  failed",
		"felis-operator  0/1 ready",
		"2 part(s) did not come up",
		"felis-velocity.service → journalctl -u felis-velocity.service -n 100 --no-pager",
		"deployment/felis-operator → k3s kubectl -n felis describe deployment felis-operator",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("report lacks %q:\n%s", line, out.String())
		}
	}
}

func TestStartGoesOnWhenTheClusterDoesNotStart(t *testing.T) {
	h, dir := newStartHost(t, nil, "k3s.service", "felis-velocity.service")
	h.broken["k3s.service"] = true
	env := startEnv{unitDir: dir, run: h.run, controlNS: "felis", poll: time.Millisecond, cluster: noCluster(t, "asked the API of a cluster that did not start")}
	var out bytes.Buffer
	if code := runStart(context.Background(), env, &out); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out.String())
	}
	if want := [][]string{{"k3s.service"}, {"felis-velocity.service"}}; !reflect.DeepEqual(h.started, want) {
		t.Fatalf("started %v, want %v", h.started, want)
	}
	if !strings.Contains(out.String(), "k3s.service → journalctl -u k3s.service") {
		t.Errorf("no hint for k3s:\n%s", out.String())
	}
}

func TestStartOnAWorkerWaitsForNoControlPlane(t *testing.T) {
	h, dir := newStartHost(t, nil, "k3s-agent.service", "felis-node-control.service")
	env := startEnv{unitDir: dir, run: h.run, controlNS: "felis", poll: time.Millisecond, cluster: noCluster(t, "a worker has no control plane to wait for")}
	var out bytes.Buffer
	if code := runStart(context.Background(), env, &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if want := [][]string{{"k3s-agent.service"}, {"felis-node-control.service"}}; !reflect.DeepEqual(h.started, want) {
		t.Fatalf("started %v, want %v", h.started, want)
	}
}

func TestStartOnAHostWithoutFelis(t *testing.T) {
	h, dir := newStartHost(t, nil)
	var out bytes.Buffer
	if code := runStart(context.Background(), startEnv{unitDir: dir, run: h.run}, &out); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if len(h.started) != 0 || !strings.Contains(out.String(), "no Felis unit is set to start on this host") {
		t.Fatalf("started %v:\n%s", h.started, out.String())
	}
}
