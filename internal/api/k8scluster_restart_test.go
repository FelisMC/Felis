package api

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPendingStartRequestsPreserveFixedPodUID(t *testing.T) {
	for _, existing := range []string{v1alpha1.AnnotationRestart, v1alpha1.AnnotationStartRetry, "both"} {
		for _, request := range []string{v1alpha1.AnnotationRestart, v1alpha1.AnnotationStartRetry} {
			for _, value := range []string{"legacy-token", `{"request": "original-token", "pod_uid": "old-pod-uid"}`} {
				t.Run(existing+" to "+request+" "+value, func(t *testing.T) {
					ms := stoppedServer()
					ms.Spec.DesiredState, ms.Status.Phase = v1alpha1.DesiredRunning, v1alpha1.PhaseRunning
					ms.Annotations = map[string]string{"unrelated": "kept"}
					if existing == "both" {
						ms.Annotations[v1alpha1.AnnotationRestart], ms.Annotations[v1alpha1.AnnotationStartRetry] = value, value
					} else {
						ms.Annotations[existing] = value
					}
					k, c := lockCluster(t, ms)
					var before v1alpha1.MinecraftServer
					if err := c.Get(context.Background(), client.ObjectKeyFromObject(ms), &before); err != nil {
						t.Fatal(err)
					}
					for range 2 {
						var err error
						if request == v1alpha1.AnnotationRestart {
							err = k.RestartServer(context.Background(), ms.Name)
						} else {
							err = k.RetryStart(context.Background(), ms.Name)
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					var got v1alpha1.MinecraftServer
					if err := c.Get(context.Background(), client.ObjectKeyFromObject(ms), &got); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got.Annotations, before.Annotations) || got.ResourceVersion != before.ResourceVersion || got.Spec.DesiredState != v1alpha1.DesiredRunning {
						t.Fatal("a repeated or cross-kind request replaced the durable pending request")
					}
				})
			}
		}
	}
}

func TestPendingStartRequestsKeepAdmissionPriority(t *testing.T) {
	for _, mode := range []string{"kill before status and maintenance", "status before maintenance", "maintenance before pending", "node maintenance before pending"} {
		t.Run(mode, func(t *testing.T) {
			ms := stoppedServer()
			ms.Spec.DesiredState, ms.Status.Phase = v1alpha1.DesiredRunning, v1alpha1.PhaseRunning
			ms.Annotations = map[string]string{v1alpha1.AnnotationRestart: `{"request":"pending","pod_uid":"old-pod"}`}
			if mode != "node maintenance before pending" {
				ms.Annotations[maintenance.Annotation] = maintenance.LockValue(maintenance.KindBackup, lockNow)
			}
			if mode == "kill before status and maintenance" {
				ms.Annotations[v1alpha1.AnnotationKill] = "force-stop"
			}
			if mode == "kill before status and maintenance" || mode == "status before maintenance" {
				ms.Status.Phase = v1alpha1.PhaseStarting
			}
			k, c := lockCluster(t, ms)
			if mode == "node maintenance before pending" {
				k.NodeMaintenanceGuard = func(context.Context) error { return ErrMaintenanceInProgress }
			}
			err := k.RestartServer(context.Background(), ms.Name)
			if mode == "maintenance before pending" || mode == "node maintenance before pending" {
				if !errors.Is(err, ErrMaintenanceInProgress) {
					t.Fatalf("maintenance was bypassed: %v", err)
				}
			} else {
				var apiErr *apiError
				want := "not_running"
				if mode == "kill before status and maintenance" {
					want = "conflict"
				}
				if !errors.As(err, &apiErr) || apiErr.code != want {
					t.Fatalf("wrong admission priority: %v, want %s", err, want)
				}
			}
			var got v1alpha1.MinecraftServer
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(ms), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Annotations, ms.Annotations) {
				t.Fatal("a rejected request changed the pending identity")
			}
		})
	}
}

func TestStoppedServerRetryReplacesOldStartIntent(t *testing.T) {
	for _, existing := range []string{v1alpha1.AnnotationRestart, v1alpha1.AnnotationStartRetry, "both"} {
		t.Run(existing, func(t *testing.T) {
			ms := stoppedServer()
			old := `{"request":"old-token","pod_uid":"old-pod"}`
			ms.Annotations = map[string]string{"unrelated": "kept"}
			if existing == "both" {
				ms.Annotations[v1alpha1.AnnotationRestart], ms.Annotations[v1alpha1.AnnotationStartRetry] = old, old
			} else {
				ms.Annotations[existing] = old
			}
			k, c := lockCluster(t, ms)
			if err := k.RetryStart(context.Background(), ms.Name); err != nil {
				t.Fatal(err)
			}
			got, desired := annotations(t, c)
			if desired != v1alpha1.DesiredRunning || got[v1alpha1.AnnotationStartRetry] != lockNow.UTC().Format(time.RFC3339Nano) || got["unrelated"] != "kept" {
				t.Fatal("a stopped server's stale request prevented a fresh startup intent")
			}
		})
	}
}

func TestEmptyStartRequestsDoNotPreventNewRestart(t *testing.T) {
	ms := stoppedServer()
	ms.Spec.DesiredState, ms.Status.Phase = v1alpha1.DesiredRunning, v1alpha1.PhaseRunning
	ms.Annotations = map[string]string{v1alpha1.AnnotationRestart: "", v1alpha1.AnnotationStartRetry: ""}
	k, c := lockCluster(t, ms)
	if err := k.RestartServer(context.Background(), ms.Name); err != nil {
		t.Fatal(err)
	}
	got, _ := annotations(t, c)
	if got[v1alpha1.AnnotationRestart] != lockNow.UTC().Format(time.RFC3339Nano) {
		t.Fatal("an empty annotation was mistaken for a pending request")
	}
}
