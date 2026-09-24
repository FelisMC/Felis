package operator

import (
	"context"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestReconcileWatchCheck: an idle or busy operator is healthy; one pass running
// past the limit fails the check and names the server; the check recovers once
// that pass returns.
func TestReconcileWatchCheck(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	w := &ReconcileWatch{StuckAfter: 10 * time.Minute, Now: func() time.Time { return now }}
	if err := w.Check(nil); err != nil {
		t.Fatalf("idle: %v", err)
	}

	endOld := w.begin("survival")
	now = now.Add(9 * time.Minute)
	endNew := w.begin("creative")
	if err := w.Check(nil); err != nil {
		t.Fatalf("9m in: %v", err)
	}

	now = now.Add(2 * time.Minute)
	err := w.Check(nil)
	if err == nil || !strings.Contains(err.Error(), "survival") || !strings.Contains(err.Error(), "11m0s") {
		t.Fatalf("11m in: err = %v, want survival stuck for 11m0s", err)
	}

	endOld()
	if err := w.Check(nil); err != nil {
		t.Fatalf("after the stuck pass returned: %v", err)
	}
	endNew()
}

// TestReconcileBoundedAndWatched: Reconcile hands the pass a deadline and holds
// an in-flight entry exactly while it runs.
func TestReconcileBoundedAndWatched(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	watch := &ReconcileWatch{}
	var deadline time.Duration
	var inflight int
	cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if d, ok := ctx.Deadline(); ok && deadline == 0 {
				deadline = time.Until(d)
			}
			watch.mu.Lock()
			inflight = len(watch.inflight)
			watch.mu.Unlock()
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r := &Reconciler{Client: cl, Scheme: scheme, Watch: watch}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "minecraft", Name: "missing"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if deadline <= 0 || deadline > reconcileTimeout {
		t.Errorf("pass deadline = %s, want within %s", deadline, reconcileTimeout)
	}
	if inflight != 1 {
		t.Errorf("in-flight passes during the pass = %d, want 1", inflight)
	}
	if n := len(watch.inflight); n != 0 {
		t.Errorf("in-flight passes after the pass = %d, want 0", n)
	}
}
