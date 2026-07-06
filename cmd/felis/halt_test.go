package main

import (
	"context"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const haltNS = "minecraft"

func haltScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("clientgo scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("v1alpha1 scheme: %v", err)
	}
	return scheme
}

func mcServer(name string, desired v1alpha1.DesiredState, phase v1alpha1.Phase) *v1alpha1.MinecraftServer {
	return &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: haltNS},
		Spec:       v1alpha1.MinecraftServerSpec{DesiredState: desired},
		Status:     v1alpha1.MinecraftServerStatus{Phase: phase},
	}
}

func haltClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(haltScheme(t)).WithObjects(objs...).Build()
}

// desiredStateOf reads a server's spec.desiredState straight back from the client,
// so the patch is verified by its actual persisted effect, not by "Patch was called".
func desiredStateOf(t *testing.T, cl client.Client, name string) v1alpha1.DesiredState {
	t.Helper()
	var ms v1alpha1.MinecraftServer
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: haltNS, Name: name}, &ms); err != nil {
		t.Fatalf("get %q back: %v", name, err)
	}
	return ms.Spec.DesiredState
}

func TestHaltServer(t *testing.T) {
	ctx := context.Background()

	t.Run("running server is patched to Stopped", func(t *testing.T) {
		cl := haltClient(t, mcServer("survival", v1alpha1.DesiredRunning, v1alpha1.PhaseRunning))
		out, err := haltServer(ctx, cl, haltNS, "survival")
		if err != nil {
			t.Fatalf("haltServer: %v", err)
		}
		if out.alreadyStopped {
			t.Error("alreadyStopped = true, want false for a running server")
		}
		if got := desiredStateOf(t, cl, "survival"); got != v1alpha1.DesiredStopped {
			t.Errorf("desiredState after halt = %q, want Stopped", got)
		}
	})

	t.Run("already-stopped server is a reported no-op", func(t *testing.T) {
		cl := haltClient(t, mcServer("survival", v1alpha1.DesiredStopped, v1alpha1.PhaseStopped))
		out, err := haltServer(ctx, cl, haltNS, "survival")
		if err != nil {
			t.Fatalf("haltServer: %v", err)
		}
		if !out.alreadyStopped {
			t.Error("alreadyStopped = false, want true for an already-stopped server")
		}
		if got := desiredStateOf(t, cl, "survival"); got != v1alpha1.DesiredStopped {
			t.Errorf("desiredState = %q, want it left Stopped", got)
		}
	})

	t.Run("missing server is a clear error, not a patch", func(t *testing.T) {
		cl := haltClient(t)
		_, err := haltServer(ctx, cl, haltNS, "ghost")
		if err == nil {
			t.Fatal("haltServer(ghost) = nil error, want not-found error")
		}
		if !strings.Contains(err.Error(), "ghost") {
			t.Errorf("error %q does not name the missing server", err)
		}
	})

	t.Run("halting login is allowed and flagged a system server", func(t *testing.T) {
		cl := haltClient(t, mcServer("login", v1alpha1.DesiredRunning, v1alpha1.PhaseRunning))
		out, err := haltServer(ctx, cl, haltNS, "login")
		if err != nil {
			t.Fatalf("haltServer(login): %v", err)
		}
		if !out.system {
			t.Error("system = false, want true for the login front-door server")
		}
		if got := desiredStateOf(t, cl, "login"); got != v1alpha1.DesiredStopped {
			t.Errorf("desiredState after halt = %q, want Stopped", got)
		}
	})
}

func TestListServersForHalt(t *testing.T) {
	cl := haltClient(t,
		mcServer("survival", v1alpha1.DesiredRunning, ""), // no status yet → phase falls back to intent
		mcServer("login", v1alpha1.DesiredRunning, ""),
	)
	got, err := listServersForHalt(context.Background(), cl, haltNS)
	if err != nil {
		t.Fatalf("listServersForHalt: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d servers, want 2", len(got))
	}
	by := map[string]haltableServer{}
	for _, s := range got {
		by[s.name] = s
	}
	if s := by["survival"]; s.system || s.phase != "Running" {
		t.Errorf("survival projected %+v, want system=false phase=Running (desired-state fallback)", s)
	}
	if s := by["login"]; !s.system {
		t.Errorf("login projected system=false, want true")
	}
}

func TestPerformHalt(t *testing.T) {
	ctx := context.Background()

	t.Run("halts and records an accountability audit row", func(t *testing.T) {
		cl := haltClient(t, mcServer("survival", v1alpha1.DesiredRunning, v1alpha1.PhaseRunning))
		f := &fakeOwnerStore{}
		out, err := performHalt(ctx, cl, f, haltNS, "survival", "deploybot")
		if err != nil {
			t.Fatalf("performHalt: %v", err)
		}
		if out.auditErr != nil {
			t.Errorf("auditErr = %v, want nil", out.auditErr)
		}
		if got := desiredStateOf(t, cl, "survival"); got != v1alpha1.DesiredStopped {
			t.Errorf("desiredState after performHalt = %q, want Stopped", got)
		}
		e, payload := auditOf(t, f)
		if e.Action != "break_glass.halt" {
			t.Errorf("audit action = %q, want break_glass.halt", e.Action)
		}
		if e.Actor != "deploybot" {
			t.Errorf("audit actor = %q, want deploybot", e.Actor)
		}
		if payload["server"] != "survival" || payload["system_server"] != false {
			t.Errorf("audit payload = %v, want server=survival system_server=false", payload)
		}
	})

	t.Run("a failed audit sink does not fail the halt", func(t *testing.T) {
		cl := haltClient(t, mcServer("survival", v1alpha1.DesiredRunning, v1alpha1.PhaseRunning))
		f := &fakeOwnerStore{auditErr: context.DeadlineExceeded}
		out, err := performHalt(ctx, cl, f, haltNS, "survival", "deploybot")
		if err != nil {
			t.Fatalf("performHalt returned error on audit failure, want halt to still succeed: %v", err)
		}
		if out.auditErr == nil {
			t.Error("auditErr = nil, want the sink failure surfaced")
		}
		if got := desiredStateOf(t, cl, "survival"); got != v1alpha1.DesiredStopped {
			t.Errorf("desiredState = %q, want Stopped even though the audit failed", got)
		}
	})
}
