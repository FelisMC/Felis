package main

import (
	"context"
	"strings"
	"testing"
	"time"

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
	login, err := loginSystemServer("reg/limbo:1", "minecraft", "http://felis-api.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net")
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
// the root domain, the resolved panel host, and the lobby name — but NEVER the
// service token (that is injected by the operator via secretKeyRef, never a literal
// in the CRD).
func TestLoginSystemServerEnv(t *testing.T) {
	login, err := loginSystemServer("reg/limbo:1", "minecraft",
		"http://felis-api.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net")
	if err != nil {
		t.Fatalf("loginSystemServer: %v", err)
	}
	got := map[string]string{}
	for _, e := range login.Spec.Env {
		got[e.Name] = e.Value
	}
	want := map[string]string{
		"FELIS_API_BASE_URL":   "http://felis-api.felis.svc.cluster.local:8081",
		"FELIS_ROOT_DOMAIN":    "mc.example.net",
		"FELIS_PANEL_HOSTNAME": "console.mc.example.net",
		"FELIS_LOBBY_SERVER":   naming.SystemLobbyServer,
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

// ensureSecretReplica copies a Secret from the control namespace into the minecraft
// namespace (create-if-absent), so the operator's secretKeyRef on the backend pod
// resolves. It must not overwrite an existing replica, and must degrade gracefully
// when the source is missing or the namespaces coincide. Exercised here with the
// service token; setup runs it a second time for the Velocity forwarding secret.
func TestEnsureSecretReplica(t *testing.T) {
	scheme := newSystemServerScheme(t)
	ctx := context.Background()

	srcSecret := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: naming.ServiceTokenSecretName, Namespace: "felis"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{naming.ServiceTokenSecretKey: []byte("s3cr3t")},
		}
	}
	replicate := func(cl client.Client, controlNS, mcNS string) systemServerOutcome {
		return ensureSecretReplica(ctx, cl, controlNS, mcNS,
			naming.ServiceTokenSecretName, naming.ServiceTokenSecretKey, "service-token")
	}

	t.Run("replicates when absent", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(srcSecret()).Build()
		out := replicate(cl, "felis", "minecraft")
		if out.err != nil || !out.created || !out.available {
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
		out := replicate(cl, "felis", "minecraft")
		if out.created || !out.available || out.skipped == "" {
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
		out := replicate(cl, "felis", "minecraft")
		if out.err != nil || out.created || out.available || out.skipped == "" {
			t.Fatalf("outcome = %+v, want skipped (source absent)", out)
		}
	})

	t.Run("rejects a source with an empty required key", func(t *testing.T) {
		bad := srcSecret()
		bad.Data[naming.ServiceTokenSecretKey] = nil
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(bad).Build()
		out := replicate(cl, "felis", "minecraft")
		if out.err != nil || out.created || out.available || !strings.Contains(out.skipped, naming.ServiceTokenSecretKey) {
			t.Fatalf("outcome = %+v, want unavailable required key", out)
		}
	})

	t.Run("rejects an existing replica with an empty required key", func(t *testing.T) {
		bad := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: naming.ServiceTokenSecretName, Namespace: "minecraft"},
			Data:       map[string][]byte{},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(srcSecret(), bad).Build()
		out := replicate(cl, "felis", "minecraft")
		if out.err != nil || out.created || out.available || !strings.Contains(out.skipped, naming.ServiceTokenSecretKey) {
			t.Fatalf("outcome = %+v, want unavailable existing replica", out)
		}
	})

	t.Run("no-op when namespaces coincide", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(srcSecret()).Build()
		out := replicate(cl, "felis", "felis")
		if out.err != nil || out.created || !out.available {
			t.Fatalf("outcome = %+v, want skipped no-op", out)
		}
	})

	t.Run("same namespace still requires source", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		out := replicate(cl, "felis", "felis")
		if out.err != nil || out.available || out.skipped == "" {
			t.Fatalf("outcome = %+v, want unavailable source", out)
		}
	})

	t.Run("same namespace still requires the key", func(t *testing.T) {
		bad := srcSecret()
		bad.Data = nil
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(bad).Build()
		out := replicate(cl, "felis", "felis")
		if out.err != nil || out.available || !strings.Contains(out.skipped, naming.ServiceTokenSecretKey) {
			t.Fatalf("outcome = %+v, want unavailable required key", out)
		}
	})
}

func TestRequiredProvisioningError(t *testing.T) {
	ready := []systemServerOutcome{
		{name: "service-token (minecraft ns)", available: true},
		{name: "forwarding-secret (minecraft ns)", available: true},
		{name: naming.SystemLoginServer, available: true},
		{name: naming.SystemLobbyServer, skipped: "image not configured"},
	}
	if err := requiredProvisioningError(ready); err != nil {
		t.Fatalf("ready outcomes: %v", err)
	}

	missing := append([]systemServerOutcome(nil), ready...)
	missing[1] = systemServerOutcome{name: "forwarding-secret (minecraft ns)", skipped: "source missing"}
	if err := requiredProvisioningError(missing); err == nil || !strings.Contains(err.Error(), "forwarding-secret") {
		t.Fatalf("missing forwarding secret = %v, want named error", err)
	}

	failed := append([]systemServerOutcome(nil), ready...)
	failed[3] = systemServerOutcome{name: naming.SystemLobbyServer, err: context.DeadlineExceeded}
	if err := requiredProvisioningError(failed); err == nil || !strings.Contains(err.Error(), naming.SystemLobbyServer) {
		t.Fatalf("lobby create failure = %v, want immediate named error", err)
	}
}

// The Owner binds by joining the game, so setup blocks on the login gate rather
// than racing it. What matters is that each ending is distinguishable: Ready
// proceeds, Failed reports the operator's own reason instead of waiting out the
// clock, and a gate that never appears (no operator reconciling it) times out
// saying so rather than dropping the operator on a bind screen that cannot work.
func TestAwaitLoginGateReady(t *testing.T) {
	scheme := newSystemServerScheme(t)
	ctx := context.Background()

	gate := func(mut func(*v1alpha1.MinecraftServer)) *v1alpha1.MinecraftServer {
		ms := &v1alpha1.MinecraftServer{
			ObjectMeta: metav1.ObjectMeta{Name: naming.SystemLoginServer, Namespace: "minecraft"},
		}
		mut(ms)
		return ms
	}

	t.Run("returns once the gate is ready", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gate(func(ms *v1alpha1.MinecraftServer) {
			ms.Status.Phase = v1alpha1.PhaseRunning
			ms.Status.Ready = true
		})).Build()
		if err := awaitLoginGateReady(ctx, cl, "minecraft", time.Second, 10*time.Millisecond, nil); err != nil {
			t.Fatalf("await: %v", err)
		}
	})

	t.Run("fails fast on Failed, carrying the operator's reason", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gate(func(ms *v1alpha1.MinecraftServer) {
			ms.Status.Phase = v1alpha1.PhaseFailed
			ms.Status.Conditions = []metav1.Condition{{
				Type:               v1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "StartupTimeout",
				Message:            "pod never became ready: ImagePullBackOff",
				LastTransitionTime: metav1.Now(),
			}}
		})).Build()
		start := time.Now()
		err := awaitLoginGateReady(ctx, cl, "minecraft", time.Minute, 10*time.Millisecond, nil)
		if err == nil {
			t.Fatal("await: nil error, want failure")
		}
		if !strings.Contains(err.Error(), "ImagePullBackOff") {
			t.Errorf("error = %q, want the operator's Ready-condition message", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Error("await sat out the full timeout on a settled Failed verdict")
		}
	})

	t.Run("times out when nothing ever reconciles the gate", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		err := awaitLoginGateReady(ctx, cl, "minecraft", 30*time.Millisecond, 10*time.Millisecond, nil)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("await = %v, want a timeout", err)
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

	first := ensureSystemServers(ctx, cl, "minecraft", "reg/limbo:1", "reg/lobby:1", "http://felis-api.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net")
	if len(first) != 2 {
		t.Fatalf("first run outcomes = %d, want 2", len(first))
	}
	for _, o := range first {
		if o.err != nil {
			t.Fatalf("%s: unexpected error: %v", o.name, o.err)
		}
		if !o.created || !o.available {
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
	if got := login.Labels[v1alpha1.LabelSystemRole]; got != naming.SystemLoginServer {
		t.Errorf("login system-role label = %q, want %q", got, naming.SystemLoginServer)
	}

	// Re-run: both already exist → skipped, nothing created, no error.
	second := ensureSystemServers(ctx, cl, "minecraft", "reg/limbo:1", "reg/lobby:1", "http://felis-api.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net")
	for _, o := range second {
		if o.err != nil {
			t.Fatalf("%s: unexpected error on re-run: %v", o.name, o.err)
		}
		if o.created {
			t.Errorf("%s: created = true on re-run, want skipped", o.name)
		}
		if !o.available {
			t.Errorf("%s: available = false on re-run", o.name)
		}
		if o.skipped == "" {
			t.Errorf("%s: skipped reason empty on re-run", o.name)
		}
	}
}

func TestEnsureSystemServersRejectsLegacyLoginNameCollision(t *testing.T) {
	scheme := newSystemServerScheme(t)
	legacy := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{
		Name:      naming.SystemLoginServer,
		Namespace: "minecraft",
	}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()

	out := ensureSystemServers(
		context.Background(), cl, "minecraft", "reg/limbo:1", "",
		"http://felis-api.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net",
	)
	if len(out) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(out))
	}
	login := out[0]
	if login.err == nil || !strings.Contains(login.err.Error(), "not marked") {
		t.Fatalf("login error = %v, want an unmarked-name collision error", login.err)
	}
	if login.available || login.created {
		t.Fatalf("login outcome = %+v, want unavailable and not created", login)
	}
}

func TestEnsureSystemServersSkipsUnsetImage(t *testing.T) {
	scheme := newSystemServerScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	// login image set, lobby image empty → login created, lobby skipped.
	out := ensureSystemServers(ctx, cl, "minecraft", "reg/limbo:1", "", "http://felis-api.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net")
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

// TestSystemServerRconPolicy pins which system service gets the RCON write
// channel. This is not a preference: the operator gates readiness on the RCON
// probe, so enabling it on a backend that serves no RCON listener would hold that
// server in Starting until it was marked Failed. The login limbo is exactly that
// backend (LOOHP/Limbo has no RCON) AND it is the front door, so getting this
// backwards locks every player out of the deployment.
func TestSystemServerRconPolicy(t *testing.T) {
	login, err := loginSystemServer("reg/limbo:1", "minecraft", "http://api:8081", "mc.example.net", "console.mc.example.net")
	if err != nil {
		t.Fatalf("loginSystemServer: %v", err)
	}
	if login.Spec.Rcon.Enabled {
		t.Fatal("the login limbo must not enable RCON: it serves no RCON listener, so the " +
			"operator's readiness probe would never succeed and the login gate would be marked Failed")
	}

	lobby, err := lobbySystemServer("reg/lobby:1", "minecraft")
	if err != nil {
		t.Fatalf("lobbySystemServer: %v", err)
	}
	if !lobby.Spec.Rcon.Enabled {
		t.Fatal("the lobby runs Paper and is administered through the panel; without RCON its " +
			"console, online-player list and permission changes are all unavailable")
	}
	if got, want := lobby.Spec.Rcon.SecretRef.Name, naming.RconSecretName("lobby"); got != want {
		t.Fatalf("lobby rcon secret = %q, want %q — the operator provisions against this name", got, want)
	}
	if got, want := lobby.Spec.Rcon.SecretRef.Key, naming.RconSecretKey; got != want {
		t.Fatalf("lobby rcon secret key = %q, want %q", got, want)
	}
	// Port stays unset so the operator's DefaultRconPort is the only place the
	// number is written down.
	if lobby.Spec.Rcon.Port != 0 {
		t.Fatalf("lobby rcon port = %d, want 0 (operator default)", lobby.Spec.Rcon.Port)
	}
}

// A root-domain change leaves the login gate handing every joining player a console
// link built from the old domain — the one screen an unauthenticated player is
// guaranteed to see. setup is the only thing that ever rewrites that env, and it is
// create-if-absent, so before this it could not. The interesting half of the test is
// the second one: converging config must not become a licence to clobber the operator
// edits create-if-absent exists to protect.
func TestEnsureSystemServersRefreshesDerivedEnv(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	// The login gate as a pre-re-domain install left it, plus one env nobody but a
	// human would have added and a memory bump off the built-in default.
	stale := func() *v1alpha1.MinecraftServer {
		ms, err := loginSystemServer("felis-limbo:demo", "minecraft",
			"http://old.internal:8081", "159.223.32.51.nip.io", "console.159.223.32.51.nip.io")
		if err != nil {
			t.Fatalf("build stale login server: %v", err)
		}
		ms.Spec.Env = append(ms.Spec.Env, v1alpha1.EnvVar{Name: "OPERATOR_TUNING", Value: "keep-me"})
		ms.Spec.JavaMemory = "768Mi"
		return ms
	}

	run := func(cl client.Client) []systemServerOutcome {
		return ensureSystemServers(ctx, cl, "minecraft", "felis-limbo:demo", "felis-lobby:demo",
			"http://felis-api-internal.felis.svc.cluster.local:8081",
			"mc.flyemoji.network", "console.mc.flyemoji.network")
	}

	envOf := func(t *testing.T, cl client.Client) map[string]string {
		t.Helper()
		var ms v1alpha1.MinecraftServer
		if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: naming.SystemLoginServer}, &ms); err != nil {
			t.Fatalf("get login server: %v", err)
		}
		got := make(map[string]string, len(ms.Spec.Env))
		for _, e := range ms.Spec.Env {
			got[e.Name] = e.Value
		}
		return got
	}

	t.Run("converges the console hostnames after a re-domain", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale()).Build()
		for _, out := range run(cl) {
			if out.err != nil {
				t.Fatalf("%s: %v", out.name, out.err)
			}
			if out.name == naming.SystemLoginServer && !strings.Contains(out.skipped, "refreshed") {
				t.Errorf("login outcome = %+v, want the refresh reported so setup's output "+
					"does not read as if nothing happened", out)
			}
		}
		env := envOf(t, cl)
		if env[envPanelHostname] != "console.mc.flyemoji.network" {
			t.Errorf("%s = %q — players are still being sent to the old console",
				envPanelHostname, env[envPanelHostname])
		}
		if env[envRootDomain] != "mc.flyemoji.network" {
			t.Errorf("%s = %q, want the new root domain", envRootDomain, env[envRootDomain])
		}
	})

	t.Run("leaves operator edits alone", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale()).Build()
		run(cl)

		env := envOf(t, cl)
		if env["OPERATOR_TUNING"] != "keep-me" {
			t.Error("an env var the operator added by hand was dropped; create-if-absent " +
				"exists so hand edits survive a re-run, and only config-derived names are ours")
		}
		var ms v1alpha1.MinecraftServer
		if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: naming.SystemLoginServer}, &ms); err != nil {
			t.Fatalf("get login server: %v", err)
		}
		if ms.Spec.JavaMemory != "768Mi" {
			t.Errorf("javaMemory = %q, want 768Mi — only env is converged, never the "+
				"rest of the spec", ms.Spec.JavaMemory)
		}
	})

	t.Run("reports no refresh when config already matches", func(t *testing.T) {
		fresh, err := loginSystemServer("felis-limbo:demo", "minecraft",
			"http://felis-api-internal.felis.svc.cluster.local:8081",
			"mc.flyemoji.network", "console.mc.flyemoji.network")
		if err != nil {
			t.Fatalf("build fresh login server: %v", err)
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fresh).Build()
		for _, out := range run(cl) {
			if out.name == naming.SystemLoginServer && strings.Contains(out.skipped, "refreshed") {
				t.Errorf("outcome = %+v — an unchanged install must not claim it rewrote "+
					"anything, or every setup run looks like a re-domain", out)
			}
		}
	})
}
