package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/build"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// LogStreamer returns ErrNotFound without a running pod and ErrConsoleUnavailable on open failure; the caller closes the stream.
type LogStreamer interface {
	StreamLogs(ctx context.Context, name string) (io.ReadCloser, error)
}

const maxLogLineBytes = 256 * 1024

const sseHeartbeat = ": keepalive\n\n"

// heartbeatInterval must stay below Cloudflare's approximately 100-second idle timeout.
var heartbeatInterval = 25 * time.Second

// writeTimeout bounds stalled clients, whose closed receive windows do not cancel r.Context().
var writeTimeout = 30 * time.Second

// Periodic authorization checks and a maximum lifetime bound access after revocation.
var (
	streamRecheckEvery = time.Minute
	streamMaxLifetime  = 30 * time.Minute
)

// streamGuard returns nil on store outages; streamMaxLifetime still bounds access.
type streamGuard func(ctx context.Context) error

const sseRevoked = "event: revoked\ndata: access to this stream was withdrawn\n\n"

const maxResumeAge = time.Hour

type logSinceKey struct{}

func withLogSince(ctx context.Context, since time.Time) context.Context {
	return context.WithValue(ctx, logSinceKey{}, since)
}

func logSinceFromContext(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(logSinceKey{}).(time.Time)
	return t, ok && !t.IsZero()
}

// logSinceFromRequest accepts Unix-second Last-Event-ID values within maxResumeAge and no later than now.
func logSinceFromRequest(r *http.Request, now time.Time) (time.Time, bool) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	t := time.Unix(secs, 0)
	if t.After(now) || now.Sub(t) > maxResumeAge {
		return time.Time{}, false
	}
	return t, true
}

func podLogOptions(ctx context.Context, container string, tail int64) *corev1.PodLogOptions {
	opts := &corev1.PodLogOptions{Container: container, Follow: true}
	if since, ok := logSinceFromContext(ctx); ok {
		st := metav1.NewTime(since)
		opts.SinceTime = &st
		return opts
	}
	opts.TailLines = &tail
	return opts
}

// relayLogStream commits SSE headers; validate JSON error conditions before calling it.
func relayLogStream(w http.ResponseWriter, r *http.Request, src io.ReadCloser, still streamGuard, closing <-chan struct{}) {
	defer src.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, newError(http.StatusInternalServerError, "internal",
			"streaming is unsupported by this server"))
		return
	}
	// ResponseController exposes flush errors that http.Flusher discards.
	rc := http.NewResponseController(w)
	// Clear the deadline so it cannot carry into the next keep-alive request.
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering
	w.WriteHeader(http.StatusOK)
	// A stalled header flush is bounded by the per-principal stream cap, not a write deadline.
	flusher.Flush()

	// Only the relay loop writes the response; cancellation releases scanner sends when the loop exits.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	lines := make(chan string)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(src)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	recheck := time.NewTicker(streamRecheckEvery)
	defer recheck.Stop()
	expire := time.NewTimer(streamMaxLifetime)
	defer expire.Stop()

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return
			}
			if !writeChunk(rc, w, "id: "+strconv.FormatInt(time.Now().Unix(), 10)+"\ndata: "+line+"\n\n") {
				return
			}
		case <-recheck.C:
			if still != nil && still(ctx) != nil {
				writeChunk(rc, w, sseRevoked)
				return
			}
		case <-expire.C:
			return
		case <-closing:
			return
		case <-ticker.C:
			if !writeChunk(rc, w, sseHeartbeat) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// writeChunk sets a deadline for both writes and flushes; a stalled socket may never cancel the request context.
func writeChunk(rc *http.ResponseController, w io.Writer, chunk string) bool {
	// Writers without deadline support leave the write unbounded.
	_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := io.WriteString(w, chunk); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// serverLogContainer must match the operator's containerName.
const serverLogContainer = "minecraft"

const defaultLogTailLines = 200

// K8sLogStreamer requires pods:list and pods/log:get; log streaming does not require RCON credentials.
type K8sLogStreamer struct {
	clientset kubernetes.Interface
	namespace string
	tailLines int64
}

func NewK8sLogStreamer(cs kubernetes.Interface, namespace string) *K8sLogStreamer {
	return &K8sLogStreamer{clientset: cs, namespace: namespace, tailLines: defaultLogTailLines}
}

// StreamLogs maps missing running pods to ErrNotFound and stream-open failures to ErrConsoleUnavailable.
func (k *K8sLogStreamer) StreamLogs(ctx context.Context, name string) (io.ReadCloser, error) {
	// The server name is validated upstream as a DNS-1123 label.
	pods, err := k.clientset.CoreV1().Pods(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: v1alpha1.LabelServer + "=" + name + "," + v1alpha1.LabelComponent + "=" + gamePodComponent,
	})
	if err != nil {
		return nil, ErrConsoleUnavailable
	}

	podName := ""
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			podName = pods.Items[i].Name
			break
		}
	}
	if podName == "" {
		return nil, ErrNotFound
	}

	stream, err := k.clientset.CoreV1().Pods(k.namespace).GetLogs(podName,
		podLogOptions(ctx, serverLogContainer, k.tailLines)).Stream(ctx)
	if err != nil {
		return nil, ErrConsoleUnavailable
	}
	return stream, nil
}

// K8sBuildLogStreamer follows only kaniko logs; an active init container may still have Pod phase Pending.
type K8sBuildLogStreamer struct {
	clientset kubernetes.Interface
	namespace string
	tailLines int64
}

// NewK8sBuildLogStreamer requires the same namespace used by Builder for its Jobs.
func NewK8sBuildLogStreamer(cs kubernetes.Interface, namespace string) *K8sBuildLogStreamer {
	return &K8sBuildLogStreamer{clientset: cs, namespace: namespace, tailLines: defaultLogTailLines}
}

// StreamLogs maps missing build pods to ErrNotFound and stream-open failures to ErrConsoleUnavailable.
func (k *K8sBuildLogStreamer) StreamLogs(ctx context.Context, buildID string) (io.ReadCloser, error) {
	pods, err := k.clientset.CoreV1().Pods(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: build.LabelBuildID + "=" + buildID,
	})
	if err != nil {
		return nil, ErrConsoleUnavailable
	}
	if len(pods.Items) == 0 {
		return nil, ErrNotFound
	}
	// backoffLimit=0 and RestartPolicyNever limit each build Job to one Pod.
	podName := pods.Items[0].Name

	stream, err := k.clientset.CoreV1().Pods(k.namespace).GetLogs(podName,
		podLogOptions(ctx, build.ContainerKaniko, k.tailLines)).Stream(ctx)
	if err != nil {
		return nil, ErrConsoleUnavailable
	}
	return stream, nil
}
