package api

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/maintenance"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Job label values must match internal/backupjob and internal/restore without reversing their dependency on api.
const (
	jobServerLabel    = "felis.lolicon.best/server"
	jobManagedByLabel = "app.kubernetes.io/managed-by"

	jobManagedByBackup  = "felis-backup"
	jobManagedByRestore = "felis-restore"
	jobManagedByExport  = "felis-export"

	jobBackupReasonLabel     = "felis.lolicon.best/backup-reason"
	jobBackupReasonScheduled = "scheduled"
)

type K8sJobStatus struct {
	c         client.Client
	namespace string
}

func NewK8sJobStatus(c client.Client, namespace string) *K8sJobStatus {
	return &K8sJobStatus{c: c, namespace: namespace}
}

func (k *K8sJobStatus) LatestJobs(ctx context.Context, serverName string) ([]AsyncJob, error) {
	var list batchv1.JobList
	if err := k.c.List(ctx, &list, client.InNamespace(k.namespace),
		client.MatchingLabels{jobServerLabel: serverName}); err != nil {
		return nil, err
	}
	out := make([]AsyncJob, 0, len(list.Items))
	for i := range list.Items {
		if aj, ok := jobToAsyncJob(&list.Items[i]); ok {
			out = append(out, aj)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if len(out) > 20 {
		out = out[:20]
	}
	k.explainFailures(ctx, serverName, out)
	return out, nil
}

// explainFailures keeps the Job condition text when pod termination details have expired or cannot be read.
func (k *K8sJobStatus) explainFailures(ctx context.Context, serverName string, jobs []AsyncJob) {
	failed := map[string]int{}
	for i, j := range jobs {
		if j.State == "failed" {
			failed[j.Name] = i
		}
	}
	if len(failed) == 0 {
		return
	}
	var pods corev1.PodList
	if err := k.c.List(ctx, &pods, client.InNamespace(k.namespace),
		client.MatchingLabels{jobServerLabel: serverName}); err != nil {
		return
	}
	newest := map[string]time.Time{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		idx, ok := failed[pod.Labels["job-name"]]
		if !ok {
			continue
		}
		msg := lastTerminationLine(pod)
		if msg == "" || !pod.CreationTimestamp.Time.After(newest[pod.Labels["job-name"]]) {
			continue
		}
		newest[pod.Labels["job-name"]] = pod.CreationTimestamp.Time
		jobs[idx].Message = msg
	}
}

const outOfMemoryLine = "the job ran out of memory and the system stopped it (OOMKilled)"

// lastTerminationLine prioritizes OOMKilled over stale output and caps the displayed line.
func lastTerminationLine(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		t := cs.State.Terminated
		if t == nil || t.ExitCode == 0 {
			continue
		}
		if t.Reason == fileedit.ReasonOOMKilled {
			return outOfMemoryLine
		}
		lines := strings.Split(strings.TrimSpace(t.Message), "\n")
		line := strings.TrimSpace(lines[len(lines)-1])
		if len(line) > 400 {
			line = line[:400] + "…"
		}
		return line
	}
	return ""
}

func jobToAsyncJob(j *batchv1.Job) (AsyncJob, bool) {
	aj, ok := jobOutcome(j)
	if !ok {
		return aj, false
	}
	if state := j.Labels[maintenance.LabelThenRestore]; state != "" && aj.Kind == "backup" {
		aj.ThenRestore = state
		aj.RestoreBackupID = j.Annotations[maintenance.AnnotationRestoreBackupID]
		if reason := j.Annotations[maintenance.AnnotationThenRestoreReason]; reason != "" {
			aj.ThenRestoreReason = reason
			if aj.Message == "" {
				aj.Message = chainAbandonMessage(reason)
			}
		}
	}
	aj.Scheduled = aj.Kind == "backup" && j.Labels[jobBackupReasonLabel] == jobBackupReasonScheduled
	return aj, true
}

// RunningWorldJobs excludes browser-paced exports so long downloads cannot delay every server's scheduled backup.
func (k *K8sJobStatus) RunningWorldJobs(ctx context.Context) (int, error) {
	var list batchv1.JobList
	if err := k.c.List(ctx, &list, client.InNamespace(k.namespace), client.HasLabels{jobManagedByLabel}); err != nil {
		return 0, err
	}
	n := 0
	for i := range list.Items {
		j := &list.Items[i]
		if _, ok := jobOutcome(j); ok && j.Labels[jobManagedByLabel] != jobManagedByExport && !maintenance.JobFinished(j) {
			n++
		}
	}
	return n, nil
}

func (k *K8sJobStatus) PendingRestoreChains(ctx context.Context) ([]RestoreChain, error) {
	var list batchv1.JobList
	if err := k.c.List(ctx, &list, client.InNamespace(k.namespace), client.MatchingLabels{
		jobManagedByLabel:            jobManagedByBackup,
		maintenance.LabelThenRestore: maintenance.ThenRestorePending,
	}); err != nil {
		return nil, err
	}
	out := make([]RestoreChain, 0, len(list.Items))
	for i := range list.Items {
		j := &list.Items[i]
		snapshot := ChainSnapshotRunning
		for _, c := range j.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				continue
			}
			switch c.Type {
			case batchv1.JobComplete, batchv1.JobSuccessCriteriaMet:
				snapshot = ChainSnapshotSucceeded
			case batchv1.JobFailed, batchv1.JobFailureTarget:
				snapshot = ChainSnapshotFailed
			}
		}
		if j.Labels["felis.lolicon.best/archive-pending"] == "true" && snapshot == ChainSnapshotSucceeded {
			snapshot = ChainSnapshotRunning
		}
		out = append(out, RestoreChain{
			Job:       j.Name,
			Server:    j.Labels[jobServerLabel],
			BackupID:  j.Annotations[maintenance.AnnotationRestoreBackupID],
			BackupRef: j.Annotations[maintenance.AnnotationRestoreRef],
			Snapshot:  snapshot,
		})
	}
	return out, nil
}

func (k *K8sJobStatus) SettleRestoreChain(ctx context.Context, job, state, reason string) error {
	meta := map[string]any{"labels": map[string]string{maintenance.LabelThenRestore: state}}
	if reason != "" {
		meta["annotations"] = map[string]string{maintenance.AnnotationThenRestoreReason: reason}
	}
	patch, err := json.Marshal(map[string]any{"metadata": meta})
	if err != nil {
		return err
	}
	obj := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: k.namespace, Name: job}}
	return k.c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
}

func jobOutcome(j *batchv1.Job) (AsyncJob, bool) {
	kind := ""
	switch j.Labels[jobManagedByLabel] {
	case jobManagedByBackup:
		kind = "backup"
	case jobManagedByRestore:
		kind = "restore"
	case jobManagedByExport:
		// Unknown export modes conservatively count as world exports that hold the volume.
		switch j.Labels[maintenance.LabelExportMode] {
		case maintenance.ExportModeBackup:
			kind = "export_backup"
		case maintenance.ExportModeFiles:
			kind = "export_files"
		default:
			kind = "export_world"
		}
	default:
		return AsyncJob{}, false
	}
	aj := AsyncJob{Name: j.Name, Kind: kind, State: "running", StartedAt: j.CreationTimestamp.Time}
	if j.Status.StartTime != nil {
		aj.StartedAt = j.Status.StartTime.Time
	}
	for _, c := range j.Status.Conditions {
		switch {
		case c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue:
			aj.State = "succeeded"
			aj.FinishedAt = c.LastTransitionTime.Time
		case c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue:
			aj.State = "failed"
			aj.FinishedAt = c.LastTransitionTime.Time
			aj.Message = c.Message
			if aj.Message == "" {
				aj.Message = c.Reason
			}
		}
	}
	return aj, true
}
