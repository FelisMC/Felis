package fileedit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// pollInterval uses Pod lists because the API has pods:list but no pods:watch grant.
const pollInterval = 150 * time.Millisecond

const maxLogBytes = 12 << 20

const (
	defaultMaxAsyncPerServer = 1
	defaultMaxAsyncGlobal    = 4
	defaultMaxSyncPerServer  = 2
	defaultMaxSyncGlobal     = 4
	deleteJobTimeout         = 5 * time.Second
)

// K8sRunner gives file Jobs no Kubernetes credentials.
type K8sRunner struct {
	cs                kubernetes.Interface
	Browser           *Browser
	MaxAsyncPerServer int
	MaxAsyncGlobal    int
	MaxSyncPerServer  int
	MaxSyncGlobal     int
	asyncMu           sync.Mutex
	syncMu            sync.Mutex
	syncGlobal        int
	syncByServer      map[string]int
}

func NewK8sRunner(cs kubernetes.Interface) *K8sRunner {
	return &K8sRunner{cs: cs}
}

func (k *K8sRunner) acquireSync(server string) bool {
	maxPerServer := k.MaxSyncPerServer
	if maxPerServer <= 0 {
		maxPerServer = defaultMaxSyncPerServer
	}
	maxGlobal := k.MaxSyncGlobal
	if maxGlobal <= 0 {
		maxGlobal = defaultMaxSyncGlobal
	}
	k.syncMu.Lock()
	defer k.syncMu.Unlock()
	if k.syncGlobal >= maxGlobal || k.syncByServer[server] >= maxPerServer {
		return false
	}
	if k.syncByServer == nil {
		k.syncByServer = map[string]int{}
	}
	k.syncGlobal++
	k.syncByServer[server]++
	return true
}

func (k *K8sRunner) releaseSync(server string) {
	k.syncMu.Lock()
	defer k.syncMu.Unlock()
	if k.syncGlobal > 0 {
		k.syncGlobal--
	}
	if n := k.syncByServer[server]; n <= 1 {
		delete(k.syncByServer, server)
	} else {
		k.syncByServer[server] = n - 1
	}
}

func (k *K8sRunner) deleteJob(namespace, name string) {
	delCtx, cancel := context.WithTimeout(context.Background(), deleteJobTimeout)
	defer cancel()
	policy := metav1.DeletePropagationBackground
	_ = k.cs.BatchV1().Jobs(namespace).Delete(delCtx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
}

// Run may stream reads early but waits for mutation termination to preserve world-lock ordering; create collisions remain errors.
func (k *K8sRunner) Run(ctx context.Context, p JobParams) ([]byte, error) {
	if k.Browser != nil && !mutates(p.Op) && p.Op != OpDiskUsage {
		payload, err := k.Browser.Run(ctx, p, k.Start)
		// Use bounded one-shot reads when reusable browser workers are at capacity.
		if !errors.Is(err, errBrowserFull) {
			return payload, err
		}
	}
	if p.Op != OpDiskUsage {
		if !k.acquireSync(p.Server) {
			return nil, ErrTooManyJobs
		}
		defer k.releaseSync(p.Server)
	}
	job, err := k.createJob(ctx, p)
	if err != nil {
		return nil, err
	}
	finished := false
	defer func() {
		if !finished && !mutates(p.Op) && p.Op != OpDiskUsage && ctx.Err() != nil {
			k.deleteJob(p.Namespace, job.Name)
		}
	}()

	pod, err := k.awaitPod(ctx, p)
	if err != nil {
		return nil, err
	}

	stream, err := k.cs.CoreV1().Pods(p.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: containerName, Follow: true,
	}).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("fileedit: read file job log: %w", err)
	}
	defer stream.Close()
	sc := bufio.NewScanner(io.LimitReader(stream, maxLogBytes))
	sc.Buffer(make([]byte, 0, 64*1024), maxLogBytes)
	var last string
	for sc.Scan() {
		if payload, ok := strings.CutPrefix(sc.Text(), ResultPrefix); ok {
			finished = true
			return []byte(payload), nil
		}
		last = sc.Text()
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("fileedit: read file job result: %w", err)
	}
	return nil, fmt.Errorf("fileedit: file job %s produced no result (phase %s): %s",
		job.Name, pod.Status.Phase, tail(last))
}

// awaitPod returns failed pods too because their logs may carry a valid diagnostic result.
func (k *K8sRunner) awaitPod(ctx context.Context, p JobParams) (*corev1.Pod, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		pods, err := k.cs.CoreV1().Pods(p.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: LabelOpID + "=" + p.OpID,
		})
		if err != nil {
			return nil, fmt.Errorf("fileedit: find file job pod: %w", err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if !mutates(p.Op) {
				for _, c := range pod.Status.ContainerStatuses {
					if c.Name == containerName && (c.State.Running != nil || c.State.Terminated != nil) {
						return pod, nil
					}
				}
			}
			switch pod.Status.Phase {
			case corev1.PodSucceeded, corev1.PodFailed:
				return pod, nil
			}
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			// Leave timed-out Jobs to their deadline and TTL; the API cannot delete them.
			return nil, fmt.Errorf("fileedit: timed out waiting for the file job to finish: %w", ctx.Err())
		}
	}
}

// extractResult selects the last marked line because pod logs merge runtime diagnostics with the result payload.
func extractResult(log string) ([]byte, bool) {
	var payload string
	var found bool
	sc := bufio.NewScanner(strings.NewReader(log))
	sc.Buffer(make([]byte, 0, 64*1024), maxLogBytes)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), ResultPrefix); ok {
			payload, found = rest, true
		}
	}
	if !found {
		return nil, false
	}
	return []byte(payload), true
}

func tail(log string) string {
	const n = 512
	log = strings.TrimSpace(log)
	if len(log) <= n {
		return log
	}
	return "..." + log[len(log)-n:]
}

func (k *K8sRunner) Start(ctx context.Context, p JobParams) error {
	_, err := k.createJob(ctx, p)
	return err
}

func (k *K8sRunner) createJob(ctx context.Context, p JobParams) (*batchv1.Job, error) {
	job, err := FilesJob(p)
	if err != nil {
		return nil, err
	}
	if p.Op == OpDiskUsage {
		if err := k.checkDiskUsageJobs(ctx, p.Namespace); err != nil {
			return nil, err
		}
	}
	if p.Async {
		k.asyncMu.Lock()
		defer k.asyncMu.Unlock()
		if err := k.checkAsyncJobs(ctx, p.Namespace, p.Server); err != nil {
			return nil, err
		}
	}
	if _, err := k.cs.BatchV1().Jobs(p.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("fileedit: create file job: %w", err)
	}
	return job, nil
}

func jobUnfinished(job batchv1.Job) bool {
	terminal := false
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) {
			terminal = true
		}
	}
	return !terminal || job.Status.Active > 0 || job.Status.Terminating != nil && *job.Status.Terminating > 0
}

func (k *K8sRunner) checkAsyncJobs(ctx context.Context, namespace, server string) error {
	maxPerServer := k.MaxAsyncPerServer
	if maxPerServer <= 0 {
		maxPerServer = defaultMaxAsyncPerServer
	}
	maxGlobal := k.MaxAsyncGlobal
	if maxGlobal <= 0 {
		maxGlobal = defaultMaxAsyncGlobal
	}
	options := metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + managedByValue + "," + LabelAsync + "=true",
		Limit:         100,
	}
	var serverCount, globalCount int
	for {
		jobs, err := k.cs.BatchV1().Jobs(namespace).List(ctx, options)
		if err != nil {
			return fmt.Errorf("fileedit: check unfinished async Jobs: %w", err)
		}
		for _, job := range jobs.Items {
			if !jobUnfinished(job) {
				continue
			}
			globalCount++
			if job.Labels[LabelServer] == server {
				serverCount++
			}
			if serverCount >= maxPerServer || globalCount >= maxGlobal {
				return ErrTooManyJobs
			}
		}
		if jobs.Continue == "" {
			return nil
		}
		options.Continue = jobs.Continue
	}
}

func (k *K8sRunner) checkDiskUsageJobs(ctx context.Context, namespace string) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	options := metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + managedByValue + "," + LabelMode + "=" + OpDiskUsage, Limit: 100}
	for {
		jobs, err := k.cs.BatchV1().Jobs(namespace).List(ctx, options)
		if err != nil {
			return fmt.Errorf("fileedit: check unfinished disk usage Jobs: %w", err)
		}
		unfinished := false
		for _, job := range jobs.Items {
			if jobUnfinished(job) {
				unfinished = true
				break
			}
		}
		if unfinished {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
			options.Continue = ""
			continue
		}
		if jobs.Continue == "" {
			return ctx.Err()
		}
		options.Continue = jobs.Continue
	}
}

const maxOps = 10

const opLogLines = 20

// Ops returns unknown progress for unreadable logs rather than failing the entire listing.
func (k *K8sRunner) Ops(ctx context.Context, namespace, server string) ([]OpState, error) {
	sel := metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + managedByValue + "," +
		LabelServer + "=" + server + "," + LabelAsync + "=true"}
	jobs, err := k.cs.BatchV1().Jobs(namespace).List(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("fileedit: list file jobs: %w", err)
	}
	pods, err := k.cs.CoreV1().Pods(namespace).List(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("fileedit: list file job pods: %w", err)
	}
	podOf := map[string]*corev1.Pod{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		id := pod.Labels[LabelOpID]
		if cur := podOf[id]; cur == nil || pod.CreationTimestamp.After(cur.CreationTimestamp.Time) {
			podOf[id] = pod
		}
	}
	items := jobs.Items
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].CreationTimestamp.After(items[j].CreationTimestamp.Time)
	})
	if len(items) > maxOps {
		items = items[:maxOps]
	}
	out := make([]OpState, 0, len(items))
	for i := range items {
		log := ""
		pod := podOf[items[i].Labels[LabelOpID]]
		if pod != nil && pod.Status.Phase != corev1.PodPending {
			log, _ = k.logTail(ctx, namespace, pod.Name)
		}
		out = append(out, opState(&items[i], pod, log))
	}
	return out, nil
}

func (k *K8sRunner) logTail(ctx context.Context, namespace, pod string) (string, error) {
	lines := int64(opLogLines)
	stream, err := k.cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: containerName, TailLines: &lines,
	}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	b, err := io.ReadAll(io.LimitReader(stream, maxLogBytes))
	return string(b), err
}

// opState trusts a printed result over Job conditions; absent results use failure reasons or ResultUnavailable when logs cannot be read.
func opState(job *batchv1.Job, pod *corev1.Pod, log string) OpState {
	st := OpState{
		ID: job.Labels[LabelOpID], Op: job.Labels[LabelMode], Path: job.Annotations[AnnotationPath],
		State: OpRunning, Started: job.CreationTimestamp.Time,
	}
	if p, ok := lastProgress(log); ok {
		st.Done, st.Total = p.Done, p.Total
	}
	ended, reason := false, ""
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete, batchv1.JobSuccessCriteriaMet:
			ended, reason = true, "ResultUnavailable"
			st.Finished = c.LastTransitionTime.Time
		case batchv1.JobFailed, batchv1.JobFailureTarget:
			ended, reason = true, c.Reason
			st.Finished = c.LastTransitionTime.Time
		}
	}
	if !ended {
		return st
	}
	if payload, ok := extractResult(log); ok {
		var res Result
		if json.Unmarshal(payload, &res) == nil {
			st.Result = &res
		}
	}
	switch {
	case st.Result != nil && st.Result.Code == "":
		st.State = OpSucceeded
	case st.Result != nil:
		st.State = OpFailed
	default:
		st.State, st.Reason = OpFailed, reason
		if killedForMemory(pod) {
			st.Reason = ReasonOOMKilled
		} else if st.Reason == "" {
			st.Reason = "Failed"
		}
	}
	return st
}

const ReasonOOMKilled = "OOMKilled"

func killedForMemory(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; cs.Name == containerName && t != nil && t.Reason == ReasonOOMKilled {
			return true
		}
	}
	return false
}
