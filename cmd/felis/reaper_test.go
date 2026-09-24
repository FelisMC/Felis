package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/reaper"
)

// TestReportReaperRunFailsTheJob: a run that could not process a server, or
// could not remove an expired backup, exits 1 so the Job shows as failed.
func TestReportReaperRunFailsTheJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		sum  reaper.Summary
		want int
	}{
		{"clean", reaper.Summary{Evaluated: 3, WorldsReaped: 1, AwaitingOffsite: 1}, 0},
		{"server failed", reaper.Summary{Evaluated: 3, Skipped: 1}, 1},
		{"store full", reaper.Summary{Evaluated: 3, Skipped: 1, StoreFull: 1}, 1},
		{"expiry failed", reaper.Summary{Evaluated: 3, ExpireFailed: 2}, 1},
		{"corrupt archive", reaper.Summary{Evaluated: 3, Verified: 4, Corrupt: 1}, 1},
		{"read-back failed", reaper.Summary{Evaluated: 3, VerifyFailed: 1}, 1},
		{"sweep failed", reaper.Summary{Evaluated: 3, SweepFailed: true}, 1},
		{"orphans kept", reaper.Summary{Evaluated: 3, Swept: 2, OrphanArchives: 1}, 0},
	} {
		var out, errb bytes.Buffer
		if got := reportReaperRun(tc.sum, &out, &errb); got != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.name, got, tc.want)
		}
		if !strings.Contains(out.String(), "skipped=") || !strings.Contains(out.String(), "expire_failed=") {
			t.Errorf("%s: summary line = %q", tc.name, out.String())
		}
		if (tc.want == 1) != (errb.Len() > 0) {
			t.Errorf("%s: stderr = %q", tc.name, errb.String())
		}
	}
}

// TestResolveWorldDir pins the two world layouts the reaper must find, and the
// fail-closed miss. The stock local-path arm is derived from the live PVC's
// volumeName — a name-based guess (glob) could tar a stale deleted PV's bytes and
// then delete the current world, which is why it is read from the API instead.
// TestReaperConfigManualKeys: the on-demand backup keys default to 30 days,
// five per server and a ten-minute cooldown, accept overrides, and refuse
// values that would keep nothing or throttle backwards.
func TestReaperConfigManualKeys(t *testing.T) {
	rc, err := reaperConfig(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if rc.ManualRetention != 30*reaper.Day || rc.ManualKeep != 5 || rc.ManualCooldown != 10*time.Minute {
		t.Fatalf("defaults = %v / %d / %v", rc.ManualRetention, rc.ManualKeep, rc.ManualCooldown)
	}
	rc, err = reaperConfig(&config.Config{Archive: config.ArchiveConfig{
		ManualRetention: "7d", ManualKeep: 2, ManualCooldown: "0s"}})
	if err != nil {
		t.Fatal(err)
	}
	if rc.ManualRetention != 7*reaper.Day || rc.ManualKeep != 2 || rc.ManualCooldown != 0 {
		t.Fatalf("overrides = %v / %d / %v", rc.ManualRetention, rc.ManualKeep, rc.ManualCooldown)
	}
	for _, bad := range []config.ArchiveConfig{
		{ManualRetention: "0d"},
		{ManualRetention: "soon"},
		{ManualKeep: -1},
		{ManualCooldown: "-5m"},
		{ManualCooldown: "often"},
	} {
		if _, err := reaperConfig(&config.Config{Archive: bad}); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}

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
