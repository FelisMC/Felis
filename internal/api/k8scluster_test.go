package api

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// K8sCluster is documented as integration-tested against a live cluster rather
// than covered by the hermetic suite, and for most of it that is the right call —
// merge-patch semantics are not worth faking. This one test departs from it
// deliberately: "CreateServer forgot to set spec.rcon" is precisely the kind of
// defect a live-cluster test catches only if someone runs it, and it shipped. It
// produced servers that reported Running, listed nobody online, and answered the
// console with 503, and the cause was a struct literal missing a field. A fake
// client is enough to pin a struct literal.
func TestCreateServerEnablesRcon(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	k := NewK8sCluster(c, "minecraft")

	if err := k.CreateServer(context.Background(), CreateServerInput{
		Name:        "survival",
		Subdomain:   "survival",
		DisplayName: "Survival",
		Image:       "reg/paper:1",
		JavaMemory:  "2G",
		StorageSize: "10Gi",
	}); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	var ms v1alpha1.MinecraftServer
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &ms); err != nil {
		t.Fatalf("get created server: %v", err)
	}

	if !ms.Spec.Rcon.Enabled {
		t.Fatal("spec.rcon.enabled is false: the operator will skip the readiness probe and " +
			"never sample a player count, and every console write will fail with 503")
	}
	// The name is the contract with internal/operator.ensureRconSecret, which
	// provisions the password against exactly this key. A mismatch leaves the pod
	// stuck on a secretKeyRef that nothing ever creates.
	if got, want := ms.Spec.Rcon.SecretRef.Name, naming.RconSecretName("survival"); got != want {
		t.Fatalf("rcon secretRef name = %q, want %q", got, want)
	}
	if got, want := ms.Spec.Rcon.SecretRef.Key, naming.RconSecretKey; got != want {
		t.Fatalf("rcon secretRef key = %q, want %q", got, want)
	}
	// felis-api holds secrets:get, not create — it must never try to mint the
	// password itself, and nothing here should carry one.
	if ms.Spec.Rcon.Port != 0 {
		t.Fatalf("rcon port = %d, want 0 so the operator default is the only copy", ms.Spec.Rcon.Port)
	}
}

// TestCreateServerDefaultsIdleStop pins the other half of "a server nobody plays
// on stops itself": the operator only idles out a server whose spec asks for
// it, so a create that leaves spec.idle empty ships a server that runs forever.
func TestCreateServerDefaultsIdleStop(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	k := NewK8sCluster(c, "minecraft")
	if err := k.CreateServer(context.Background(), CreateServerInput{
		Name: "survival", Subdomain: "survival", Image: "reg/paper:1", JavaMemory: "2G", StorageSize: "10Gi",
	}); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	info, err := k.GetServer(context.Background(), "survival")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if info.IdleStopSeconds != v1alpha1.DefaultEmptySecondsBeforeStop {
		t.Fatalf("idleStopSeconds = %d, want the default %d", info.IdleStopSeconds, v1alpha1.DefaultEmptySecondsBeforeStop)
	}
}

// TestPatchIdleStopKeepsTheChoiceVisible: turning idle stop off must leave a
// duration on the spec, because a spec with none at all is what converge fills
// with the default. Off followed by a converge must stay off.
func TestPatchIdleStopKeepsTheChoiceVisible(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	legacy := &v1alpha1.MinecraftServer{}
	legacy.Name, legacy.Namespace = "survival", "minecraft"
	legacy.Spec.Rcon.Enabled = true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
	k := NewK8sCluster(c, "minecraft")
	get := func() v1alpha1.IdleSpec {
		var ms v1alpha1.MinecraftServer
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &ms); err != nil {
			t.Fatalf("get: %v", err)
		}
		return ms.Spec.Idle
	}

	off := int32(0)
	if err := k.PatchServerSpec(context.Background(), "survival", ServerSpecPatch{IdleStopSeconds: &off}); err != nil {
		t.Fatalf("patch off: %v", err)
	}
	if got := get(); got.AutoStopEnabled || got.EmptySecondsBeforeStop <= 0 {
		t.Fatalf("idle after off = %+v, want disabled with a duration kept", got)
	}

	thirty := int32(1800)
	if err := k.PatchServerSpec(context.Background(), "survival", ServerSpecPatch{IdleStopSeconds: &thirty}); err != nil {
		t.Fatalf("patch on: %v", err)
	}
	if got := get(); !got.AutoStopEnabled || got.EmptySecondsBeforeStop != 1800 {
		t.Fatalf("idle after 1800 = %+v, want enabled at 1800", got)
	}
	info, err := k.GetServer(context.Background(), "survival")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if info.IdleStopSeconds != 1800 {
		t.Fatalf("view idleStopSeconds = %d, want 1800", info.IdleStopSeconds)
	}
}

// spyReader records the options of every List it serves.
type spyReader struct {
	client.Reader
	lists []*client.ListOptions
}

func (s *spyReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	lo := &client.ListOptions{}
	lo.ApplyOptions(opts)
	s.lists = append(s.lists, lo)
	return s.Reader.List(ctx, list, opts...)
}

func testServer(name, subdomain string) *v1alpha1.MinecraftServer {
	return &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "minecraft"},
		Spec:       v1alpha1.MinecraftServerSpec{Subdomain: subdomain, DesiredState: v1alpha1.DesiredRunning},
	}
}

// The fleet reads come from the informer cache, the subdomain lookup through its
// index, and everything that reads one server to act on it from the apiserver. The
// two fakes hold different fleets so each read shows which one it asked.
func TestFleetReadsComeFromTheServerCache(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	direct := fake.NewClientBuilder().WithScheme(scheme).WithObjects(testServer("fresh", "fresh")).Build()
	cached := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(testServer("alpha", "alpha"), testServer("beta", "beta")).
		WithIndex(&v1alpha1.MinecraftServer{}, SubdomainIndex, SubdomainOf).Build()
	spy := &spyReader{Reader: cached}
	synced := false
	k := NewK8sCluster(direct, "minecraft").WithServerCache(spy, func() bool { return synced })

	if err := k.Ping(ctx); err == nil || err.Error() != "MinecraftServer cache has not synced" {
		t.Fatalf("Ping before the cache synced = %v, want the not-synced error", err)
	}
	synced = true
	if err := k.Ping(ctx); err != nil {
		t.Fatalf("Ping once synced: %v", err)
	}

	infos, err := k.ListServers(ctx)
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	var names []string
	for _, i := range infos {
		names = append(names, i.Name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "alpha,beta" {
		t.Fatalf("ListServers = %s, want alpha,beta (the cache's fleet)", got)
	}
	info, err := k.GetBySubdomain(ctx, "beta")
	if err != nil || info.Name != "beta" {
		t.Fatalf("GetBySubdomain(beta) = %+v, %v; want beta", info, err)
	}
	if got := spy.lists[len(spy.lists)-1].FieldSelector.String(); got != "spec.subdomain=beta" {
		t.Fatalf("subdomain lookup selector = %q, want spec.subdomain=beta (served by the index)", got)
	}
	if _, err := k.GetBySubdomain(ctx, "fresh"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBySubdomain(fresh) = %v, want ErrNotFound (only the apiserver has it)", err)
	}

	if info, err := k.GetServer(ctx, "fresh"); err != nil || info.Name != "fresh" {
		t.Fatalf("GetServer(fresh) = %+v, %v; want it from the apiserver", info, err)
	}
	if _, err := k.GetServer(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetServer(alpha) = %v, want ErrNotFound (a single read never trusts the cache)", err)
	}
	if err := k.SetDesiredState(ctx, "fresh", v1alpha1.DesiredStopped); err != nil {
		t.Fatalf("SetDesiredState(fresh): %v", err)
	}
	var ms v1alpha1.MinecraftServer
	if err := direct.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "fresh"}, &ms); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if ms.Spec.DesiredState != v1alpha1.DesiredStopped {
		t.Fatalf("desiredState = %s, want Stopped", ms.Spec.DesiredState)
	}
	if err := k.SetDesiredState(ctx, "alpha", v1alpha1.DesiredStopped); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetDesiredState(alpha) = %v, want ErrNotFound (writes read the apiserver)", err)
	}
}

// Without a cache the subdomain lookup lists the namespace: the apiserver serves no
// field selector on a CRD, and the fake refuses one it has no index for.
func TestGetBySubdomainWithoutCache(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	k := NewK8sCluster(fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(testServer("alpha", "alpha"), testServer("beta", "survival")).Build(), "minecraft")
	info, err := k.GetBySubdomain(context.Background(), "survival")
	if err != nil || info.Name != "beta" {
		t.Fatalf("GetBySubdomain(survival) = %+v, %v; want beta", info, err)
	}
	if err := k.Ping(context.Background()); err != nil {
		t.Fatalf("Ping with no cache: %v", err)
	}
}

func TestSubdomainOf(t *testing.T) {
	if got := SubdomainOf(testServer("a", "survival")); len(got) != 1 || got[0] != "survival" {
		t.Fatalf("SubdomainOf = %v, want [survival]", got)
	}
	if got := SubdomainOf(testServer("a", "")); got != nil {
		t.Fatalf("SubdomainOf(no subdomain) = %v, want nil", got)
	}
}
