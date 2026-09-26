package operator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

// podAt is survival-0 as the StatefulSet controller made it from revision rev.
func podAt(rev string) *corev1.Pod {
	p := gamePod()
	p.UID = types.UID("pod-" + rev)
	p.Labels = map[string]string{appsv1.ControllerRevisionHashLabelKey: rev}
	return p
}

func withReady(p *corev1.Pod, status corev1.ConditionStatus) *corev1.Pod {
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return p
}

// observeTemplate stands in for the StatefulSet controller: it has seen the
// current template, which it names revision rev, and the pod is not ready.
func observeTemplate(t *testing.T, c client.Client, rev string) {
	t.Helper()
	sts := getSTS(t, c, "survival")
	sts.Status.ObservedGeneration = sts.Generation
	sts.Status.UpdateRevision = rev
	sts.Status.Replicas = 1
	sts.Status.ReadyReplicas = 0
	if err := c.Status().Update(context.Background(), sts); err != nil {
		t.Fatalf("update sts status: %v", err)
	}
}

func editImage(t *testing.T, c client.Client, image string) {
	t.Helper()
	s := getServer(t, c, "survival")
	s.Spec.Image = image
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatalf("edit image: %v", err)
	}
}

func readyReason(s *v1alpha1.MinecraftServer) string {
	if c := meta.FindStatusCondition(s.Status.Conditions, v1alpha1.ConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

// A server that gave up on a broken image is not stuck on it: once its spec is
// corrected, the pod still crash-looping on the old template is recreated from
// the new one, and the corrected start gets its own timeout and restarts.
func TestSpecChangeReplacesAPodThatGaveUp(t *testing.T) {
	srv := runningServer()
	srv.Spec.Startup.TimeoutSeconds = 30
	r, c := newReconciler(t, fakeProber{}, srv, rconSecret(), podAt("survival-old"))
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	clock := base
	r.Now = func() metav1.Time { return metav1.NewTime(clock) }
	at := func(offset time.Duration) *v1alpha1.MinecraftServer {
		clock = base.Add(offset)
		reconcile(t, r, "survival")
		return getServer(t, c, "survival")
	}

	// Spend the three automatic restarts (autorestart_test.go pins the schedule),
	// each pod coming back on the same broken template.
	at(0)
	observeTemplate(t, c, "survival-old")
	for _, due := range []time.Duration{90 * time.Second, 240 * time.Second, 510 * time.Second} {
		at(due)
		if err := c.Create(context.Background(), podAt("survival-old")); err != nil {
			t.Fatal(err)
		}
	}
	s := at(time.Hour)
	if !v1alpha1.StartGaveUp(&s.Status) {
		t.Fatalf("setup: not given up: phase=%s restarts=%d", s.Status.Phase, s.Status.AutoRestarts)
	}

	// The admin corrects the image; the StatefulSet takes the new template but,
	// the pod never having been ready, leaves the pod as it is.
	editImage(t, c, "registry.internal/felis/paper:fixed")
	s = at(time.Hour + time.Second)
	if !podPresent(t, c) {
		t.Fatal("the pod was deleted before the StatefulSet had observed the new template")
	}
	if img := getSTS(t, c, "survival").Spec.Template.Spec.Containers[0].Image; img != "registry.internal/felis/paper:fixed" {
		t.Fatalf("setup: template image = %q", img)
	}
	observeTemplate(t, c, "survival-new")

	s = at(time.Hour + 2*time.Second)
	if podPresent(t, c) {
		t.Fatal("the pod on the old template was left in place")
	}
	if s.Status.Phase != v1alpha1.PhaseStarting || readyReason(s) != "SpecChanged" {
		t.Fatalf("phase=%s reason=%q, want Starting SpecChanged", s.Status.Phase, readyReason(s))
	}
	if s.Status.AutoRestarts != 0 {
		t.Fatalf("autoRestarts = %d, want the budget back at 0", s.Status.AutoRestarts)
	}
	if s.Status.StartRequestedAt == nil || !s.Status.StartRequestedAt.Time.Equal(clock) {
		t.Fatalf("startRequestedAt = %v, want the start re-anchored at %v", s.Status.StartRequestedAt, clock)
	}

	// The StatefulSet recreates the pod from the new revision; that one is left
	// to start, and the new start times out on its own clock.
	if err := c.Create(context.Background(), podAt("survival-new")); err != nil {
		t.Fatal(err)
	}
	s = at(time.Hour + 20*time.Second)
	if !podPresent(t, c) || s.Status.Phase != v1alpha1.PhaseStarting {
		t.Fatalf("pod present=%v phase=%s, want the new pod kept while it starts", podPresent(t, c), s.Status.Phase)
	}
	s = at(time.Hour + 33*time.Second)
	if s.Status.Phase != v1alpha1.PhaseFailed || s.Status.AutoRestarts != 0 {
		t.Fatalf("phase=%s restarts=%d, want Failed with the budget untouched at the new timeout", s.Status.Phase, s.Status.AutoRestarts)
	}
}

// Which pods a Starting server's pass replaces once the StatefulSet has a newer
// template, and which it leaves alone.
func TestStalePodReplacement(t *testing.T) {
	terminating := podAt("survival-old")
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 6, 25, 11, 59, 0, 0, time.UTC)}
	terminating.Finalizers = []string{"test.felis/hold"}

	for _, tc := range []struct {
		name     string
		pod      *corev1.Pod
		observed bool
		replaced bool
	}{
		{"old template, not ready", podAt("survival-old"), true, true},
		{"old template, readiness false", withReady(podAt("survival-old"), corev1.ConditionFalse), true, true},
		{"current template", podAt("survival-new"), true, false},
		{"old template but ready: the StatefulSet rolls it", withReady(podAt("survival-old"), corev1.ConditionTrue), true, false},
		{"already terminating", terminating, true, false},
		{"template not yet observed", podAt("survival-old"), false, false},
		{"no pod", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{runningServer(), rconSecret()}
			if tc.pod != nil {
				objs = append(objs, tc.pod.DeepCopy())
			}
			r, c := newReconciler(t, fakeProber{}, objs...)
			reconcile(t, r, "survival")
			observeTemplate(t, c, "survival-new")
			if !tc.observed {
				sts := getSTS(t, c, "survival")
				sts.Generation = 2
				if err := c.Update(context.Background(), sts); err != nil {
					t.Fatal(err)
				}
				sts = getSTS(t, c, "survival")
				if sts.Status.ObservedGeneration >= sts.Generation {
					t.Fatalf("setup: observedGeneration %d is not behind generation %d", sts.Status.ObservedGeneration, sts.Generation)
				}
			}

			res := reconcile(t, r, "survival")
			s := getServer(t, c, "survival")
			if gone := tc.pod != nil && !podPresent(t, c); gone != tc.replaced {
				t.Fatalf("pod deleted = %v, want %v", gone, tc.replaced)
			}
			want := "PodNotReady"
			if tc.replaced {
				want = "SpecChanged"
			}
			if readyReason(s) != want || s.Status.Phase != v1alpha1.PhaseStarting {
				t.Fatalf("phase=%s reason=%q, want Starting %s", s.Status.Phase, readyReason(s), want)
			}
			if res.RequeueAfter != 2*time.Second {
				t.Fatalf("requeue = %v, want the Starting pace", res.RequeueAfter)
			}
		})
	}
}

// noCachedPods stands in for the manager's cached client, which has no pod
// informer: any pod read through it fails.
type noCachedPods struct{ client.Client }

func (c noCachedPods) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		return errors.New("pods are not cached")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// The pod is read through Reconciler.Pods alone, so the operator runs with
// pods:get and no pod informer.
func TestStalePodIsReadThroughTheUncachedReader(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret(), podAt("survival-old"))
	r.Client = noCachedPods{c}
	r.Pods = c
	reconcile(t, r, "survival")
	observeTemplate(t, c, "survival-new")
	reconcile(t, r, "survival")
	if podPresent(t, c) {
		t.Fatal("the pod on the old template was left in place")
	}
}
