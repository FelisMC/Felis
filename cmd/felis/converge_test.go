package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// converge is the explicit pass over an installed system server whose CR predates
// a field the desired spec has since gained (#1). It must fill exactly the
// zero-valued whitelist fields and the derived env, and must not touch anything a
// non-zero value already occupies — that is the operator's.
func TestConvergeSystemServersFillsPredatedFields(t *testing.T) {
	scheme := newSystemServerScheme(t)
	ctx := context.Background()

	// An old install: the lobby CR was created before the desired spec began
	// rendering spec.rcon, and the login CR before the HTTP readiness gate existed.
	// One derived env key is absent entirely (as if it were added later), and one
	// hand-added env var plus a non-whitelisted spec field must survive.
	lobby, err := lobbySystemServer("reg/lobby:1", "minecraft")
	if err != nil {
		t.Fatalf("build lobby: %v", err)
	}
	lobby.Spec.Rcon = v1alpha1.RconSpec{}
	lobby.Spec.JavaMemory = "999Mi"

	login, err := loginSystemServer("reg/limbo:1", "minecraft",
		"http://felis-api.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net")
	if err != nil {
		t.Fatalf("build login: %v", err)
	}
	login.Spec.Startup.HealthHTTPPort = 0
	kept := login.Spec.Env
	login.Spec.Env = nil
	for _, e := range kept {
		if e.Name != envPanelHostname {
			login.Spec.Env = append(login.Spec.Env, e)
		}
	}
	login.Spec.Env = append(login.Spec.Env, v1alpha1.EnvVar{Name: "OPERATOR_TUNING", Value: "keep-me"})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lobby, login).Build()
	outcomes := convergeSystemServers(ctx, cl, "minecraft", "reg/limbo:1", "reg/lobby:1",
		"http://felis-api.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net")

	byName := map[string]systemServerOutcome{}
	for _, o := range outcomes {
		if o.err != nil {
			t.Fatalf("%s: unexpected error: %v", o.name, o.err)
		}
		byName[o.name] = o
	}
	lobbyOut := byName[naming.SystemLobbyServer]
	if len(lobbyOut.changes) != 1 || lobbyOut.changes[0] != "spec.rcon" {
		t.Errorf("lobby changes = %v, want [spec.rcon] (only the zero-valued field)", lobbyOut.changes)
	}
	loginOut := byName[naming.SystemLoginServer]
	if !slices.Contains(loginOut.changes, "spec.startup.healthHTTPPort") || !slices.Contains(loginOut.changes, "env "+envPanelHostname) {
		t.Errorf("login changes = %v, want the health port plus the missing derived env key", loginOut.changes)
	}

	var gotLobby v1alpha1.MinecraftServer
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: naming.SystemLobbyServer}, &gotLobby); err != nil {
		t.Fatalf("get lobby: %v", err)
	}
	if !gotLobby.Spec.Rcon.Enabled ||
		gotLobby.Spec.Rcon.SecretRef.Name != naming.RconSecretName(naming.SystemLobbyServer) ||
		gotLobby.Spec.Rcon.SecretRef.Key != naming.RconSecretKey {
		t.Errorf("lobby rcon = %+v, want the desired block with the %s secret",
			gotLobby.Spec.Rcon, naming.RconSecretName(naming.SystemLobbyServer))
	}
	if gotLobby.Spec.JavaMemory != "999Mi" {
		t.Errorf("lobby javaMemory = %q, want 999Mi — converge fills new fields, it does not rewrite the spec", gotLobby.Spec.JavaMemory)
	}

	var gotLogin v1alpha1.MinecraftServer
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: naming.SystemLoginServer}, &gotLogin); err != nil {
		t.Fatalf("get login: %v", err)
	}
	if gotLogin.Spec.Startup.HealthHTTPPort != felisLimboHealthPort {
		t.Errorf("login healthHTTPPort = %d, want %d", gotLogin.Spec.Startup.HealthHTTPPort, felisLimboHealthPort)
	}
	env := map[string]string{}
	for _, e := range gotLogin.Spec.Env {
		env[e.Name] = e.Value
	}
	if env[envPanelHostname] != "console.mc.example.net" {
		t.Errorf("%s was not added back: %q", envPanelHostname, env[envPanelHostname])
	}
	if env["OPERATOR_TUNING"] != "keep-me" {
		t.Error("a hand-added env var was dropped; converge only touches config-derived names")
	}
}

// A field already holding a non-zero value belongs to the operator: converge must
// report "already converged" and write nothing.
func TestConvergeSystemServersLeavesNonZeroFieldsAlone(t *testing.T) {
	scheme := newSystemServerScheme(t)
	ctx := context.Background()

	lobby, err := lobbySystemServer("reg/lobby:1", "minecraft")
	if err != nil {
		t.Fatalf("build lobby: %v", err)
	}
	lobby.Spec.Rcon = v1alpha1.RconSpec{
		Enabled:   true,
		SecretRef: v1alpha1.SecretKeyRef{Name: "operator-rotated", Key: "password"},
	}
	login, err := loginSystemServer("reg/limbo:1", "minecraft",
		"http://felis-api.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net")
	if err != nil {
		t.Fatalf("build login: %v", err)
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lobby, login).Build()
	for _, o := range convergeSystemServers(ctx, cl, "minecraft", "reg/limbo:1", "reg/lobby:1",
		"http://felis-api.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net") {
		if o.err != nil {
			t.Fatalf("%s: unexpected error: %v", o.name, o.err)
		}
		if len(o.changes) != 0 || o.skipped != "already converged" {
			t.Errorf("%s outcome = %+v, want already converged with no writes", o.name, o)
		}
	}
	var got v1alpha1.MinecraftServer
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: naming.SystemLobbyServer}, &got); err != nil {
		t.Fatalf("get lobby: %v", err)
	}
	if got.Spec.Rcon.SecretRef.Name != "operator-rotated" {
		t.Errorf("lobby rcon secretRef = %q — converge overwrote a field the operator had already set",
			got.Spec.Rcon.SecretRef.Name)
	}
}

// Guards: an absent CR is reported (creation is setup's job), a foreign CR is
// refused rather than adopted, and an unset image skips like the provisioner does.
func TestConvergeSystemServersGuards(t *testing.T) {
	scheme := newSystemServerScheme(t)
	ctx := context.Background()
	run := func(cl client.Client, loginImage, lobbyImage string) []systemServerOutcome {
		return convergeSystemServers(ctx, cl, "minecraft", loginImage, lobbyImage,
			"http://felis-api.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net")
	}

	t.Run("absent CRs are reported, not created", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		for _, o := range run(cl, "reg/limbo:1", "reg/lobby:1") {
			if o.err != nil {
				t.Fatalf("%s: %v", o.name, o.err)
			}
			if o.created || !strings.Contains(o.skipped, "not present") {
				t.Errorf("%s outcome = %+v, want a not-present skip", o.name, o)
			}
		}
	})

	t.Run("foreign CR is refused", func(t *testing.T) {
		foreign := &v1alpha1.MinecraftServer{}
		foreign.Name = naming.SystemLoginServer
		foreign.Namespace = "minecraft"
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
		out := run(cl, "reg/limbo:1", "")
		if len(out) != 2 {
			t.Fatalf("outcomes = %d, want 2", len(out))
		}
		if out[0].err == nil || !strings.Contains(out[0].err.Error(), "not marked") {
			t.Fatalf("login error = %v, want an unmarked-name refusal", out[0].err)
		}
	})

	t.Run("unset image skips", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		out := run(cl, "", "reg/lobby:1")
		if out[0].skipped != "image not configured" {
			t.Errorf("login skipped = %q, want %q", out[0].skipped, "image not configured")
		}
	})
}
