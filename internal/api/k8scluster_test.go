package api

import (
	"context"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
