package operator_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/operator"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func pendingStartupRestart(t *testing.T) (*operator.Reconciler, client.Client) {
	t.Helper()
	server := runningServer()
	server.Spec.JavaFlags = []string{"-Dmode=old"}
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Known: true}}, server, rconSecret(), gamePod())
	reconcile(t, r, server.Name)
	markPodReady(t, c, server.Name)
	reconcile(t, r, server.Name)
	server = getServer(t, c, server.Name)
	server.Spec.JavaFlags = []string{"-Dmode=new"}
	server.Annotations = map[string]string{v1alpha1.AnnotationRestart: "2026-10-24T00:00:00Z"}
	if err := c.Update(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	return r, c
}

func restartPass(r *operator.Reconciler) error {
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "minecraft", Name: "survival"}})
	return err
}

func replacementPod(t *testing.T, c client.Client) *corev1.Pod {
	t.Helper()
	pod := gamePod()
	pod.UID = "replacement-pod"
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

func TestRestartSynchronizesTemplateBeforeDeletingPinnedPod(t *testing.T) {
	r, c := pendingStartupRestart(t)
	deleted := false
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				deleted = true
				var workload appsv1.StatefulSet
				if err := cl.Get(ctx, client.ObjectKey{Namespace: "minecraft", Name: "survival"}, &workload); err != nil {
					return err
				}
				if startupEnvValue(workload.Spec.Template.Spec.Containers[0].Env, "JAVA_FLAGS") != "-Dmode=new" {
					t.Fatal("the original Pod was deleted before the saved startup settings reached the StatefulSet")
				}
				options := &client.DeleteOptions{}
				for _, option := range opts {
					option.ApplyToDelete(options)
				}
				if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "original-pod" {
					t.Fatal("restart deletion was not fixed to the captured Pod UID")
				}
			}
			return cl.Delete(ctx, obj, opts...)
		},
	})
	if err := restartPass(r); err != nil {
		t.Fatal(err)
	}
	server := getServer(t, c, "survival")
	if !deleted || podPresent(t, c) || server.Status.Phase != v1alpha1.PhaseStarting || server.Annotations[v1alpha1.AnnotationRestart] != "" {
		t.Fatal("restart did not consume its original Pod and leave the new start pending")
	}
}

func TestRestartWaitsForStatefulSetToObserveItsTemplate(t *testing.T) {
	r, c := pendingStartupRestart(t)
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if workload, ok := obj.(*appsv1.StatefulSet); ok {
				var previous appsv1.StatefulSet
				if err := cl.Get(ctx, client.ObjectKeyFromObject(workload), &previous); err != nil {
					return err
				}
				if !reflect.DeepEqual(previous.Spec.Template, workload.Spec.Template) {
					workload.Generation = previous.Generation + 1
				}
			}
			return cl.Update(ctx, obj, opts...)
		},
	})
	if err := restartPass(r); err != nil {
		t.Fatal(err)
	}
	if !podPresent(t, c) || getServer(t, c, "survival").Annotations[v1alpha1.AnnotationRestart] == "" {
		t.Fatal("the Pod was deleted before the StatefulSet observed the new template")
	}
	workload := getSTS(t, c, "survival")
	workload.Status.ObservedGeneration = workload.Generation
	if err := c.Status().Update(context.Background(), workload); err != nil {
		t.Fatal(err)
	}
	if err := restartPass(r); err != nil {
		t.Fatal(err)
	}
	if podPresent(t, c) || getServer(t, c, "survival").Annotations[v1alpha1.AnnotationRestart] != "" {
		t.Fatal("the observed template did not complete its restart")
	}
}

func TestRestartRunsStopCommandsBeforeUpdatingTheTemplate(t *testing.T) {
	for _, saveFails := range []bool{false, true} {
		r, c := pendingStartupRestart(t)
		server := getServer(t, c, "survival")
		server.Spec.Lifecycle.StopCommands = []string{"say restarting"}
		if err := c.Update(context.Background(), server); err != nil {
			t.Fatal(err)
		}
		var steps []string
		prober := fakeProber{steps: &steps}
		if saveFails {
			prober.saveErr = errors.New("save unavailable")
		}
		r.Prober = prober
		r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*appsv1.StatefulSet); ok {
					steps = append(steps, "template")
				}
				return cl.Update(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					steps = append(steps, "delete")
				}
				return cl.Delete(ctx, obj, opts...)
			},
		})
		if err := restartPass(r); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(steps, []string{"save", "commands", "template", "delete"}) {
			t.Fatalf("restart steps = %v", steps)
		}
	}
}

func TestRestartRetriesStopPreparationAfterPinnedReadFailure(t *testing.T) {
	for _, failedRead := range []string{"secret", "workload", "pod"} {
		t.Run(failedRead, func(t *testing.T) {
			r, c := pendingStartupRestart(t)
			server := getServer(t, c, "survival")
			server.Spec.Lifecycle.StopCommands = []string{"say restarting"}
			if err := c.Update(context.Background(), server); err != nil {
				t.Fatal(err)
			}
			var steps []string
			r.Prober = fakeProber{steps: &steps}
			failed := false
			r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					matches := false
					switch obj.(type) {
					case *corev1.Secret:
						matches = failedRead == "secret"
					case *appsv1.StatefulSet:
						matches = failedRead == "workload"
					case *corev1.Pod:
						matches = failedRead == "pod"
					}
					if !failed && matches && strings.HasPrefix(getServer(t, c, "survival").Annotations[v1alpha1.AnnotationRestart], "{") {
						failed = true
						return errors.New("temporary read failure")
					}
					return cl.Get(ctx, key, obj, opts...)
				},
				Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*appsv1.StatefulSet); ok {
						steps = append(steps, "template")
					}
					return cl.Update(ctx, obj, opts...)
				},
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, ok := obj.(*corev1.Pod); ok {
						steps = append(steps, "delete")
					}
					return cl.Delete(ctx, obj, opts...)
				},
			})
			if err := restartPass(r); err == nil || !failed || len(steps) != 0 || !podPresent(t, c) {
				t.Fatal("the injected read failure did not leave the original Pod and pinned request pending")
			}
			if err := restartPass(r); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(steps, []string{"save", "commands", "template", "delete"}) {
				t.Fatalf("preparation retry steps = %v", steps)
			}
		})
	}
}

func TestRestartKeepsReplacementAfterStatusOrCleanupFailure(t *testing.T) {
	for _, failure := range []string{"status", "cleanup", "missing initial Pod"} {
		t.Run(failure, func(t *testing.T) {
			r, c := pendingStartupRestart(t)
			if failure == "missing initial Pod" {
				if err := c.Delete(context.Background(), gamePod()); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "cleanup" {
				r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if _, ok := obj.(*v1alpha1.MinecraftServer); ok && obj.GetAnnotations()[v1alpha1.AnnotationRestart] == "" {
							return errors.New("cleanup unavailable")
						}
						return cl.Patch(ctx, obj, patch, opts...)
					},
				})
			} else {
				r.Client = failStatus{c}
			}
			if err := restartPass(r); err == nil {
				t.Fatal("the injected persistence failure did not interrupt the restart")
			}
			if podPresent(t, c) {
				t.Fatal("the first pass did not remove its original Pod")
			}
			server := getServer(t, c, "survival")
			anchor := server.Status.StartRequestedAt.DeepCopy()
			if server.Annotations[v1alpha1.AnnotationRestart] == "" {
				t.Fatal("failed restart lost its pinned target")
			}
			replacementPod(t, c)
			r.Client = c
			r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(time.Hour)) }
			if err := restartPass(r); err != nil {
				t.Fatal(err)
			}
			server = getServer(t, c, "survival")
			if !podPresent(t, c) || server.Annotations[v1alpha1.AnnotationRestart] != "" {
				t.Fatal("retry deleted the replacement or did not settle the pinned request")
			}
			if failure == "cleanup" && !server.Status.StartRequestedAt.Equal(anchor) {
				t.Fatal("cleanup retry reset the already persisted startup budget")
			}
		})
	}
}

func TestRestartHandlesReplacementBetweenReadAndDelete(t *testing.T) {
	r, c := pendingStartupRestart(t)
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.Pod); !ok {
				return cl.Delete(ctx, obj, opts...)
			}
			if err := cl.Delete(ctx, gamePod()); err != nil {
				return err
			}
			replacement := replacementPod(t, cl)
			options := &client.DeleteOptions{}
			for _, option := range opts {
				option.ApplyToDelete(options)
			}
			if options.Preconditions != nil && options.Preconditions.UID != nil && *options.Preconditions.UID != replacement.UID {
				return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, replacement.Name, errors.New("UID precondition failed"))
			}
			return cl.Delete(ctx, replacement)
		},
	})
	if err := restartPass(r); err != nil {
		t.Fatal(err)
	}
	if !podPresent(t, c) || getServer(t, c, "survival").Annotations[v1alpha1.AnnotationRestart] != "" {
		t.Fatal("a controller replacement was deleted or left the request pending")
	}
}

func TestRestartDoesNotConsumeConflictWhileOriginalPodStillExists(t *testing.T) {
	r, c := pendingStartupRestart(t)
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "survival-0", errors.New("try again"))
		},
	})
	if err := restartPass(r); !apierrors.IsConflict(err) {
		t.Fatal("an unfulfilled delete conflict was treated as completion")
	}
	if !podPresent(t, c) || getServer(t, c, "survival").Annotations[v1alpha1.AnnotationRestart] == "" {
		t.Fatal("an unfulfilled restart lost its original Pod or pinned request")
	}
	r.Client = c
	if err := restartPass(r); err != nil {
		t.Fatal(err)
	}
	if podPresent(t, c) {
		t.Fatal("the pinned original Pod was not retried")
	}
}

func TestMalformedPinnedRestartNeverRecapturesPodAndDoesNotBlockStop(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		server := runningServer()
		server.Annotations = map[string]string{v1alpha1.AnnotationRestart: "{broken"}
		if stopped {
			server.Spec.DesiredState = v1alpha1.DesiredStopped
		}
		r, c := newReconciler(t, fakeProber{}, server, gamePod())
		err := restartPass(r)
		if stopped {
			if err != nil || getServer(t, c, server.Name).Annotations[v1alpha1.AnnotationRestart] != "" {
				t.Fatal("a malformed pending request blocked the newer stop intent")
			}
		} else if err == nil || !podPresent(t, c) {
			t.Fatal("malformed progress was treated as a new restart target")
		}
	}
}
