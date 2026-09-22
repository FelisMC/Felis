package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestResolveWorldDir pins the two world layouts the reaper must find, and the
// fail-closed miss. The stock local-path arm is derived from the live PVC's
// volumeName — a name-based guess (glob) could tar a stale deleted PV's bytes and
// then delete the current world, which is why it is read from the API instead.
func TestResolveWorldDir(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// Arrange a world under the documented <root>/<pvc> layout.
	named := filepath.Join(root, "world-named-0")
	if err := os.MkdirAll(named, 0o750); err != nil {
		t.Fatal(err)
	}
	// Arrange a second world the way k3s local-path stores it.
	pvDir := filepath.Join(root, "pvc-11111111-2222-3333-4444-555555555555_minecraft_world-live-0")
	if err := os.MkdirAll(pvDir, 0o750); err != nil {
		t.Fatal(err)
	}

	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "world-live-0", Namespace: "minecraft"},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: "pvc-11111111-2222-3333-4444-555555555555",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(haltScheme(t)).WithObjects(claim).Build()
	resolve := resolveWorldDir(ctx, cl, "minecraft", root)

	t.Run("documented name layout wins", func(t *testing.T) {
		got, err := resolve("world-named-0")
		if err != nil || got != named {
			t.Fatalf("resolve = (%q, %v), want (%q, nil)", got, err, named)
		}
	})

	t.Run("stock local-path layout resolves exactly", func(t *testing.T) {
		got, err := resolve("world-live-0")
		if err != nil || got != pvDir {
			t.Fatalf("resolve = (%q, %v), want (%q, nil)", got, err, pvDir)
		}
	})

	t.Run("neither layout present falls back to the documented path", func(t *testing.T) {
		// The claim exists but its directory does not: return the documented path so
		// the archive walk fails there, and the reaper preserves the world.
		missing := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "world-gone-0", Namespace: "minecraft"},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-99999999-0000-0000-0000-000000000000"},
		}
		cl := fake.NewClientBuilder().WithScheme(haltScheme(t)).WithObjects(missing).Build()
		got, err := resolveWorldDir(ctx, cl, "minecraft", root)("world-gone-0")
		if err != nil || got != filepath.Join(root, "world-gone-0") {
			t.Fatalf("resolve = (%q, %v), want (%q, nil)", got, err, filepath.Join(root, "world-gone-0"))
		}
	})

	t.Run("unknown pvc is an error, not a guess", func(t *testing.T) {
		_, err := resolve("world-unknown-0")
		if err == nil || !strings.Contains(err.Error(), "resolve world PVC world-unknown-0") {
			t.Fatalf("err = %v, want a resolve-world-PVC error", err)
		}
	})
}

// The pre-reap warner resolves the owner's VERIFIED email and hands the notice
// to the mailer. Every failure (no verified address, relay refusal) returns an
// error so the reaper retries on its next run instead of stamping a notice
// nobody received.
func TestMailWarner(t *testing.T) {
	lookup := func(email string, err error) func(context.Context, string) (string, error) {
		return func(context.Context, string) (string, error) { return email, err }
	}

	n := &captureNotifier{}
	w := &mailWarner{lookupEmail: lookup("owner@example.net", nil), notifier: n}
	if err := w.Warn(context.Background(), "u1", "survival", "3d"); err != nil {
		t.Fatalf("Warn: %v", err)
	}
	if n.email != "owner@example.net" || !strings.Contains(n.subject, "survival") || !strings.Contains(n.subject, "3d") {
		t.Fatalf("notice envelope = (%q, %q)", n.email, n.subject)
	}
	if !strings.Contains(n.body, "survival") || !strings.Contains(n.body, "3d") {
		t.Fatalf("body missing server/remaining:\n%s", n.body)
	}

	w = &mailWarner{lookupEmail: lookup("", errors.New("owner u2 has no verified email")), notifier: n}
	if err := w.Warn(context.Background(), "u2", "survival", "3d"); err == nil || !strings.Contains(err.Error(), "verified email") {
		t.Fatalf("unverified owner = %v, want the lookup error surfaced", err)
	}

	w = &mailWarner{lookupEmail: lookup("owner@example.net", nil), notifier: &captureNotifier{err: errors.New("relay down")}}
	if err := w.Warn(context.Background(), "u1", "survival", "3d"); err == nil || !strings.Contains(err.Error(), "relay down") {
		t.Fatalf("relay failure = %v, want it surfaced", err)
	}
}

type captureNotifier struct {
	email, subject, body string
	err                  error
}

func (n *captureNotifier) SendNotice(_ context.Context, email, subject, body string) error {
	if n.err != nil {
		return n.err
	}
	n.email, n.subject, n.body = email, subject, body
	return nil
}
