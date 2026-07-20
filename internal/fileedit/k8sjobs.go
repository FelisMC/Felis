package fileedit

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// pollInterval is how often the runner re-Lists Pods while waiting for the file
// Job to finish. It is a POLL rather than a Watch because felis-api holds
// pods:list and NOT pods:watch (internal/platform.APIMinecraftRole) — establishing
// a watch would need a permission this design exists to avoid. Half a second is
// well inside the human-perceptible floor for an operation already dominated by
// Pod scheduling, while keeping the request count on a slow image pull modest.
const pollInterval = 500 * time.Millisecond

// maxLogBytes bounds what the runner will buffer from a Pod's log. The payload is
// at most a base64-encoded MaxReadBytes (≈4/3 of 1 MiB) plus the JSON envelope, so
// 4 MiB is generous headroom while still refusing to let a Pod that floods stderr
// pull felis-api's memory down with it.
const maxLogBytes = 4 << 20

// K8sRunner is the production Runner (spec §7, §16). It drives one file operation
// end to end using ONLY the three permissions felis-api already holds in the
// minecraft namespace, which is the entire point of the design:
//
//	jobs:create   → create the file Job
//	pods:list     → find its Pod and observe the phase (no pods:get, no pods:watch)
//	pods/log:get  → read the printed result back
//
// It deliberately takes a typed kubernetes.Interface rather than the
// controller-runtime client that internal/restore's K8sJobs uses: the log
// subresource (GetLogs(...).Stream) exists only on the typed CoreV1 client, and
// the Job create is available on both — so one client covers all three calls
// instead of the binding carrying two.
//
// INTEGRATION-ONLY: like K8sLogStreamer and K8sCluster this needs a live cluster;
// it compiles here but is exercised only against one, never by the hermetic test
// suite. The Oracle verifies the layer above it (Editor orchestration and error
// mapping) against a fake Runner, and the Job shape via the pure jobspec.
type K8sRunner struct {
	cs kubernetes.Interface
}

// NewK8sRunner builds a Runner over cs. Every per-operation parameter — the
// namespace included — travels in the JobParams the Editor renders, so there is no
// Config to retain here.
func NewK8sRunner(cs kubernetes.Interface) *K8sRunner {
	return &K8sRunner{cs: cs}
}

// Run creates the file Job, waits for its Pod to reach a terminal phase, and
// returns the JSON payload from the ResultPrefix line of that Pod's log.
//
// The Job name carries a fresh random OpID (FilesJobName), so a create collision is
// not an expected condition the way it is for restore — an AlreadyExists here means
// a 64-bit collision inside one TTL window and is reported rather than absorbed,
// because absorbing it would mean returning ANOTHER operation's output.
func (k *K8sRunner) Run(ctx context.Context, p JobParams) ([]byte, error) {
	job, err := FilesJob(p)
	if err != nil {
		return nil, err
	}
	if _, err := k.cs.BatchV1().Jobs(p.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("fileedit: create file job: %w", err)
	}

	pod, err := k.awaitPod(ctx, p)
	if err != nil {
		return nil, err
	}

	log, err := k.podLog(ctx, p.Namespace, pod.Name)
	if err != nil {
		return nil, err
	}

	payload, ok := extractResult(log)
	if !ok {
		// No marked line: the entrypoint died before printing (an unmountable volume,
		// an OOM kill, a deadline). The log tail travels in the error for the operator's
		// benefit — this error reaches felis-api's logs, while the caller gets the
		// generic 500 writeError produces, so no node detail leaks to the browser.
		return nil, fmt.Errorf("fileedit: file job %s produced no result (phase %s): %s",
			job.Name, pod.Status.Phase, tail(log))
	}
	return payload, nil
}

// awaitPod polls until the operation's Pod reaches a terminal phase. It selects by
// the per-invocation LabelOpID, so it can never observe a different operation's Pod
// — the reason that label exists.
//
// Both Succeeded and Failed are terminal and BOTH return the Pod rather than an
// error, because a caller-fault result (a path that escapes the root, a file that
// is too large) is printed and then exited on cleanly, and even a genuinely failed
// Pod may have printed a diagnosable result first. Deciding what the outcome MEANS
// is the caller's job (Run reads the printed result); this function only decides
// when there is nothing left to wait for.
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
			switch pods.Items[i].Status.Phase {
			case corev1.PodSucceeded, corev1.PodFailed:
				return &pods.Items[i], nil
			}
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			// The Editor's Timeout (or the client disconnecting) fired. The Job is left
			// alone deliberately: felis-api holds no jobs:delete, and the Job's own
			// activeDeadlineSeconds plus ttlSecondsAfterFinished retire it without help.
			return nil, fmt.Errorf("fileedit: timed out waiting for the file job to finish: %w", ctx.Err())
		}
	}
}

// podLog reads a finished Pod's log. Follow is off — the Pod has already
// terminated, so the log is complete and a follow would merely block until the
// stream closed.
func (k *K8sRunner) podLog(ctx context.Context, namespace, pod string) (string, error) {
	stream, err := k.cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: containerName,
	}).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("fileedit: read file job log: %w", err)
	}
	defer stream.Close()

	b, err := io.ReadAll(io.LimitReader(stream, maxLogBytes))
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("fileedit: read file job log: %w", err)
	}
	return string(b), nil
}

// extractResult finds the marked payload in a Pod log. It scans for the LAST line
// carrying ResultPrefix because pods/log returns stdout and stderr MERGED: a Go
// runtime warning or a libc message can appear anywhere in the stream, so the
// payload must be located by its marker rather than by position. Taking the last
// match rather than the first is the conservative choice — if a marker somehow
// appeared more than once, the final one is the operation's actual outcome.
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

// tail returns the last few hundred bytes of a log for an error message, so a
// diagnostic is useful without embedding an entire log in an error string.
func tail(log string) string {
	const n = 512
	log = strings.TrimSpace(log)
	if len(log) <= n {
		return log
	}
	return "..." + log[len(log)-n:]
}
