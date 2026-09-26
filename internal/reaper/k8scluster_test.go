package reaper

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/operator"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// HoldWorld against a fake API server, which honours resourceVersion on the
// optimistic-lock patches the lock is written with.

func holdServer(desired v1alpha1.DesiredState, phase v1alpha1.Phase) *v1alpha1.MinecraftServer {
	return &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft"},
		Spec:       v1alpha1.MinecraftServerSpec{DesiredState: desired},
		Status:     v1alpha1.MinecraftServerStatus{Phase: phase},
	}
}

// fakeClock is a clock the tests and the heartbeat goroutine share.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func holdCluster(t *testing.T, objs ...client.Object) (*K8sCluster, client.Client, *fakeClock) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.MinecraftServer{}).Build()
	clk := &fakeClock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	k := NewK8sCluster(c, "minecraft")
	k.now = clk.now
	k.beat = time.Hour
	return k, c, clk
}

func serverState(t *testing.T, c client.Client) (map[string]string, v1alpha1.DesiredState) {
	t.Helper()
	var ms v1alpha1.MinecraftServer
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &ms); err != nil {
		t.Fatalf("get: %v", err)
	}
	return ms.Annotations, ms.Spec.DesiredState
}

func TestHoldWorldStopsARunningServer(t *testing.T) {
	k, c, _ := holdCluster(t, holdServer(v1alpha1.DesiredRunning, v1alpha1.PhaseRunning))
	_, _, err := k.HoldWorld(context.Background(), "survival")
	if !errors.Is(err, ErrNotQuiet) {
		t.Fatalf("HoldWorld = %v, want ErrNotQuiet", err)
	}
	ann, desired := serverState(t, c)
	if desired != v1alpha1.DesiredStopped || ann[maintenance.Annotation] != "" {
		t.Fatalf("desired=%q annotations=%v: want told to stop and no lock yet", desired, ann)
	}
}

func TestHoldWorldWaitsUntilQuiet(t *testing.T) {
	ready := holdServer(v1alpha1.DesiredStopped, v1alpha1.PhaseStopped)
	ready.Status.Ready = true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "survival-0", Namespace: "minecraft",
		Labels: map[string]string{v1alpha1.LabelServer: "survival", v1alpha1.LabelComponent: gamePodComponent}}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "restore-survival", Namespace: "minecraft",
		Labels: map[string]string{maintenance.LabelServer: "survival", maintenance.LabelManagedBy: "felis-restore"}}}
	locked := holdServer(v1alpha1.DesiredStopped, v1alpha1.PhaseStopped)
	locked.Annotations = map[string]string{
		maintenance.Annotation: maintenance.LockValue(maintenance.KindBackup, time.Date(2026, 9, 24, 11, 59, 30, 0, time.UTC)),
	}
	for _, tc := range []struct {
		name string
		objs []client.Object
		why  string
	}{
		{"stopping", []client.Object{holdServer(v1alpha1.DesiredStopped, v1alpha1.PhaseStopping)}, "still stopping"},
		{"ready", []client.Object{ready}, "still stopping"},
		{"game pod", []client.Object{holdServer(v1alpha1.DesiredStopped, v1alpha1.PhaseStopped), pod}, "game pod"},
		{"restore job", []client.Object{holdServer(v1alpha1.DesiredStopped, v1alpha1.PhaseStopped), job}, "a restore"},
		{"admission lock", []client.Object{locked}, "a backup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, c, _ := holdCluster(t, tc.objs...)
			before, _ := serverState(t, c)
			_, _, err := k.HoldWorld(context.Background(), "survival")
			if !errors.Is(err, ErrNotQuiet) || !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("HoldWorld = %v, want ErrNotQuiet naming %q", err, tc.why)
			}
			if after, _ := serverState(t, c); after[maintenance.Annotation] != before[maintenance.Annotation] {
				t.Fatalf("lock changed to %q", after[maintenance.Annotation])
			}
		})
	}
}

// Once held, every other holder check sees the reaper, and release lets go.
func TestHoldWorldLocksAndReleases(t *testing.T) {
	k, c, clk := holdCluster(t, holdServer(v1alpha1.DesiredStopped, v1alpha1.PhaseStopped))
	held, release, err := k.HoldWorld(context.Background(), "survival")
	if err != nil {
		t.Fatalf("HoldWorld: %v", err)
	}
	ann, _ := serverState(t, c)
	if kind, ok := maintenance.Holder("survival", ann, nil, clk.now()); !ok || kind != maintenance.KindReap {
		t.Fatalf("Holder = (%q, %v) with %v, want the reaper", kind, ok, ann)
	}
	release()
	if held.Err() == nil {
		t.Fatal("held context still live after release")
	}
	if ann, _ := serverState(t, c); ann[maintenance.Annotation] != "" {
		t.Fatalf("lock left behind: %v", ann)
	}
}

// The lock is rewritten while held, so it never lapses under a long archive.
func TestHoldWorldKeepsLockFresh(t *testing.T) {
	k, c, clk := holdCluster(t, holdServer(v1alpha1.DesiredStopped, v1alpha1.PhaseStopped))
	k.beat = 5 * time.Millisecond
	held, release, err := k.HoldWorld(context.Background(), "survival")
	if err != nil {
		t.Fatalf("HoldWorld: %v", err)
	}
	defer release()
	clk.add(10 * maintenance.Grace)
	want := maintenance.LockValue(maintenance.KindReap, clk.now())
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ann, _ := serverState(t, c); ann[maintenance.Annotation] == want {
			break
		}
		if time.Now().After(deadline) {
			ann, _ := serverState(t, c)
			t.Fatalf("lock = %q, want rewritten to %q", ann[maintenance.Annotation], want)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if held.Err() != nil {
		t.Fatalf("held ended: %v", context.Cause(held))
	}
}

// A lock someone else removed (or replaced) ends the hold: the reaper must not
// delete a world it no longer holds.
func TestHoldWorldEndsWhenLockIsLost(t *testing.T) {
	k, c, _ := holdCluster(t, holdServer(v1alpha1.DesiredStopped, v1alpha1.PhaseStopped))
	k.beat = 5 * time.Millisecond
	held, release, err := k.HoldWorld(context.Background(), "survival")
	if err != nil {
		t.Fatalf("HoldWorld: %v", err)
	}
	var ms v1alpha1.MinecraftServer
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &ms); err != nil {
		t.Fatalf("get: %v", err)
	}
	patch := client.MergeFrom(ms.DeepCopy())
	ms.Annotations[maintenance.Annotation] = maintenance.LockValue(maintenance.KindRestore, time.Now())
	if err := c.Patch(context.Background(), &ms, patch); err != nil {
		t.Fatalf("patch: %v", err)
	}
	select {
	case <-held.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("held context still live after the lock was taken over")
	}
	if !errors.Is(context.Cause(held), errLockLost) {
		t.Fatalf("cause = %v, want errLockLost", context.Cause(held))
	}
	release()
	if ann, _ := serverState(t, c); !strings.HasPrefix(ann[maintenance.Annotation], maintenance.KindRestore+"@") {
		t.Fatalf("release removed another holder's lock: %v", ann)
	}
}

func TestWorldExists(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "world-survival-0", Namespace: "minecraft"}}
	k, _, _ := holdCluster(t, pvc)
	for name, want := range map[string]bool{"world-survival-0": true, "world-other-0": false} {
		if got, err := k.WorldExists(context.Background(), name); err != nil || got != want {
			t.Errorf("WorldExists(%s) = (%v, %v), want %v", name, got, err, want)
		}
	}
}

// gamePodComponent is a copy of the operator's pod label value; a drift would
// let the reaper archive a world beside a server still saving it.
func TestGamePodComponentMatchesOperator(t *testing.T) {
	if gamePodComponent != operator.ComponentValue {
		t.Fatalf("gamePodComponent = %q, operator labels its pods %q", gamePodComponent, operator.ComponentValue)
	}
}

// DeleteServer removes the MinecraftServer Inspect returned, and nothing that
// merely carries its name: a server made again under the name (another uid)
// stays, and one already gone is not an error.
func TestDeleteServerRemovesOnlyTheInspectedServer(t *testing.T) {
	ms := holdServer(v1alpha1.DesiredStopped, v1alpha1.PhaseStopped)
	ms.UID = "uid-1"
	k, c, _ := holdCluster(t, ms)
	ctx := context.Background()

	crd, err := k.Inspect(ctx, "survival")
	if err != nil || crd.UID != "uid-1" {
		t.Fatalf("Inspect = %+v, %v; want uid-1", crd, err)
	}

	if err := k.DeleteServer(ctx, "survival", "uid-0"); err == nil {
		t.Fatal("DeleteServer with another uid succeeded")
	}
	var got v1alpha1.MinecraftServer
	if err := c.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &got); err != nil {
		t.Fatalf("the server of another uid was deleted: %v", err)
	}

	if err := k.DeleteServer(ctx, "survival", crd.UID); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &got); err == nil {
		t.Fatal("the server is still there")
	}
	if err := k.DeleteServer(ctx, "survival", crd.UID); err != nil {
		t.Fatalf("DeleteServer of a server already gone = %v, want nil", err)
	}
}
