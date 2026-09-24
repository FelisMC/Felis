package api

import (
	"context"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Labels the backup/restore executors apply (internal/backupjob and
// internal/restore keep their own copies). Literals on purpose: api defines the
// read seam and must not import the executors — their dependency direction is
// "executors implement api's interfaces", never the reverse.
const (
	jobServerLabel    = "felis.lolicon.best/server"
	jobManagedByLabel = "app.kubernetes.io/managed-by"

	jobManagedByBackup  = "felis-backup"
	jobManagedByRestore = "felis-restore"
)

// K8sJobStatus reads the async Jobs the executors created, by the server label
// both apply.
type K8sJobStatus struct {
	c         client.Client
	namespace string
}

// NewK8sJobStatus builds the reader over the cluster client and the namespace the
// server workloads (and their Jobs) live in.
func NewK8sJobStatus(c client.Client, namespace string) *K8sJobStatus {
	return &K8sJobStatus{c: c, namespace: namespace}
}

// LatestJobs lists this server's backup/restore Jobs newest-first, capped so a
// long history cannot balloon the response. Jobs the label selector catches but
// another component created (unknown managed-by) are dropped.
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

// explainFailures replaces a failed Job's condition text ("Job has reached the
// specified backoff limit") with the error its container exited on. The
// executors set TerminationMessagePolicy FallbackToLogsOnError, so the
// terminated state carries the tail of the log, whose last line is the
// "felis backup: …" / "felis restore: …" error. The pods carry the Job's
// labels, so one list covers every Job of the server; a pod already collected
// by the Job TTL, or a list error, leaves the condition text in place.
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

// lastTerminationLine returns the last non-empty line of the pod's terminated
// container message, capped for display.
func lastTerminationLine(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		t := cs.State.Terminated
		if t == nil || t.ExitCode == 0 {
			continue
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

// jobToAsyncJob projects one Job onto its kind/state/message. Complete condition →
// succeeded, Failed → failed with its reason (Job conditions carry the generic
// "backoff limit exceeded" text; the pod log holds the underlying error), anything
// else is still running.
func jobToAsyncJob(j *batchv1.Job) (AsyncJob, bool) {
	kind := ""
	switch j.Labels[jobManagedByLabel] {
	case jobManagedByBackup:
		kind = "backup"
	case jobManagedByRestore:
		kind = "restore"
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
