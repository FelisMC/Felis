package main

import (
	"context"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A system service must be born up, reaper-exempt, publicly wakeable, and
// RCON-free — the uniform shape the topology invariants depend on.
func TestBuildSystemServerShape(t *testing.T) {
	ms, err := buildSystemServer(systemServerSpec{
		name:      "login",
		subdomain: "login",
		image:     "reg/limbo:1",
		memory:    "512Mi",
		storage:   "1Gi",
	}, "minecraft")
	if err != nil {
		t.Fatalf("buildSystemServer: %v", err)
	}
	if ms.Spec.DesiredState != v1alpha1.DesiredRunning {
		t.Errorf("DesiredState = %q, want Running", ms.Spec.DesiredState)
	}
	if !ms.Spec.ReaperExempt {
		t.Error("ReaperExempt = false, want true")
	}
	if ms.Spec.AutostartPolicy != v1alpha1.AutostartPublic {
		t.Errorf("AutostartPolicy = %q, want public", ms.Spec.AutostartPolicy)
	}
	if ms.Spec.Rcon.Enabled {
		t.Error("Rcon.Enabled = true, want false (LOOHP/Limbo has no RCON)")
	}
	if ms.Spec.OnlineMode {
		t.Error("OnlineMode = true, want false (backend behind the proxy)")
	}
	if _, ok := ms.Spec.Resources.Limits[corev1.ResourceMemory]; !ok {
		t.Error("missing memory limit (§22 ceiling)")
	}
	if ms.Namespace != "minecraft" {
		t.Errorf("namespace = %q, want minecraft", ms.Namespace)
	}
}

// The login gate is the front door and the only safe fallback, so it carries no
// fallback of its own; the lobby falls back to login. Neither may fall back to
// the lobby — that would route a player past authentication.
func TestSystemServerFallbackPolicy(t *testing.T) {
	login, err := loginSystemServer("reg/limbo:1", "minecraft", "http://felis-api.felis.svc.cluster.local:8081", "mc.example.net")
	if err != nil {
		t.Fatalf("loginSystemServer: %v", err)
	}
	if login.Spec.FallbackServer != "" {
		t.Errorf("login FallbackServer = %q, want empty (refuse when down)", login.Spec.FallbackServer)
	}

	lobby, err := lobbySystemServer("reg/lobby:1", "minecraft")
	if err != nil {
		t.Fatalf("lobbySystemServer: %v", err)
	}
	if lobby.Spec.FallbackServer != naming.SystemLoginServer {
		t.Errorf("lobby FallbackServer = %q, want %q", lobby.Spec.FallbackServer, naming.SystemLoginServer)
	}
}

// buildSystemServer must refuse a spec that falls back to the lobby rather than
// silently shipping the auth-bypass.
func TestBuildSystemServerRejectsLobbyFallback(t *testing.T) {
	_, err := buildSystemServer(systemServerSpec{
		name:           "survival",
		subdomain:      "survival",
		image:          "reg/paper:1",
		memory:         "1Gi",
		storage:        "1Gi",
		fallbackServer: naming.SystemLobbyServer,
	}, "minecraft")
	if err == nil {
		t.Fatal("expected error for fallback=lobby, got nil")
	}
}

func TestBuildSystemServerRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		in   systemServerSpec
	}{
		{"empty image", systemServerSpec{name: "login", subdomain: "login", memory: "512Mi", storage: "1Gi"}},
		{"bad name", systemServerSpec{name: "ab", subdomain: "login", image: "x", memory: "512Mi", storage: "1Gi"}},
		{"zero memory", systemServerSpec{name: "login", subdomain: "login", image: "x", memory: "0", storage: "1Gi"}},
		{"bad storage", systemServerSpec{name: "login", subdomain: "login", image: "x", memory: "512Mi", storage: "nonsense"}},
	}
	for _, tc := range cases {
		if _, err := buildSystemServer(tc.in, "minecraft"); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

// The login gate needs its deployment config as plain env: the internal API URL,
// the root domain, and the lobby name — but NEVER the service token (that is
// injected by the operator via secretKeyRef, never a literal in the CRD).
func TestLoginSystemServerEnv(t *testing.T) {
	login, err := loginSystemServer("reg/limbo:1", "minecraft",
		"http://felis-api.felis.svc.cluster.local:8081", "mc.example.net")
	if err != nil {
		t.Fatalf("loginSystemServer: %v", err)
	}
	got := map[string]string{}
	for _, e := range login.Spec.Env {
		got[e.Name] = e.Value
	}
	want := map[string]string{
		"FELIS_API_BASE_URL": "http://felis-api.felis.svc.cluster.local:8081",
		"FELIS_ROOT_DOMAIN":  "mc.example.net",
		"FELIS_LOBBY_SERVER": naming.SystemLobbyServer,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q", k, got[k], v)
		}
	}
	// The CRD's EnvVar type has no valueFrom, so the token must NEVER appear here —
	// it would have to be a plaintext value, which is the leak we refuse.
	if _, leaked := got["FELIS_SERVICE_TOKEN"]; leaked {
		t.Error("FELIS_SERVICE_TOKEN must not be baked into the CRD (operator injects it via secretKeyRef)")
	}
}

// ensureServiceTokenReplica copies the token Secret from the control namespace into
// the minecraft namespace (create-if-absent), so the operator's secretKeyRef on the
// login pod resolves. It must not overwrite an existing replica, and must degrade
// gracefully when the source is missing or the namespaces coincide.
func TestEnsureServiceTokenReplica(t *testing.T) {
	scheme := newSystemServerScheme(t)
	ctx := context.Background()

	srcSecret := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: naming.ServiceTokenSecretName, Namespace: "felis"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{naming.ServiceTokenSecretKey: []byte("s3cr3t")},
		}
	}

	t.Run("replicates when absent", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(srcSecret()).Build()
		out := ensureServiceTokenReplica(ctx, cl, "felis", "minecraft")
		if out.err != nil || !out.created {
			t.Fatalf("outcome = %+v, want created", out)
		}
		var replica corev1.Secret
		if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: naming.ServiceTokenSecretName}, &replica); err != nil {
			t.Fatalf("get replica: %v", err)
		}
		if string(replica.Data[naming.ServiceTokenSecretKey]) != "s3cr3t" {
			t.Errorf("replica token = %q, want s3cr3t", replica.Data[naming.ServiceTokenSecretKey])
		}
	})

	t.Run("does not overwrite existing replica", func(t *testing.T) {
		existing := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: naming.ServiceTokenSecretName, Namespace: "minecraft"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{naming.ServiceTokenSecretKey: []byte("rotated")},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(srcSecret(), existing).Build()
		out := ensureServiceTokenReplica(ctx, cl, "felis", "minecraft")
		if out.created || out.skipped == "" {
			t.Fatalf("outcome = %+v, want skipped (not clobbered)", out)
		}
		var replica corev1.Secret
		if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: naming.ServiceTokenSecretName}, &replica); err != nil {
			t.Fatalf("get replica: %v", err)
		}
		if string(replica.Data[naming.ServiceTokenSecretKey]) != "rotated" {
			t.Error("existing replica was overwritten — a rotated token must survive")
		}
	})

	t.Run("skips when source missing", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		out := ensureServiceTokenReplica(ctx, cl, "felis", "minecraft")
		if out.err != nil || out.created || out.skipped == "" {
			t.Fatalf("outcome = %+v, want skipped (source absent)", out)
		}
	})

	t.Run("no-op when namespaces coincide", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		out := ensureServiceTokenReplica(ctx, cl, "felis", "felis")
		if out.err != nil || out.created {
			t.Fatalf("outcome = %+v, want skipped no-op", out)
		}
	})
}

func newSystemServerScheme(t *testing.T) *runtime.Scheme {
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

// ensureSystemServers creates both services on a fresh cluster, then is a no-op
// on re-run (create-if-absent), and skips a service whose image is unset.
func TestEnsureSystemServersIdempotent(t *testing.T) {
	scheme := newSystemServerScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	first := ensureSystemServers(ctx, cl, "minecraft", "reg/limbo:1", "reg/lobby:1", "http://felis-api.felis.svc.cluster.local:8081", "mc.example.net")
	if len(first) != 2 {
		t.Fatalf("first run outcomes = %d, want 2", len(first))
	}
	for _, o := range first {
		if o.err != nil {
			t.Fatalf("%s: unexpected error: %v", o.name, o.err)
		}
		if !o.created {
			t.Errorf("%s: created = false on fresh cluster (skipped=%q)", o.name, o.skipped)
		}
	}

	// The created login CRD must carry the always-on system-service shape.
	var login v1alpha1.MinecraftServer
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: "login"}, &login); err != nil {
		t.Fatalf("get login after create: %v", err)
	}
	if login.Spec.DesiredState != v1alpha1.DesiredRunning || !login.Spec.ReaperExempt {
		t.Errorf("login spec = {desired=%q exempt=%v}, want {Running true}", login.Spec.DesiredState, login.Spec.ReaperExempt)
	}

	// Re-run: both already exist → skipped, nothing created, no error.
	second := ensureSystemServers(ctx, cl, "minecraft", "reg/limbo:1", "reg/lobby:1", "http://felis-api.felis.svc.cluster.local:8081", "mc.example.net")
	for _, o := range second {
		if o.err != nil {
			t.Fatalf("%s: unexpected error on re-run: %v", o.name, o.err)
		}
		if o.created {
			t.Errorf("%s: created = true on re-run, want skipped", o.name)
		}
		if o.skipped == "" {
			t.Errorf("%s: skipped reason empty on re-run", o.name)
		}
	}
}

func TestEnsureSystemServersSkipsUnsetImage(t *testing.T) {
	scheme := newSystemServerScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	// login image set, lobby image empty → login created, lobby skipped.
	out := ensureSystemServers(ctx, cl, "minecraft", "reg/limbo:1", "", "http://felis-api.felis.svc.cluster.local:8081", "mc.example.net")
	byName := map[string]systemServerOutcome{}
	for _, o := range out {
		byName[o.name] = o
	}
	if !byName[naming.SystemLoginServer].created {
		t.Errorf("login: created = false, want true")
	}
	if byName[naming.SystemLobbyServer].skipped != "image not configured" {
		t.Errorf("lobby: skipped = %q, want %q", byName[naming.SystemLobbyServer].skipped, "image not configured")
	}
}
