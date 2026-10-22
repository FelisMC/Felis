package operator_test

import (
	"context"
	"errors"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/operator"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestKillSkipsSavingAndShutdownGrace(t *testing.T) {
	for _, phase := range []v1alpha1.Phase{v1alpha1.PhaseRunning, v1alpha1.PhaseStarting, v1alpha1.PhaseStopping, v1alpha1.PhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			srv := runningServer()
			srv.Spec.DesiredState = v1alpha1.DesiredStopped
			srv.Annotations = map[string]string{v1alpha1.AnnotationKill: "request"}
			srv.Status.Phase, srv.Status.Ready = phase, true
			now := fixedNow()
			srv.Status.StopNoticeAt = &now
			one := int32(1)
			sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: srv.Name, Namespace: srv.Namespace},
				Spec: appsv1.StatefulSetSpec{Replicas: &one}, Status: appsv1.StatefulSetStatus{Replicas: 1, ReadyReplicas: 1}}
			var saves, broadcasts []string
			r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Known: true, Online: 3}, saves: &saves, broadcasts: &broadcasts}, srv, sts, gamePod(), rconSecret())
			deletes, failDelete := 0, true
			r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					options := (&client.DeleteOptions{}).ApplyOptions(opts)
					if obj.GetName() != "survival-0" || options.GracePeriodSeconds == nil || *options.GracePeriodSeconds != 0 {
						t.Fatalf("delete %s with grace %v, want survival-0 with zero grace", obj.GetName(), options.GracePeriodSeconds)
					}
					if replicas := getSTS(t, c, srv.Name).Spec.Replicas; replicas == nil || *replicas != 0 {
						t.Fatal("workload can still replace the killed pod")
					}
					deletes++
					if failDelete {
						return errors.New("pod deletion unavailable")
					}
					return c.Delete(ctx, obj, opts...)
				},
			})
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(srv)}); err == nil {
				t.Fatal("deletion failure was not returned")
			}
			if getServer(t, c, srv.Name).Annotations[v1alpha1.AnnotationKill] != "request" {
				t.Fatal("failed deletion lost the durable request")
			}
			failDelete = false
			reconcile(t, r, srv.Name)
			got := getServer(t, c, srv.Name)
			if _, pending := got.Annotations[v1alpha1.AnnotationKill]; pending {
				t.Fatal("completed kill retained its request")
			}
			if got.Status.StopNoticeAt != nil || got.Status.Phase != v1alpha1.PhaseStopping {
				t.Fatalf("status after kill = %+v", got.Status)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(gamePod()), &corev1.Pod{}); !apierrors.IsNotFound(err) {
				t.Fatalf("pod survived force stop: %v", err)
			}
			reconcile(t, r, srv.Name)
			if deletes != 2 || len(saves) != 0 || len(broadcasts) != 0 {
				t.Fatalf("delete=%d saves=%v broadcasts=%v", deletes, saves, broadcasts)
			}
			current := getSTS(t, c, srv.Name)
			current.Status = appsv1.StatefulSetStatus{}
			if err := c.Status().Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			reconcile(t, r, srv.Name)
			if getServer(t, c, srv.Name).Status.Phase != v1alpha1.PhaseStopped {
				t.Fatal("force stop did not finish")
			}
		})
	}
}

func TestKillWithoutWorkloadIsIdempotent(t *testing.T) {
	srv := runningServer()
	srv.Spec.DesiredState = v1alpha1.DesiredStopped
	srv.Annotations = map[string]string{v1alpha1.AnnotationKill: "request"}
	r, c := newReconciler(t, fakeProber{}, srv, gamePod())
	for range 2 {
		reconcile(t, r, srv.Name)
		got := getServer(t, c, srv.Name)
		if got.Status.Phase != v1alpha1.PhaseStopped || len(got.Annotations) != 0 {
			t.Fatalf("force stop = %+v", got)
		}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(gamePod()), &corev1.Pod{}); !apierrors.IsNotFound(err) {
			t.Fatalf("orphan pod survived: %v", err)
		}
		got.Annotations = map[string]string{v1alpha1.AnnotationKill: "again"}
		if err := c.Update(context.Background(), got); err != nil {
			t.Fatal(err)
		}
	}
}
