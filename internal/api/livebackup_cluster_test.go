package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/maintenance"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func runningServer() *v1alpha1.MinecraftServer {
	ms := stoppedServer()
	ms.Spec.DesiredState = v1alpha1.DesiredRunning
	ms.Status.Phase, ms.Status.Ready = v1alpha1.PhaseRunning, true
	return ms
}

func gamePodOf(uid string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "survival-0", Namespace: "minecraft", UID: types.UID(uid),
			Labels: map[string]string{v1alpha1.LabelServer: "survival", v1alpha1.LabelComponent: gamePodComponent}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "server", RestartCount: restarts}}},
	}
}

// TestAcquireLiveBackupAdmitsOnlyARunningFreeServer pins the admission: a server that is
// up and ready, with its game pod running and nothing else on its world, gets the freeze
// recorded with the world lock; anything else is refused before the game is touched.
func TestAcquireLiveBackupAdmitsOnlyARunningFreeServer(t *testing.T) {
	ctx := context.Background()
	k, c := lockCluster(t, runningServer(), gamePodOf("pod-1", 2))
	f, err := k.AcquireLiveBackup(ctx, "survival")
	if err != nil {
		t.Fatal(err)
	}
	if f.PodUID != "pod-1" || f.Restarts != 2 || !f.Started.Equal(lockNow) {
		t.Errorf("freeze = %+v", f)
	}
	ann, _ := annotations(t, c)
	if ann[maintenance.Annotation] != maintenance.LockValue(maintenance.KindBackup, lockNow) || ann[v1alpha1.AnnotationLiveBackup] == "" {
		t.Fatalf("annotations = %v", ann)
	}
	if _, err := k.AcquireLiveBackup(ctx, "survival"); err == nil {
		t.Error("a second live backup was admitted beside the first")
	}

	for name, objs := range map[string][]client.Object{
		"stopped":     {stoppedServer()},
		"stopping":    {stoppedServer(), gamePodOf("pod-1", 0)},
		"no game pod": {runningServer()},
		"restart pending": {func() client.Object {
			ms := runningServer()
			ms.Annotations = map[string]string{v1alpha1.AnnotationRestart: "now"}
			return ms
		}(), gamePodOf("pod-1", 0)},
		"a restore runs": {runningServer(), gamePodOf("pod-1", 0), runningRestore(t)},
	} {
		k, c := lockCluster(t, objs...)
		if _, err := k.AcquireLiveBackup(ctx, "survival"); err == nil {
			t.Errorf("%s: admitted", name)
		}
		if ann, _ := annotations(t, c); ann[v1alpha1.AnnotationLiveBackup] != "" {
			t.Errorf("%s: a refused backup recorded a freeze", name)
		}
	}
	k, _ = lockCluster(t, stoppedServer())
	if _, err := k.AcquireLiveBackup(ctx, "survival"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stopped server: %v, want ErrNotRunning", err)
	}
}

// TestLiveBackupRecordLifecycle pins the record: saving it names the Job and hands the
// world lock to that Job, listing finds it, and clearing removes it and the lock only when
// the lock is the backup's own.
func TestLiveBackupRecordLifecycle(t *testing.T) {
	ctx := context.Background()
	k, c := lockCluster(t, runningServer(), gamePodOf("pod-1", 0))
	f, err := k.AcquireLiveBackup(ctx, "survival")
	if err != nil {
		t.Fatal(err)
	}
	f.Job, f.WasOff = "backup-survival-x", true
	if err := k.SaveLiveBackup(ctx, f); err != nil {
		t.Fatal(err)
	}
	ann, _ := annotations(t, c)
	if _, locked := ann[maintenance.Annotation]; locked {
		t.Error("the admission lock outlived the Job's creation")
	}
	var stored LiveFreeze
	if err := json.Unmarshal([]byte(ann[v1alpha1.AnnotationLiveBackup]), &stored); err != nil || stored.Job != f.Job || !stored.WasOff {
		t.Fatalf("stored freeze = %+v (%v)", stored, err)
	}
	list, err := k.ListLiveBackups(ctx)
	if err != nil || len(list) != 1 || list[0].Server != "survival" || list[0].Job != f.Job {
		t.Fatalf("list = %+v (%v)", list, err)
	}
	if err := k.ClearLiveBackup(ctx, "survival"); err != nil {
		t.Fatal(err)
	}
	if ann, _ := annotations(t, c); ann[v1alpha1.AnnotationLiveBackup] != "" {
		t.Error("the freeze survived clearing")
	}

	ms := runningServer()
	ms.Annotations = map[string]string{maintenance.Annotation: maintenance.LockValue(maintenance.KindRestore, lockNow), v1alpha1.AnnotationLiveBackup: `{"job":"j"}`}
	k, c = lockCluster(t, ms)
	if err := k.ClearLiveBackup(ctx, "survival"); err != nil {
		t.Fatal(err)
	}
	if ann, _ := annotations(t, c); ann[maintenance.Annotation] == "" {
		t.Error("clearing a freeze dropped another operation's lock")
	}
}

// TestLiveBackupProgressReadsTheCopyStep pins what the settle loop sees: the game pod's
// identity, and the backup Job's copy step having exited.
func TestLiveBackupProgressReadsTheCopyStep(t *testing.T) {
	ctx := context.Background()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "backup-survival-x", Namespace: "minecraft", Labels: map[string]string{backupjob.LabelServer: "survival", backupjob.LabelLive: "true"}}}
	copying := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "backup-survival-x-abcde", Namespace: "minecraft", Labels: map[string]string{"job-name": "backup-survival-x"}},
		Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: backupjob.StageContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	other := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "backup-survival-plain", Namespace: "minecraft", Labels: map[string]string{backupjob.LabelServer: "survival"}}}
	k, c := lockCluster(t, runningServer(), gamePodOf("pod-1", 1), job, copying, other)
	f := LiveFreeze{Server: "survival", Job: "backup-survival-x", PodUID: "pod-1", Restarts: 1}
	p, err := k.LiveBackupProgress(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if p.PodUID != "pod-1" || p.Restarts != 1 || !p.JobExists || p.StagingDone || p.JobFinished {
		t.Fatalf("while copying = %+v", p)
	}
	copying.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	if err := c.Status().Update(ctx, copying); err != nil {
		t.Fatal(err)
	}
	if p, err = k.LiveBackupProgress(ctx, f); err != nil || !p.StagingDone {
		t.Fatalf("after the copy = %+v (%v)", p, err)
	}

	if err := k.DeleteLiveBackupJob(ctx, "survival", ""); err != nil {
		t.Fatal(err)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace("minecraft")); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 || jobs.Items[0].Name != "backup-survival-plain" {
		t.Errorf("after dropping the live Jobs: %d left (%v)", len(jobs.Items), jobs.Items)
	}
	if p, err = k.LiveBackupProgress(ctx, f); err != nil || p.JobExists {
		t.Errorf("a dropped Job reads as %+v (%v)", p, err)
	}
}

// TestEnsureWorldVolumeMatchesTheOperator pins that a rebuilt host's world claim has the
// name, size and class the operator's StatefulSet would ask for (so it adopts the claim
// on the first start), that an existing claim is left alone, and that an unknown server
// is refused.
func TestEnsureWorldVolumeMatchesTheOperator(t *testing.T) {
	ctx := context.Background()
	ms := stoppedServer()
	ms.Spec.Storage.Size, ms.Spec.Storage.StorageClassName = "4Gi", "local-path"
	k, c := lockCluster(t, ms)
	if err := k.EnsureWorldVolume(ctx, "survival"); err != nil {
		t.Fatal(err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "world-survival-0"}, &pvc); err != nil {
		t.Fatal(err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "4Gi" || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "local-path" || pvc.Labels[v1alpha1.LabelServer] != "survival" {
		t.Errorf("claim = %+v", pvc)
	}
	pvc.Labels["kept"] = "yes"
	if err := c.Update(ctx, &pvc); err != nil {
		t.Fatal(err)
	}
	if err := k.EnsureWorldVolume(ctx, "survival"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "world-survival-0"}, &pvc); err != nil || pvc.Labels["kept"] != "yes" {
		t.Errorf("an existing claim was replaced: %+v (%v)", pvc.Labels, err)
	}
	if err := k.EnsureWorldVolume(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown server = %v", err)
	}

	plain := stoppedServer()
	k, c = lockCluster(t, plain)
	if err := k.EnsureWorldVolume(ctx, "survival"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "world-survival-0"}, &pvc); err != nil || pvc.Spec.Resources.Requests.Storage().String() != "8Gi" || pvc.Spec.StorageClassName != nil {
		t.Errorf("default claim = %+v (%v)", pvc.Spec, err)
	}
}
