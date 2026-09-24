package operator_test

import (
	"context"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func maintenanceJob(managedBy string, finished bool) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "restore-survival", Namespace: "minecraft",
		Labels: map[string]string{maintenance.LabelServer: "survival", maintenance.LabelManagedBy: managedBy}}}
	if finished {
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	}
	return j
}

// A desiredState flipped to Running behind felis-api's back (kubectl, a script)
// while a restore Job is unpacking into the world must not get a game pod.
func TestReconcileRunning_HeldDuringMaintenance(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret(), maintenanceJob("felis-restore", false))
	r.Jobs = c

	res := reconcile(t, r, "survival")
	if res.RequeueAfter <= 0 {
		t.Fatal("a held server must requeue to notice the Job finishing")
	}
	var sts appsv1.StatefulSet
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &sts)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("StatefulSet created while the restore runs (err=%v)", err)
	}
	s := getServer(t, c, "survival")
	cond := meta.FindStatusCondition(s.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil || cond.Reason != "MaintenanceInProgress" {
		t.Fatalf("Ready condition = %+v, want reason MaintenanceInProgress", cond)
	}
	if s.Status.StartRequestedAt != nil {
		t.Fatal("the hold must not start the startup-timeout clock")
	}

	// The Job finishes: the next pass starts the server.
	var j batchv1.Job
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "restore-survival"}, &j); err != nil {
		t.Fatalf("get job: %v", err)
	}
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(context.Background(), &j); err != nil {
		if err := c.Update(context.Background(), &j); err != nil {
			t.Fatalf("finish job: %v", err)
		}
	}
	reconcile(t, r, "survival")
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &sts); err != nil {
		t.Fatalf("StatefulSet not created after the restore finished: %v", err)
	}
}

func TestReconcileRunning_FreshLockHolds(t *testing.T) {
	ms := runningServer()
	ms.Annotations = map[string]string{maintenance.Annotation: maintenance.LockValue(maintenance.KindBackup, fixedNow().Add(-5*time.Second))}
	r, c := newReconciler(t, fakeProber{}, ms, rconSecret())
	r.Jobs = c
	reconcile(t, r, "survival")
	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &sts); !apierrors.IsNotFound(err) {
		t.Fatalf("StatefulSet created under a fresh maintenance lock (err=%v)", err)
	}
}

func TestReconcileRunning_NotHeldByReadsOrFinishedJobs(t *testing.T) {
	read := maintenanceJob("felis-files", false)
	read.Name = "files-survival-read"
	read.Labels[maintenance.LabelFilesMode] = "read"
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret(), read, maintenanceJob("felis-backup", true))
	r.Jobs = c
	reconcile(t, r, "survival")
	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &sts); err != nil {
		t.Fatalf("StatefulSet not created: %v", err)
	}
}

// A server that is already up is never held: its pod is the one on the volume,
// and holding would only stall its readiness bookkeeping.
func TestReconcileRunning_RunningServerNotHeld(t *testing.T) {
	r, c := newReconciler(t, fakeProber{}, runningServer(), rconSecret())
	reconcile(t, r, "survival") // creates the StatefulSet at replicas 1
	if err := c.Create(context.Background(), maintenanceJob("felis-restore", false)); err != nil {
		t.Fatalf("create job: %v", err)
	}
	r.Jobs = c
	reconcile(t, r, "survival")
	cond := meta.FindStatusCondition(getServer(t, c, "survival").Status.Conditions, v1alpha1.ConditionReady)
	if cond != nil && cond.Reason == "MaintenanceInProgress" {
		t.Fatal("a scaled-up server was held")
	}
}
