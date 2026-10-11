package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/maintenance"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LiveFreeze is a running server whose saving felis-api has turned off for a live
// restore point (livebackup.go). It lives in the server's AnnotationLiveBackup, so a
// felis-api restart picks the freeze up again and still turns saving back on.
type LiveFreeze struct {
	Server string `json:"-"`
	// Job is the backup Job copying the world; empty until it is created.
	Job     string    `json:"job,omitempty"`
	Started time.Time `json:"started"`
	// PodUID and Restarts name the game process that was frozen: a different one has
	// saved on its way down, so a copy still running may be torn.
	PodUID   string `json:"podUID"`
	Restarts int32  `json:"restarts"`
	// WasOff records that saving was already off before the freeze (an owner turned it
	// off), so thawing leaves it off.
	WasOff bool `json:"wasOff,omitempty"`
}

// LiveProgress is how far a freeze's backup has come.
type LiveProgress struct {
	// PodUID and Restarts describe the game pod now; PodUID is empty when it is gone.
	PodUID   string
	Restarts int32
	// JobExists, StagingDone and JobFinished follow the backup Job: its copy step has
	// exited (either way), or the whole Job has.
	JobExists, StagingDone, JobFinished bool
}

// LiveBackupCluster is the cluster side of live restore points.
type LiveBackupCluster interface {
	// AcquireLiveBackup admits a live backup of a running, ready server that nothing
	// else holds, and records the freeze with the world lock, under the
	// resourceVersion it checked.
	AcquireLiveBackup(ctx context.Context, name string) (LiveFreeze, error)
	// SaveLiveBackup records the freeze's Job and drops the admission lock: the Job
	// holds the world from here on.
	SaveLiveBackup(ctx context.Context, f LiveFreeze) error
	// ClearLiveBackup forgets the freeze, and the admission lock if it is still there.
	ClearLiveBackup(ctx context.Context, name string) error
	ListLiveBackups(ctx context.Context) ([]LiveFreeze, error)
	LiveBackupProgress(ctx context.Context, f LiveFreeze) (LiveProgress, error)
	// DeleteLiveBackupJob drops a live backup Job and its pod; an empty job drops every
	// live backup Job of the server.
	DeleteLiveBackupJob(ctx context.Context, name, job string) error
}

// ErrNotRunning refuses a live backup of a server that is not up and ready.
var ErrNotRunning = errors.New("server is not running")

func (k *K8sCluster) AcquireLiveBackup(ctx context.Context, name string) (LiveFreeze, error) {
	if k.NodeMaintenanceGuard != nil {
		if err := k.NodeMaintenanceGuard(ctx); err != nil {
			return LiveFreeze{}, err
		}
	}
	var f LiveFreeze
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.getServer(ctx, name, &ms); err != nil {
			return err
		}
		if ms.Spec.DesiredState != v1alpha1.DesiredRunning || !ms.Status.Ready || ms.Status.Phase != v1alpha1.PhaseRunning {
			return ErrNotRunning
		}
		if ms.Annotations[v1alpha1.AnnotationRestart] != "" || ms.Annotations[v1alpha1.AnnotationLiveBackup] != "" {
			return ErrMaintenanceInProgress
		}
		holder, held, err := k.maintenanceHolder(ctx, &ms)
		if err != nil {
			return err
		}
		if held {
			return &MaintenanceBusyError{Kind: holder}
		}
		pod, err := k.gamePod(ctx, name)
		if err != nil {
			return err
		}
		if pod == nil || pod.Status.Phase != corev1.PodRunning {
			return ErrNotRunning
		}
		f = LiveFreeze{Server: name, Started: k.clock().UTC(), PodUID: string(pod.UID), Restarts: podRestarts(pod)}
		raw, _ := json.Marshal(f)
		patch := client.MergeFromWithOptions(ms.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if ms.Annotations == nil {
			ms.Annotations = map[string]string{}
		}
		ms.Annotations[maintenance.Annotation] = maintenance.LockValue(maintenance.KindBackup, k.clock())
		ms.Annotations[v1alpha1.AnnotationLiveBackup] = string(raw)
		return k.c.Patch(ctx, &ms, patch)
	})
	return f, err
}

func (k *K8sCluster) SaveLiveBackup(ctx context.Context, f LiveFreeze) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.getServer(ctx, f.Server, &ms); err != nil {
			return err
		}
		raw, _ := json.Marshal(f)
		patch := client.MergeFrom(ms.DeepCopy())
		if ms.Annotations == nil {
			ms.Annotations = map[string]string{}
		}
		ms.Annotations[v1alpha1.AnnotationLiveBackup] = string(raw)
		delete(ms.Annotations, maintenance.Annotation)
		return k.c.Patch(ctx, &ms, patch)
	})
}

func (k *K8sCluster) ClearLiveBackup(ctx context.Context, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.getServer(ctx, name, &ms); err != nil {
			return err
		}
		lock := ms.Annotations[maintenance.Annotation]
		ours := lock != "" && maintenance.LockKind(lock) == maintenance.KindBackup
		if ms.Annotations[v1alpha1.AnnotationLiveBackup] == "" && !ours {
			return nil
		}
		patch := client.MergeFrom(ms.DeepCopy())
		delete(ms.Annotations, v1alpha1.AnnotationLiveBackup)
		if ours {
			delete(ms.Annotations, maintenance.Annotation)
		}
		return k.c.Patch(ctx, &ms, patch)
	})
}

func (k *K8sCluster) ListLiveBackups(ctx context.Context) ([]LiveFreeze, error) {
	var list v1alpha1.MinecraftServerList
	if err := k.c.List(ctx, &list, client.InNamespace(k.namespace)); err != nil {
		return nil, err
	}
	var out []LiveFreeze
	for _, ms := range list.Items {
		raw := ms.Annotations[v1alpha1.AnnotationLiveBackup]
		if raw == "" {
			continue
		}
		var f LiveFreeze
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			// Unreadable: settle it as a freeze of no known Job, which thaws it.
			f = LiveFreeze{}
		}
		f.Server = ms.Name
		out = append(out, f)
	}
	return out, nil
}

func (k *K8sCluster) LiveBackupProgress(ctx context.Context, f LiveFreeze) (LiveProgress, error) {
	var p LiveProgress
	pod, err := k.gamePod(ctx, f.Server)
	if err != nil {
		return p, err
	}
	if pod != nil && pod.DeletionTimestamp == nil {
		p.PodUID, p.Restarts = string(pod.UID), podRestarts(pod)
	}
	if f.Job == "" {
		return p, nil
	}
	var job batchv1.Job
	switch err := k.c.Get(ctx, client.ObjectKey{Namespace: k.namespace, Name: f.Job}, &job); {
	case apierrors.IsNotFound(err):
		return p, nil
	case err != nil:
		return p, err
	}
	p.JobExists = true
	p.JobFinished = maintenance.JobFinished(&job)
	var pods corev1.PodList
	if err := k.c.List(ctx, &pods, client.InNamespace(k.namespace), client.MatchingLabels{"job-name": f.Job}); err != nil {
		return p, err
	}
	for _, jp := range pods.Items {
		for _, s := range jp.Status.InitContainerStatuses {
			if s.Name == backupjob.StageContainer && s.State.Terminated != nil {
				p.StagingDone = true
			}
		}
	}
	return p, nil
}

func (k *K8sCluster) DeleteLiveBackupJob(ctx context.Context, name, job string) error {
	var jobs batchv1.JobList
	if err := k.c.List(ctx, &jobs, client.InNamespace(k.namespace), client.MatchingLabels{
		backupjob.LabelServer: name, backupjob.LabelLive: "true",
	}); err != nil {
		return err
	}
	background := client.PropagationPolicy("Background")
	for i := range jobs.Items {
		if job != "" && jobs.Items[i].Name != job {
			continue
		}
		if err := k.c.Delete(ctx, &jobs.Items[i], background); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// gamePod is the server's game pod, nil when it has none.
func (k *K8sCluster) gamePod(ctx context.Context, name string) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := k.c.List(ctx, &pods, client.InNamespace(k.namespace), client.MatchingLabels{
		v1alpha1.LabelServer: name, v1alpha1.LabelComponent: gamePodComponent,
	}); err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	return &pods.Items[0], nil
}

func podRestarts(p *corev1.Pod) int32 {
	var n int32
	for _, s := range p.Status.ContainerStatuses {
		n += s.RestartCount
	}
	return n
}
