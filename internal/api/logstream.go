package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/build"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// LogStreamer is the read-side console channel (spec §8, 读写分离: 读=pods/log
// follow). StreamLogs opens a *follow* stream of the named server's container log
// and returns it as an io.ReadCloser, which relayLogStream copies to the client
// as Server-Sent Events (spec §262: GET /servers/{name}/console # SSE). It is the
// counterpart to Console (写=RCON): the read side never dials RCON and never
// resolves the RCON password — it only reads pod logs (看 join/聊天/异步打印 and
// boot progress).
//
// Contract: it returns ErrNotFound when the server has no running pod (stopped or
// not yet scheduled — the handler maps it to 409 not_running) and
// ErrConsoleUnavailable when a pod exists but its log stream cannot be opened (the
// handler maps it to 503). The returned stream MUST be closed by the caller;
// relayLogStream does so.
//
// It is an interface so the handler is tested against a fake
// (handlers_logstream_test.go); the client-go implementation (K8sLogStreamer) is
// integration-only.
type LogStreamer interface {
	StreamLogs(ctx context.Context, name string) (io.ReadCloser, error)
}

// maxLogLineBytes bounds a single log line the relay will buffer before it gives
// up on the stream. Real server log lines are well under 1 KiB; the generous cap
// (256 KiB) defends against a pathological unbounded line forcing felis-api to
// buffer without limit. A line longer than this ends the stream rather than
// growing memory without bound.
const maxLogLineBytes = 256 * 1024

// sseHeartbeat is the keep-alive the relay emits on an idle stream. A line whose
// first character is ':' is an SSE *comment*: EventSource ignores it entirely (it
// is never delivered as a message), but it is still bytes on the wire, which is
// all a proxy idle timer cares about. Without it a quiet Minecraft server (no
// chat / no log output) would have its console connection torn down by an
// intermediary, forcing a reconnect that replays the tail backlog.
const sseHeartbeat = ": keepalive\n\n"

// heartbeatInterval is how often relayLogStream emits sseHeartbeat while no log
// line is flowing. It must sit comfortably under the shortest idle timeout in the
// path — Cloudflare's proxy drops an idle streamed response at ~100s — while
// staying quiet enough not to be chatty; 25s gives ~4 keep-alives per timeout
// window. It is a var, not a const, ONLY so a test can shrink it to observe a
// heartbeat without waiting; production never reassigns it.
var heartbeatInterval = 25 * time.Second

// relayLogStream is the shared §8 read-side relay: it copies a line-oriented log
// source to the client as Server-Sent Events (spec §262 SSE, NOT WebSocket). It
// is the single reusable artifact the server console (handleServerConsole) and,
// in a later micro-slice, the build-log endpoint (handleBuildLogs, spec §16/§416
// 日志流复用 §8) both funnel through, so the SSE framing and flush discipline are
// written and tested exactly once.
//
// It MUST be called only after the caller has committed to streaming — every
// error case resolved and already written as a normal JSON envelope. Once the SSE
// headers ship the status code is fixed and no error envelope can follow, so the
// handler maps nil-dep / not-running / unavailable BEFORE handing the source here.
//
// Teardown is governed by the caller's request context: the source is opened with
// r.Context(), so a client disconnect cancels it, the underlying Read errors, the
// scan loop exits, and the deferred Close releases the upstream stream (no leaked
// apiserver connection).
func relayLogStream(w http.ResponseWriter, r *http.Request, src io.ReadCloser) {
	defer src.Close()

	// SSE needs per-event flushing; without a Flusher the bytes buffer and never
	// reach the client. net/http's ResponseWriter implements it (so does
	// httptest.ResponseRecorder). If somehow absent, fail as a clean 500 BEFORE any
	// SSE byte — at this point no header has been written, so the status is free.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, newError(http.StatusInternalServerError, "internal",
			"streaming is unsupported by this server"))
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Defeat proxy buffering (nginx / ingress) so events arrive promptly.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// bufio.Scanner.Scan blocks until a line arrives, so to interleave a periodic
	// keep-alive on an idle stream the scan runs in a goroutine that feeds a
	// channel, and this loop selects those lines against a ticker. The child
	// context — cancelled the moment this function returns — is the leak guard: it
	// unblocks the goroutine even if it is parked on the channel send (a client
	// going away while a backlog line is mid-handoff), which closing src alone
	// would not. w is written ONLY here in the select loop, never by the goroutine,
	// so there is a single writer to the ResponseWriter.
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
		// Scan returned false: EOF (stream ended) or a Read error (the source's
		// context was cancelled on client disconnect, or the upstream closed).
	}()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				// The scan goroutine finished — the stream is over. Return and let
				// the deferred Close release the source.
				return
			}
			// One log line → one SSE "data:" event. A write error means the client
			// side is gone; stop (the deferred Close tears the upstream down too).
			if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// No line for a whole interval: emit a comment so the connection stays
			// warm past the proxy idle timeout. A write error means the client is
			// gone; stop.
			if _, err := io.WriteString(w, sseHeartbeat); err != nil {
				return
			}
			flusher.Flush()
		case <-ctx.Done():
			// Client disconnected (request context cancelled). Return; defers run.
			return
		}
	}
}

// serverLogContainer is the container whose logs the read-side relay streams. It
// mirrors the operator's pod container name (internal/operator builders —
// containerName), duplicated here for the same reason rconEndpoint duplicates the
// RCON address convention: the api package does not import internal/operator, so
// the single shared convention is restated with this note rather than coupling
// the app face across the runtime boundary.
const serverLogContainer = "minecraft"

// defaultLogTailLines bounds the backlog a fresh console attach replays before it
// switches to live follow, so opening the console does not dump an entire pod log
// history. The live tail (chat / join / boot progress) is what matters.
const defaultLogTailLines = 200

// K8sLogStreamer is the production LogStreamer: it finds the server's pod by the
// operator's LabelServer selector (robust to the StatefulSet's <name>-0 pod
// naming) and opens a follow stream of its container log via client-go
// (pods/log). It needs a typed kubernetes.Interface clientset, NOT the
// controller-runtime client.Client K8sConsole uses, because the log subresource
// (GetLogs(...).Stream) lives only on the typed CoreV1 client.
//
// INTEGRATION-ONLY: like K8sConsole / K8sCluster this needs a live cluster; it
// compiles here but is exercised only against a real cluster, never by the
// hermetic api_test.go suite. The Oracle verifies the handler + relay layer
// (handleServerConsole, relayLogStream) against a fake LogStreamer.
//
// Security: the read side never touches the RCON Secret or password — it only
// reads pod logs, which is why its RBAC grant is the minimal pods:list (to find
// the pod) + pods/log:get (to read it), and nothing more (internal/platform
// APIMinecraftRole).
type K8sLogStreamer struct {
	clientset kubernetes.Interface
	namespace string
	tailLines int64
}

// NewK8sLogStreamer builds a LogStreamer over cs, scoped to namespace.
func NewK8sLogStreamer(cs kubernetes.Interface, namespace string) *K8sLogStreamer {
	return &K8sLogStreamer{clientset: cs, namespace: namespace, tailLines: defaultLogTailLines}
}

// StreamLogs selects the server's running pod by label and opens a follow stream
// of its container log. A missing / stopped server (no running pod) is
// ErrNotFound (the handler maps it to 409 not_running); any failure to open the
// stream collapses to ErrConsoleUnavailable (handler → 503) so no driver detail
// leaks. The stream is opened with ctx, so the handler's request context cancels
// it on client disconnect, unblocking the relay and releasing the connection.
func (k *K8sLogStreamer) StreamLogs(ctx context.Context, name string) (io.ReadCloser, error) {
	// Select by the StatefulSet's own selector label (the server name); robust to
	// the <name>-0 pod naming. The name is DNS-1123-validated upstream, so it is a
	// safe label-selector value.
	pods, err := k.clientset.CoreV1().Pods(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: v1alpha1.LabelServer + "=" + name,
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
		// No running pod: the server is stopped or not yet scheduled.
		return nil, ErrNotFound
	}

	tail := k.tailLines
	stream, err := k.clientset.CoreV1().Pods(k.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: serverLogContainer,
		Follow:    true,
		TailLines: &tail,
	}).Stream(ctx)
	if err != nil {
		return nil, ErrConsoleUnavailable
	}
	return stream, nil
}

// K8sBuildLogStreamer is the build-namespace LogStreamer (spec §16, §416 日志流复用
// §8). It is the read side of the build subsystem: it finds the build Job's Pod by
// the build-id label and follows the kaniko container's log — the build/push
// output an admin watches live as a build runs. It deliberately does NOT reuse
// K8sLogStreamer's PodRunning filter: a Pod running its kaniko *initContainer* is
// Phase=Pending (the main trivy container has not started), so a running filter
// would never match a live build. Trivy's CRITICAL-CVE verdict is the admission
// gate, surfaced via the build status (handleGetBuild), not through this stream.
//
// INTEGRATION-ONLY: like K8sLogStreamer it needs a live cluster — the pods/log
// subresource (GetLogs(...).Stream) lives only on the typed CoreV1 client. The
// Oracle verifies the handler + relay (handleBuildLogs, relayLogStream) against a
// fake LogStreamer (images_test.go). The buildID is validated as a label value by
// the handler BEFORE it reaches here, so the selector is injection-safe.
//
// Known limitation: this stream carries the kaniko container only. While kaniko's
// image is still pulling (Pod Pending, container Waiting) GetLogs errors and the
// handler returns 503 — a transient retry state, not a failure; once kaniko
// starts, its log (plus the tailed backlog) flows. Trivy's per-CVE scan detail is
// never streamed — only its pass/fail verdict reaches the admin, via build status.
//
// Security: like the §8 read side this never touches the build SA token, the RCON
// Secret, or any password — it only lists Pods and reads pod logs, which is the
// minimal felis-api-builds grant (pods:list + pods/log:get, internal/platform
// APIBuildRole).
type K8sBuildLogStreamer struct {
	clientset kubernetes.Interface
	namespace string
	tailLines int64
}

// NewK8sBuildLogStreamer builds a build-namespace LogStreamer over cs, scoped to
// namespace. namespace MUST be the same value the Builder renders Jobs into
// (cfg.Registry.BuildNamespace), so the streamer looks where the build Pods
// actually run.
func NewK8sBuildLogStreamer(cs kubernetes.Interface, namespace string) *K8sBuildLogStreamer {
	return &K8sBuildLogStreamer{clientset: cs, namespace: namespace, tailLines: defaultLogTailLines}
}

// StreamLogs selects the build Job's Pod by the build-id label and opens a follow
// stream of the kaniko container's log. No Pod (the build is not yet scheduled, or
// its Pod was garbage-collected after completion) is ErrNotFound (the handler maps
// it to 404); any failure to open the stream collapses to ErrConsoleUnavailable
// (handler → 503) so no driver detail leaks. The stream is opened with ctx, so the
// handler's request context cancels it on client disconnect, unblocking the relay
// and releasing the apiserver connection.
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
	// backoffLimit=0 + RestartPolicyNever (build.BuildJob) means a build Job creates
	// at most one Pod, so the first match is the build's Pod.
	podName := pods.Items[0].Name

	tail := k.tailLines
	stream, err := k.clientset.CoreV1().Pods(k.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: build.ContainerKaniko,
		Follow:    true,
		TailLines: &tail,
	}).Stream(ctx)
	if err != nil {
		return nil, ErrConsoleUnavailable
	}
	return stream, nil
}
