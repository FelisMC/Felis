package operator_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/operator"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func stopWorkload() (*v1alpha1.MinecraftServer, *appsv1.StatefulSet) {
	server := runningServer()
	server.Spec.DesiredState = v1alpha1.DesiredStopped
	server.Spec.Lifecycle.StopCommands = []string{"say private-command", "save-off"}
	server.Status.Phase = v1alpha1.PhaseRunning
	one := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: server.Name, Namespace: server.Namespace},
		Spec:       appsv1.StatefulSetSpec{Replicas: &one},
		Status:     appsv1.StatefulSetStatus{Replicas: 1, ReadyReplicas: 1},
	}
	return server, sts
}

func TestStopCommandsFollowSaveBeforeScale(t *testing.T) {
	cases := []struct {
		name        string
		saveErr     error
		commandsErr error
		empty       bool
	}{
		{name: "success"},
		{name: "save failure", saveErr: errors.New("fake-reply hunter2")},
		{name: "command failure", commandsErr: errors.New("fake-reply private-command hunter2")},
		{name: "command timeout", commandsErr: context.DeadlineExceeded},
		{name: "empty commands", empty: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, sts := stopWorkload()
			if tc.empty {
				server.Spec.Lifecycle.StopCommands = []string{}
			}
			var steps, commands []string
			prober := fakeProber{players: operator.PlayerCount{Known: true}, steps: &steps, commands: &commands,
				saveErr: tc.saveErr, commandsErr: tc.commandsErr}
			r, c := newReconciler(t, prober, server, sts, rconSecret())
			r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
				Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if workload, ok := obj.(*appsv1.StatefulSet); ok && workload.Spec.Replicas != nil && *workload.Spec.Replicas == 0 {
						steps = append(steps, "scale")
					}
					return cl.Update(ctx, obj, opts...)
				},
			})
			var logs bytes.Buffer
			ctx := log.IntoContext(context.Background(), logr.FromSlogHandler(slog.NewJSONHandler(&logs, nil)))
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(server)}
			if _, err := r.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			want := []string{"save", "commands", "scale"}
			if tc.empty {
				want = []string{"save", "scale"}
			}
			if !slices.Equal(steps, want) || !slices.Equal(commands, server.Spec.Lifecycle.StopCommands) {
				t.Fatalf("steps=%v commands=%q", steps, commands)
			}
			if current := getSTS(t, c, server.Name); current.Spec.Replicas == nil || *current.Spec.Replicas != 0 {
				t.Fatal("pre-stop failure prevented scaling down")
			}
			if getServer(t, c, server.Name).Status.Phase != v1alpha1.PhaseStopping {
				t.Fatal("stop did not advance after pre-stop attempts")
			}
			for _, private := range []string{"private-command", "fake-reply", "hunter2"} {
				if strings.Contains(logs.String(), private) {
					t.Fatal("pre-stop diagnostics exposed private input")
				}
			}
			if _, err := r.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(steps, want) {
				t.Fatal("scaled-down workload repeated pre-stop commands")
			}
		})
	}
}

func TestStopCommandsRetryWhenScalingFails(t *testing.T) {
	server, sts := stopWorkload()
	var commands []string
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Known: true}, commands: &commands}, server, sts, rconSecret())
	fail := true
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if workload, ok := obj.(*appsv1.StatefulSet); ok && workload.Spec.Replicas != nil && *workload.Spec.Replicas == 0 && fail {
				fail = false
				return errors.New("scale unavailable")
			}
			return cl.Update(ctx, obj, opts...)
		},
	})
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(server)}
	if _, err := r.Reconcile(context.Background(), request); err == nil {
		t.Fatal("scale failure was not returned for retry")
	}
	if *getSTS(t, c, server.Name).Spec.Replicas != 1 {
		t.Fatal("failed scale unexpectedly changed replicas")
	}
	reconcile(t, r, server.Name)
	want := append(slices.Clone(server.Spec.Lifecycle.StopCommands), server.Spec.Lifecycle.StopCommands...)
	if !slices.Equal(commands, want) || *getSTS(t, c, server.Name).Spec.Replicas != 0 {
		t.Fatal("retry did not finish its best-effort commands and scale")
	}
}

func TestStopCommandsSkipUnavailableRconAndEmergencyScale(t *testing.T) {
	for _, mode := range []string{"pod not ready", "rcon disabled", "missing password", "emergency scaled"} {
		t.Run(mode, func(t *testing.T) {
			server, sts := stopWorkload()
			objects := []client.Object{server, sts}
			if mode != "missing password" {
				objects = append(objects, rconSecret())
			}
			switch mode {
			case "pod not ready":
				sts.Status.ReadyReplicas = 0
			case "rcon disabled":
				server.Spec.Rcon.Enabled = false
			case "emergency scaled":
				zero := int32(0)
				sts.Spec.Replicas = &zero
			}
			var saves, commands []string
			r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Known: true}, saves: &saves, commands: &commands}, objects...)
			reconcile(t, r, server.Name)
			if len(saves) != 0 || len(commands) != 0 {
				t.Fatal("unavailable or emergency stop attempted RCON hooks")
			}
			if *getSTS(t, c, server.Name).Spec.Replicas != 0 {
				t.Fatal("unavailable RCON prevented stopping")
			}
		})
	}
}
