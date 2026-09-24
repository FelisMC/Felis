package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/imagepin"
	"felis.lolicon.best/internal/naming"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const pinTestDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

// TestPinUserServerImages pins exactly the user servers still on a platform tag,
// reports a tag the registry lost as an error without touching that server, and
// has nothing left to do on a second pass.
func TestPinUserServerImages(t *testing.T) {
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/felis/paper/manifests/demo" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", pinTestDigest)
	}))
	defer reg.Close()
	res := imagepin.Resolver{Registry: defaultRegistryURL, Endpoint: strings.TrimPrefix(reg.URL, "http://")}

	paper := defaultRegistryURL + "/felis/paper:demo"
	mk := func(name, image, role string) *v1alpha1.MinecraftServer {
		ms := &v1alpha1.MinecraftServer{}
		ms.Name, ms.Namespace = name, "minecraft"
		ms.Spec.Image = image
		if role != "" {
			ms.Labels = map[string]string{v1alpha1.LabelSystemRole: role}
		}
		return ms
	}
	cl := fake.NewClientBuilder().WithScheme(newSystemServerScheme(t)).WithObjects(
		mk("legacy", paper, ""),
		mk("pinned", paper+"@sha256:"+strings.Repeat("3", 64), ""),
		mk("external", "docker.io/itzg/minecraft-server:java21", ""),
		mk("gone", defaultRegistryURL+"/felis/paper:old", ""),
		mk(naming.SystemLobbyServer, defaultRegistryURL+"/felis/felis-lobby:demo", naming.SystemLobbyServer),
	).Build()
	ctx := context.Background()

	outcomes, err := pinUserServerImages(ctx, cl, "minecraft", res)
	if err != nil {
		t.Fatalf("pinUserServerImages: %v", err)
	}
	byName := map[string]systemServerOutcome{}
	for _, o := range outcomes {
		byName[o.name] = o
	}
	if len(outcomes) != 2 || byName["legacy"].err != nil || byName["gone"].err == nil {
		t.Fatalf("outcomes = %+v, want legacy pinned and gone reported", outcomes)
	}

	want := map[string]string{
		"legacy":                 paper + "@" + pinTestDigest,
		"pinned":                 paper + "@sha256:" + strings.Repeat("3", 64),
		"external":               "docker.io/itzg/minecraft-server:java21",
		"gone":                   defaultRegistryURL + "/felis/paper:old",
		naming.SystemLobbyServer: defaultRegistryURL + "/felis/felis-lobby:demo",
	}
	for name, image := range want {
		var ms v1alpha1.MinecraftServer
		if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: name}, &ms); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if ms.Spec.Image != image {
			t.Errorf("%s image = %q, want %q", name, ms.Spec.Image, image)
		}
	}

	again, err := pinUserServerImages(ctx, cl, "minecraft", res)
	if err != nil || len(again) != 1 || again[0].name != "gone" {
		t.Fatalf("second pass = %+v, %v; want only the unresolvable server again", again, err)
	}
}

// A fresh install has no CRD yet; the command must read that as nothing to pin.
func TestPinUserServerImagesNoCRD(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(newSystemServerScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "felis.lolicon.best", Kind: "MinecraftServer"}}
		},
	}).Build()
	_, err := pinUserServerImages(context.Background(), cl, "minecraft", imagepin.Resolver{Registry: defaultRegistryURL})
	if !meta.IsNoMatchError(err) {
		t.Fatalf("err = %v, want a NoMatch error the command can recognise", err)
	}
}

func TestLoopbackEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"registry.felis.svc:5000": "127.0.0.1:5000",
		"registry.example":        "127.0.0.1",
	} {
		if got := loopbackEndpoint(in); got != want {
			t.Errorf("loopbackEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
