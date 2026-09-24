package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/operator"
	"felis.lolicon.best/internal/restore"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The world-volume lock against a fake API server. The fake client honours
// resourceVersion on an optimistic-lock patch, so the atomic half is exercised
// for real; what it cannot model is two felis-api replicas racing, which the
// resourceVersion check is precisely the defence against.

var lockNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func stoppedServer() *v1alpha1.MinecraftServer {
	return &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft"},
		Spec:       v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredStopped},
		Status:     v1alpha1.MinecraftServerStatus{Phase: v1alpha1.PhaseStopped},
	}
}

func lockCluster(t *testing.T, objs ...client.Object) (*K8sCluster, client.Client) {
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
	k := NewK8sCluster(c, "minecraft")
	k.now = func() time.Time { return lockNow }
	return k, c
}

func runningRestore(t *testing.T) *batchv1.Job {
	t.Helper()
	j, err := restore.RestoreJob(restore.JobParams{
		Server: "survival", WorldPVC: "world-survival-0", BackupPVC: "felis-backups",
		BackupRef: "/backups/survival/a.tar.gz", ArchiveStore: "tarLocal",
		Namespace: "minecraft", ServiceAccount: "felis-restore", Image: "felis:1",
		BackupRoot: "/backups", WorldsRoot: "/world", Deadline: time.Minute,
		CPULimit: "1", MemLimit: "1Gi", TTLAfterFinished: time.Minute,
	})
	if err != nil {
		t.Fatalf("RestoreJob: %v", err)
	}
	return j
}

func annotations(t *testing.T, c client.Client) (map[string]string, v1alpha1.DesiredState) {
	t.Helper()
	var ms v1alpha1.MinecraftServer
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &ms); err != nil {
		t.Fatalf("get: %v", err)
	}
	return ms.Annotations, ms.Spec.DesiredState
}

func TestAcquireMaintenance(t *testing.T) {
	ctx := context.Background()

	t.Run("stopped and free -> lock written", func(t *testing.T) {
		k, c := lockCluster(t, stoppedServer())
		if err := k.AcquireMaintenance(ctx, "survival", maintenance.KindRestore); err != nil {
			t.Fatalf("AcquireMaintenance: %v", err)
		}
		ann, _ := annotations(t, c)
		if got, want := ann[maintenance.Annotation], maintenance.LockValue(maintenance.KindRestore, lockNow); got != want {
			t.Fatalf("lock = %q, want %q", got, want)
		}
		// A second admission while the first holds (its Job not yet created) is refused.
		var busy *MaintenanceBusyError
		if err := k.AcquireMaintenance(ctx, "survival", maintenance.KindBackup); !errors.As(err, &busy) || busy.Kind != maintenance.KindRestore {
			t.Fatalf("second admission: %v, want busy(restore)", err)
		}
		if err := k.ReleaseMaintenance(ctx, "survival"); err != nil {
			t.Fatalf("ReleaseMaintenance: %v", err)
		}
		if ann, _ := annotations(t, c); ann[maintenance.Annotation] != "" {
			t.Fatalf("lock survived release: %q", ann[maintenance.Annotation])
		}
	})

	notStopped := []struct {
		name string
		mut  func(*v1alpha1.MinecraftServer)
	}{
		{"desired Running", func(ms *v1alpha1.MinecraftServer) { ms.Spec.DesiredState = v1alpha1.DesiredRunning }},
		{"still Stopping", func(ms *v1alpha1.MinecraftServer) { ms.Status.Phase = v1alpha1.PhaseStopping }},
		{"Ready", func(ms *v1alpha1.MinecraftServer) { ms.Status.Ready = true }},
		{"never reconciled", func(ms *v1alpha1.MinecraftServer) { ms.Status.Phase = "" }},
	}
	for _, tc := range notStopped {
		t.Run(tc.name+" -> ErrNotStopped", func(t *testing.T) {
			ms := stoppedServer()
			tc.mut(ms)
			k, c := lockCluster(t, ms)
			if err := k.AcquireMaintenance(ctx, "survival", maintenance.KindRestore); !errors.Is(err, ErrNotStopped) {
				t.Fatalf("err = %v, want ErrNotStopped", err)
			}
			if ann, _ := annotations(t, c); ann[maintenance.Annotation] != "" {
				t.Fatal("a refused admission wrote the lock")
			}
		})
	}

	t.Run("game pod still terminating -> ErrNotStopped", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "survival-0", Namespace: "minecraft",
			Labels: map[string]string{v1alpha1.LabelServer: "survival", v1alpha1.LabelComponent: gamePodComponent}}}
		k, _ := lockCluster(t, stoppedServer(), pod)
		if err := k.AcquireMaintenance(ctx, "survival", maintenance.KindBackup); !errors.Is(err, ErrNotStopped) {
			t.Fatalf("err = %v, want ErrNotStopped", err)
		}
	})

	t.Run("a Job's own pod is not the game pod", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "files-x", Namespace: "minecraft",
			Labels: map[string]string{v1alpha1.LabelServer: "survival"}}}
		k, _ := lockCluster(t, stoppedServer(), pod)
		if err := k.AcquireMaintenance(ctx, "survival", maintenance.KindBackup); err != nil {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("running restore Job -> busy", func(t *testing.T) {
		k, _ := lockCluster(t, stoppedServer(), runningRestore(t))
		var busy *MaintenanceBusyError
		if err := k.AcquireMaintenance(ctx, "survival", maintenance.KindFileWrite); !errors.As(err, &busy) || busy.Kind != maintenance.KindRestore {
			t.Fatalf("err = %v, want busy(restore)", err)
		}
	})

	t.Run("unknown server -> ErrNotFound", func(t *testing.T) {
		k, _ := lockCluster(t)
		if err := k.AcquireMaintenance(ctx, "survival", maintenance.KindRestore); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if err := k.ReleaseMaintenance(ctx, "survival"); err != nil {
			t.Fatalf("release on a deleted server: %v", err)
		}
	})
}

func TestStartRespectsMaintenance(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh lock -> busy, desiredState untouched", func(t *testing.T) {
		ms := stoppedServer()
		ms.Annotations = map[string]string{maintenance.Annotation: maintenance.LockValue(maintenance.KindBackup, lockNow.Add(-10*time.Second))}
		k, c := lockCluster(t, ms)
		var busy *MaintenanceBusyError
		if err := k.SetDesiredState(ctx, "survival", v1alpha1.DesiredRunning); !errors.As(err, &busy) || busy.Kind != maintenance.KindBackup {
			t.Fatalf("err = %v, want busy(backup)", err)
		}
		if !errors.Is(&MaintenanceBusyError{}, ErrMaintenanceInProgress) {
			t.Fatal("MaintenanceBusyError must match ErrMaintenanceInProgress")
		}
		if _, desired := annotations(t, c); desired != v1alpha1.DesiredStopped {
			t.Fatalf("desiredState = %q, want Stopped", desired)
		}
	})

	t.Run("running restore Job -> busy", func(t *testing.T) {
		k, _ := lockCluster(t, stoppedServer(), runningRestore(t))
		if err := k.SetDesiredState(ctx, "survival", v1alpha1.DesiredRunning); !errors.Is(err, ErrMaintenanceInProgress) {
			t.Fatalf("err = %v, want maintenance in progress", err)
		}
	})

	t.Run("finished restore Job -> starts", func(t *testing.T) {
		j := runningRestore(t)
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		k, c := lockCluster(t, stoppedServer(), j)
		if err := k.SetDesiredState(ctx, "survival", v1alpha1.DesiredRunning); err != nil {
			t.Fatalf("err = %v", err)
		}
		if _, desired := annotations(t, c); desired != v1alpha1.DesiredRunning {
			t.Fatalf("desiredState = %q, want Running", desired)
		}
	})

	t.Run("stale lock -> starts and the lock is dropped", func(t *testing.T) {
		ms := stoppedServer()
		ms.Annotations = map[string]string{maintenance.Annotation: maintenance.LockValue(maintenance.KindRestore, lockNow.Add(-maintenance.Grace-time.Second))}
		k, c := lockCluster(t, ms)
		if err := k.SetDesiredState(ctx, "survival", v1alpha1.DesiredRunning); err != nil {
			t.Fatalf("err = %v", err)
		}
		ann, desired := annotations(t, c)
		if desired != v1alpha1.DesiredRunning {
			t.Fatalf("desiredState = %q, want Running", desired)
		}
		if _, ok := ann[maintenance.Annotation]; ok {
			t.Fatal("the stale lock was left behind")
		}
	})

	t.Run("stop ignores the lock", func(t *testing.T) {
		ms := stoppedServer()
		ms.Spec.DesiredState = v1alpha1.DesiredRunning
		ms.Annotations = map[string]string{maintenance.Annotation: maintenance.LockValue(maintenance.KindRestore, lockNow)}
		k, c := lockCluster(t, ms)
		if err := k.SetDesiredState(ctx, "survival", v1alpha1.DesiredStopped); err != nil {
			t.Fatalf("err = %v", err)
		}
		if _, desired := annotations(t, c); desired != v1alpha1.DesiredStopped {
			t.Fatalf("desiredState = %q, want Stopped", desired)
		}
	})
}

// gamePodComponent is a copy of the operator's pod label value; a drift would
// let a restore start beside a terminating server.
func TestGamePodComponentMatchesOperator(t *testing.T) {
	if gamePodComponent != operator.ComponentValue {
		t.Fatalf("gamePodComponent = %q, operator labels its pods %q", gamePodComponent, operator.ComponentValue)
	}
}
