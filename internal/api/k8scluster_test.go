package api

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/naming"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPersistentMigrationBlocksWakeAndWorldOperations(t *testing.T) {
	scheme := runtime.NewScheme()
	v1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)
	batchv1.AddToScheme(scheme)
	s := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft", Annotations: map[string]string{maintenance.Annotation: maintenance.LockValue(maintenance.KindMigration, time.Now().Add(-24*time.Hour))}}, Spec: v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredStopped}, Status: v1alpha1.MinecraftServerStatus{Phase: v1alpha1.PhaseStopped}}
	s.Spec.Storage.ClaimName = "world-survival-migrated"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(s, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: s.WorldPVC(), Namespace: s.Namespace}}).Build()
	k := NewK8sCluster(c, s.Namespace)
	ctx := context.Background()
	if exists, err := k.WorldVolumeExists(ctx, s.Name); err != nil || !exists {
		t.Fatal("active volume ignored", err)
	}
	if err := k.SetDesiredState(ctx, s.Name, v1alpha1.DesiredRunning); !errors.Is(err, ErrMaintenanceInProgress) {
		t.Fatal("migration admitted wake", err)
	}
	for _, kind := range []string{maintenance.KindBackup, maintenance.KindFileWrite, maintenance.KindReap} {
		if err := k.AcquireMaintenance(ctx, s.Name, kind); !errors.Is(err, ErrMaintenanceInProgress) {
			t.Fatal("migration admitted", kind, err)
		}
	}
	if err := k.ReleaseMaintenance(ctx, s.Name); err != nil {
		t.Fatal(err)
	}
	var current v1alpha1.MinecraftServer
	c.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &current)
	if current.Annotations[maintenance.Annotation] == "" {
		t.Fatal("normal release cleared persistent migration lock")
	}
}

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

// The forwarding=legacy label reaches the proxy as legacyForwarding on the server
// list (#15); any other value, and no label, leaves the server on modern
// forwarding and keeps the key out of the JSON.
func TestServerListCarriesLegacyForwarding(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	labeled := func(name, value string) *v1alpha1.MinecraftServer {
		ms := testServer(name, name)
		ms.Labels = map[string]string{v1alpha1.LabelForwarding: value}
		return ms
	}
	k := NewK8sCluster(fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		labeled("legacy18", "legacy"), labeled("shouty", "LEGACY"), labeled("modern", "modern"),
		testServer("plain", "plain"),
	).Build(), "minecraft")
	infos, err := k.ListServers(context.Background())
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	for _, i := range infos {
		b, err := json.Marshal(i)
		if err != nil {
			t.Fatal(err)
		}
		want := i.Name == "legacy18"
		if i.LegacyForwarding != want || strings.Contains(string(b), `"legacyForwarding":true`) != want {
			t.Errorf("%s: legacyForwarding = %v, JSON %s; want %v", i.Name, i.LegacyForwarding, b, want)
		}
		if !want && strings.Contains(string(b), "legacyForwarding") {
			t.Errorf("%s: JSON carries legacyForwarding although it is off: %s", i.Name, b)
		}
	}
}

// A Failed server reaches the proxy and the panel with whether the operator will
// still retry it: the proxy keeps a waiting player through the restart backoff and
// lets them go once the retries are spent.
func TestServerInfoCarriesStartGaveUp(t *testing.T) {
	failed := func(name string, restarts int32) *v1alpha1.MinecraftServer {
		ms := testServer(name, name)
		now := metav1.Now()
		ms.Status = v1alpha1.MinecraftServerStatus{
			Phase:            v1alpha1.PhaseFailed,
			AutoRestarts:     restarts,
			StartRequestedAt: &now,
			Conditions: []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse,
				Reason: v1alpha1.ReasonStartupTimeout}},
		}
		return ms
	}
	retrying := serverInfo(failed("retrying", 1))
	if retrying.StartGaveUp || retrying.AutoRestarts != 1 {
		t.Fatalf("retrying: startGaveUp=%v autoRestarts=%d, want false 1", retrying.StartGaveUp, retrying.AutoRestarts)
	}
	spent := serverInfo(failed("spent", v1alpha1.MaxAutoRestarts))
	b, err := json.Marshal(spent)
	if err != nil {
		t.Fatal(err)
	}
	if !spent.StartGaveUp || !strings.Contains(string(b), `"startGaveUp":true`) {
		t.Fatalf("spent: startGaveUp=%v JSON %s, want true", spent.StartGaveUp, b)
	}
}

// A system server reaches the panel marked, so the console offers no give-up or
// delete the API would refuse; every other server leaves the field out.
func TestServerInfoCarriesReaperExempt(t *testing.T) {
	lobby := testServer("lobby", "lobby")
	lobby.Spec.ReaperExempt = true
	for _, tc := range []struct {
		ms   *v1alpha1.MinecraftServer
		want bool
	}{{lobby, true}, {testServer("plain", "plain"), false}} {
		info := serverInfo(tc.ms)
		b, err := json.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		if info.ReaperExempt != tc.want || strings.Contains(string(b), `"reaperExempt":true`) != tc.want ||
			(!tc.want && strings.Contains(string(b), "reaperExempt")) {
			t.Errorf("%s: reaperExempt = %v, JSON %s; want %v", tc.ms.Name, info.ReaperExempt, b, tc.want)
		}
	}
}

// An admin edits one resource at a time. The view hands the handler the whole
// pod block, the handler lays the change over it, and the merge patch keeps
// everything the admin left alone: memory-only keeps the CPU limit, CPU-only
// keeps the memory, and an emptied CPU field really drops the limit.
func TestResourcePatchesKeepWhatTheyLeaveOut(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	ms := testServer("survival", "survival")
	ms.Spec.JavaMemory = "3072M"
	ms.Spec.Resources = corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourceCPU: resource.MustParse("2")},
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourceCPU: resource.MustParse("500m")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ms).Build()
	k := NewK8sCluster(c, "minecraft")
	ctx := context.Background()
	str := func(s string) *string { return &s }
	// edit is one PATCH /servers/survival: read the view, merge, write.
	edit := func(memory *string, rr *patchResourceRequest) *ServerInfo {
		t.Helper()
		info, err := k.GetServer(ctx, "survival")
		if err != nil {
			t.Fatalf("GetServer: %v", err)
		}
		heap, moved, res, err := mergeResources(info.Resources, memory, rr)
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		p := ServerSpecPatch{Resources: &res}
		if memory != nil || moved {
			p.JavaMemory = &heap
		}
		if err := k.PatchServerSpec(ctx, "survival", p); err != nil {
			t.Fatalf("patch: %v", err)
		}
		after, err := k.GetServer(ctx, "survival")
		if err != nil {
			t.Fatalf("GetServer: %v", err)
		}
		return after
	}

	info, err := k.GetServer(ctx, "survival")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if info.Memory != "4Gi" || info.CPU != "2" || info.JavaMemory != "3072M" {
		t.Fatalf("view = memory %q cpu %q heap %q, want 4Gi 2 3072M", info.Memory, info.CPU, info.JavaMemory)
	}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "requests") || strings.Contains(string(b), "Resources") {
		t.Fatalf("the pod block reached the wire: %s", b)
	}

	after := edit(str("8Gi"), nil)
	cpuReq := after.Resources.Requests[corev1.ResourceCPU]
	if after.Memory != "8Gi" || after.CPU != "2" || cpuReq.String() != "500m" || after.JavaMemory != "6144M" {
		t.Fatalf("memory-only: memory %q cpu %q cpuRequest %s heap %q, want 8Gi 2 500m 6144M",
			after.Memory, after.CPU, cpuReq.String(), after.JavaMemory)
	}

	after = edit(nil, &patchResourceRequest{CPU: str("3")})
	memReq := after.Resources.Requests[corev1.ResourceMemory]
	if after.CPU != "3" || after.Memory != "8Gi" || memReq.String() != "8Gi" || after.JavaMemory != "6144M" {
		t.Fatalf("cpu-only: cpu %q memory %q memoryRequest %s heap %q, want 3 8Gi 8Gi 6144M",
			after.CPU, after.Memory, memReq.String(), after.JavaMemory)
	}

	after = edit(nil, &patchResourceRequest{CPU: str("")})
	if _, has := after.Resources.Limits[corev1.ResourceCPU]; has || after.CPU != "" || after.Memory != "8Gi" {
		t.Fatalf("cleared cpu: limits %v, want the CPU limit gone and memory 8Gi kept", after.Resources.Limits)
	}
}
