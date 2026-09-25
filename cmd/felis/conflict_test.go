package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/imagepin"
	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// racingClient lands one concurrent write (race) on the stored object just before
// setup's first write of it goes out, the way the operator's status update or
// felis-api's idle patch can, and counts setup's writes.
func racingClient(t *testing.T, race func(ctx context.Context, c client.WithWatch), objs ...client.Object) (client.Client, *int) {
	t.Helper()
	writes := 0
	before := func(ctx context.Context, c client.WithWatch) {
		writes++
		if writes == 1 {
			race(ctx, c)
		}
	}
	cl := fake.NewClientBuilder().WithScheme(newSystemServerScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.MinecraftServer{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				before(ctx, c)
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				before(ctx, c)
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	return cl, &writes
}

func staleLoginGate(t *testing.T) *v1alpha1.MinecraftServer {
	t.Helper()
	ms, err := loginSystemServer("felis-limbo:demo", "minecraft",
		"http://old.internal:8081", "203.0.113.10.nip.io", "console.203.0.113.10.nip.io")
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func getLogin(t *testing.T, cl client.Client) *v1alpha1.MinecraftServer {
	t.Helper()
	var ms v1alpha1.MinecraftServer
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "minecraft", Name: naming.SystemLoginServer}, &ms); err != nil {
		t.Fatal(err)
	}
	return &ms
}

func envMap(ms *v1alpha1.MinecraftServer) map[string]string {
	m := map[string]string{}
	for _, e := range ms.Spec.Env {
		m[e.Name] = e.Value
	}
	return m
}

// A hand edit that lands while setup refreshes the console hostnames costs setup a
// re-read and a second write; both the edit and the refresh survive.
func TestRefreshDerivedEnvRetriesAConcurrentEdit(t *testing.T) {
	ctx := context.Background()
	cl, writes := racingClient(t, func(ctx context.Context, c client.WithWatch) {
		ms := getLogin(t, c)
		ms.Spec.Env = append(ms.Spec.Env, v1alpha1.EnvVar{Name: "HAND_TUNED", Value: "1"})
		if err := c.Update(ctx, ms); err != nil {
			t.Fatal(err)
		}
	}, staleLoginGate(t))

	desired, err := loginSystemServer("felis-limbo:demo", "minecraft",
		"http://felis-api-internal.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net")
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := refreshDerivedEnv(ctx, cl, getLogin(t, cl), desired)
	if err != nil || !refreshed {
		t.Fatalf("refreshed=%v err=%v, want a refresh after the retry", refreshed, err)
	}
	env := envMap(getLogin(t, cl))
	if env[envPanelHostname] != "console.mc.example.net" || env[envRootDomain] != "mc.example.net" {
		t.Errorf("env = %v, want the new hostnames", env)
	}
	if env["HAND_TUNED"] != "1" {
		t.Errorf("env = %v: the concurrent hand edit was dropped", env)
	}
	if *writes != 2 {
		t.Errorf("writes = %d, want 2 (one conflict, one retry)", *writes)
	}
}

// converge reports each fill once even when a status write forced a retry.
func TestConvergeRetriesAConcurrentStatusWrite(t *testing.T) {
	ctx := context.Background()
	lobby, err := lobbySystemServer("reg/lobby:1", "minecraft")
	if err != nil {
		t.Fatal(err)
	}
	lobby.Spec.Rcon = v1alpha1.RconSpec{}
	cl, writes := racingClient(t, func(ctx context.Context, c client.WithWatch) {
		var ms v1alpha1.MinecraftServer
		if err := c.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: naming.SystemLobbyServer}, &ms); err != nil {
			t.Fatal(err)
		}
		ms.Status.Phase = v1alpha1.PhaseRunning
		if err := c.Status().Update(ctx, &ms); err != nil {
			t.Fatal(err)
		}
	}, lobby)

	var got systemServerOutcome
	for _, o := range convergeSystemServers(ctx, cl, "minecraft", "", "reg/lobby:1",
		"http://felis-api-internal.felis.svc.cluster.local:8081", "mc.example.net", "console.mc.example.net") {
		if o.name == naming.SystemLobbyServer {
			got = o
		}
	}
	if got.err != nil || !got.updated || !slices.Equal(got.changes, []string{"spec.rcon"}) {
		t.Fatalf("lobby outcome = %+v, want spec.rcon filled once", got)
	}
	var ms v1alpha1.MinecraftServer
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: naming.SystemLobbyServer}, &ms); err != nil {
		t.Fatal(err)
	}
	if !ms.Spec.Rcon.Enabled || ms.Status.Phase != v1alpha1.PhaseRunning {
		t.Errorf("rcon.enabled=%v phase=%q, want the fill and the status write both kept", ms.Spec.Rcon.Enabled, ms.Status.Phase)
	}
	if *writes != 2 {
		t.Errorf("writes = %d, want 2", *writes)
	}
}

// A replica refresh racing another writer keeps that writer's key.
func TestSecretReplicaRefreshRetriesAConcurrentWrite(t *testing.T) {
	secret := func(ns, body string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "felis-config", Namespace: ns},
			Data: map[string][]byte{"felis.toml": []byte(body)}}
	}
	cl, writes := racingClient(t, func(ctx context.Context, c client.WithWatch) {
		var s corev1.Secret
		if err := c.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: "felis-config"}, &s); err != nil {
			t.Fatal(err)
		}
		s.Data["extra"] = []byte("x")
		if err := c.Update(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}, secret("felis", "current"), secret("minecraft", "stale"))

	out := ensureSecretReplica(context.Background(), cl, "felis", "minecraft",
		"felis-config", "felis.toml", "config", "minecraft ns", true)
	if out.err != nil || !out.updated {
		t.Fatalf("outcome = %+v, want refreshed", out)
	}
	var s corev1.Secret
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "minecraft", Name: "felis-config"}, &s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data["felis.toml"]) != "current" || string(s.Data["extra"]) != "x" {
		t.Errorf("replica data = %q, want felis.toml=current and extra=x", s.Data)
	}
	if *writes != 2 {
		t.Errorf("writes = %d, want 2", *writes)
	}
}

const (
	sysOldDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	sysNewDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

func systemPinRegistry(t *testing.T) imagepin.Resolver {
	t.Helper()
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/felis/limbo/manifests/demo", "/v2/felis/lobby/manifests/demo":
			w.Header().Set("Docker-Content-Digest", sysNewDigest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(reg.Close)
	return imagepin.Resolver{Registry: defaultRegistryURL, Endpoint: strings.TrimPrefix(reg.URL, "http://")}
}

func systemServer(name, image string) *v1alpha1.MinecraftServer {
	ms := &v1alpha1.MinecraftServer{}
	ms.Name, ms.Namespace = name, "minecraft"
	ms.Labels = map[string]string{v1alpha1.LabelSystemRole: name}
	ms.Spec.Image = image
	return ms
}

// The installer's --system pin moves a system server onto the build its tag names
// now, from the bare tag or from an earlier digest, and is a no-op the second time.
func TestPinSystemServerImage(t *testing.T) {
	ctx := context.Background()
	res := systemPinRegistry(t)
	limbo := defaultRegistryURL + "/felis/limbo:demo"
	lobby := defaultRegistryURL + "/felis/lobby:demo"
	cl := fake.NewClientBuilder().WithScheme(newSystemServerScheme(t)).WithObjects(
		systemServer(naming.SystemLoginServer, limbo),
		systemServer(naming.SystemLobbyServer, lobby+"@"+sysOldDigest),
	).Build()
	image := func(name string) string {
		var ms v1alpha1.MinecraftServer
		if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: name}, &ms); err != nil {
			t.Fatal(err)
		}
		return ms.Spec.Image
	}

	for name, want := range map[string]string{
		naming.SystemLoginServer: limbo + "@" + sysNewDigest,
		naming.SystemLobbyServer: lobby + "@" + sysNewDigest,
	} {
		o, err := pinSystemServerImage(ctx, cl, "minecraft", name, res)
		if err != nil || o.err != nil || !o.updated {
			t.Fatalf("%s: outcome=%+v err=%v, want pinned", name, o, err)
		}
		if got := image(name); got != want {
			t.Errorf("%s image = %q, want %q", name, got, want)
		}
		again, err := pinSystemServerImage(ctx, cl, "minecraft", name, res)
		if err != nil || again.updated || again.skipped != "already runs "+want {
			t.Errorf("%s second pass = %+v, %v; want already runs %s", name, again, err, want)
		}
	}
}

func TestPinSystemServerImageLeavesOthersAlone(t *testing.T) {
	ctx := context.Background()
	res := systemPinRegistry(t)
	pin := func(t *testing.T, ms *v1alpha1.MinecraftServer) (systemServerOutcome, string) {
		t.Helper()
		cl := fake.NewClientBuilder().WithScheme(newSystemServerScheme(t)).WithObjects(ms).Build()
		o, err := pinSystemServerImage(ctx, cl, "minecraft", naming.SystemLoginServer, res)
		if err != nil {
			t.Fatal(err)
		}
		var got v1alpha1.MinecraftServer
		if err := cl.Get(ctx, client.ObjectKeyFromObject(ms), &got); err != nil {
			t.Fatal(err)
		}
		return o, got.Spec.Image
	}

	t.Run("external image", func(t *testing.T) {
		o, img := pin(t, systemServer(naming.SystemLoginServer, "docker.io/example/limbo:1.2"))
		if o.err != nil || o.updated || img != "docker.io/example/limbo:1.2" ||
			o.skipped != "runs docker.io/example/limbo:1.2, which names no platform registry tag to follow; left alone" {
			t.Fatalf("outcome=%+v image=%q, want left alone", o, img)
		}
	})
	t.Run("digest without a tag", func(t *testing.T) {
		ref := defaultRegistryURL + "/felis/limbo@" + sysOldDigest
		o, img := pin(t, systemServer(naming.SystemLoginServer, ref))
		if o.err != nil || o.updated || img != ref {
			t.Fatalf("outcome=%+v image=%q, want left alone", o, img)
		}
	})
	t.Run("not a system server", func(t *testing.T) {
		ms := systemServer(naming.SystemLoginServer, defaultRegistryURL+"/felis/limbo:demo")
		ms.Labels = nil
		o, img := pin(t, ms)
		if o.err == nil || img != defaultRegistryURL+"/felis/limbo:demo" {
			t.Fatalf("outcome=%+v image=%q, want refused", o, img)
		}
	})
	t.Run("tag the registry lost", func(t *testing.T) {
		o, img := pin(t, systemServer(naming.SystemLoginServer, defaultRegistryURL+"/felis/limbo:gone"))
		if o.err == nil || img != defaultRegistryURL+"/felis/limbo:gone" {
			t.Fatalf("outcome=%+v image=%q, want an error the installer falls back on", o, img)
		}
	})
	t.Run("absent", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(newSystemServerScheme(t)).Build()
		o, err := pinSystemServerImage(ctx, cl, "minecraft", naming.SystemLoginServer, res)
		if err != nil || o.err != nil || o.skipped != "not present yet; sudo felis setup creates it" {
			t.Fatalf("outcome=%+v err=%v, want a skip", o, err)
		}
	})
	t.Run("retargeted while pinning", func(t *testing.T) {
		cl, _ := racingClient(t, func(ctx context.Context, c client.WithWatch) {
			ms := getLogin(t, c)
			ms.Spec.Image = "docker.io/example/limbo:1.2"
			if err := c.Update(ctx, ms); err != nil {
				t.Fatal(err)
			}
		}, systemServer(naming.SystemLoginServer, defaultRegistryURL+"/felis/limbo:demo"))
		o, err := pinSystemServerImage(ctx, cl, "minecraft", naming.SystemLoginServer, res)
		if err != nil || o.err != nil || o.updated {
			t.Fatalf("outcome=%+v err=%v, want nothing written", o, err)
		}
		if img := getLogin(t, cl).Spec.Image; img != "docker.io/example/limbo:1.2" {
			t.Errorf("image = %q, want the admin's retarget kept", img)
		}
	})
}

// --system names one of the two system servers; anything else is a usage error
// before any cluster is touched.
func TestPinImagesSystemFlagTakesOnlySystemServers(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := cmdPinImages([]string{"--system", "survival"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if got := stderr.String(); got != "felis pin-images: --system takes login or lobby, not \"survival\"\n" {
		t.Errorf("stderr = %q", got)
	}
}

// An idle setting the panel saves while converge fills the default is kept.
func TestConvergeIdleKeepsAConcurrentPanelEdit(t *testing.T) {
	ctx := context.Background()
	srv := &v1alpha1.MinecraftServer{}
	srv.Name, srv.Namespace = "survival", "minecraft"
	cl, writes := racingClient(t, func(ctx context.Context, c client.WithWatch) {
		var ms v1alpha1.MinecraftServer
		if err := c.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: "survival"}, &ms); err != nil {
			t.Fatal(err)
		}
		ms.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: false, EmptySecondsBeforeStop: 1800}
		if err := c.Update(ctx, &ms); err != nil {
			t.Fatal(err)
		}
	}, srv)

	if out := convergeUserServerIdle(ctx, cl, "minecraft"); len(out) != 0 {
		t.Fatalf("outcomes = %+v, want none: the server has a setting by the time converge writes", out)
	}
	var ms v1alpha1.MinecraftServer
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: "survival"}, &ms); err != nil {
		t.Fatal(err)
	}
	if want := (v1alpha1.IdleSpec{AutoStopEnabled: false, EmptySecondsBeforeStop: 1800}); ms.Spec.Idle != want {
		t.Errorf("idle = %+v, want the panel's %+v", ms.Spec.Idle, want)
	}
	if *writes != 1 {
		t.Errorf("writes = %d, want 1 (the conflicted attempt; the retry sends nothing)", *writes)
	}
}
