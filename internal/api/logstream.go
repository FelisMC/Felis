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

// writeTimeout bounds how long a single SSE write+flush to the client may block on
// the socket before the relay abandons the stream. It is the leak guard's teeth: on
// a stalled-but-open reader (client connected, its TCP receive window shut, never
// reading) the flush — where net/http actually drains the socket, since it buffers
// the small "data:" line rather than writing it through — would otherwise block
// forever INSIDE the write, with the request context never firing (r.Context()
// cancels on an actual disconnect, not on a stall). That pins this goroutine and its
// upstream pod-log follow indefinitely. relayLogStream applies this as a per-write
// deadline via http.ResponseController, so an unresponsive client is torn down
// within writeTimeout of a stalled flush instead of leaking. It sits comfortably
// above any transient slow-client write (a data line is bytes-to-KB) yet well under
// the ~100s proxy idle drop. Best-effort: writers without deadline support
// (httptest.ResponseRecorder; some HTTP/2 origins) ignore it and the relay behaves
// exactly as before. It is a var ONLY so a test can shrink it; production never
// reassigns it.
var writeTimeout = 30 * time.Second

// A stream is authorized once, when it opens, and then may run for hours. So the
// relay asks again every streamRecheckEvery (the caller's streamGuard: is the
// session still live, does the caller still own the server) and ends the stream
// after streamMaxLifetime regardless. A withdrawn grant gets an "event: revoked"
// before the close, which the panel treats as final; the lifetime cap is a plain
// close, which EventSource answers by reconnecting through the full auth path,
// resuming from its Last-Event-ID (see logSinceFromRequest) instead of replaying
// the backlog. Both are vars only so a test can shrink them.
var (
	streamRecheckEvery = time.Minute
	streamMaxLifetime  = 30 * time.Minute
)

// streamGuard re-checks a running stream's authorization. nil means it still
// holds; an error ends the stream with "event: revoked". A guard that cannot
// reach its store should return nil: an outage is no verdict on the caller, and
// streamMaxLifetime still bounds how long the stream can outlive a revocation.
type streamGuard func(ctx context.Context) error

// sseRevoked is the last event a stream whose grant was withdrawn receives.
const sseRevoked = "event: revoked\ndata: access to this stream was withdrawn\n\n"

// maxResumeAge bounds how far back a Last-Event-ID may resume: past it the pod
// has likely restarted anyway, and the tailed backlog is the better start.
const maxResumeAge = time.Hour

type logSinceKey struct{}

// withLogSince asks the LogStreamer to start the follow at since instead of the
// tailed backlog. It rides the context so the LogStreamer interface (and its test
// fakes) stays one method.
func withLogSince(ctx context.Context, since time.Time) context.Context {
	return context.WithValue(ctx, logSinceKey{}, since)
}

func logSinceFromContext(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(logSinceKey{}).(time.Time)
	return t, ok && !t.IsZero()
}

// logSinceFromRequest reads the resume point an EventSource sends on reconnect.
// Every relayed line carries "id: <unix seconds>", so Last-Event-ID is the second
// the client last heard from; resuming at that second may repeat a line or two
// from it, which beats replaying 200 lines of backlog on every reconnect. Anything
// unparsable, in the future, or older than maxResumeAge is ignored.
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

// podLogOptions is the follow request both streamers send: the tailed backlog,
// or everything since the resume point when the client is reconnecting.
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
//
// still, when non-nil, is asked every streamRecheckEvery whether the caller may
// keep reading; the stream also ends after streamMaxLifetime (see streamGuard),
// and when closing is closed (API.CloseStreams, at shutdown).
func relayLogStream(w http.ResponseWriter, r *http.Request, src io.ReadCloser, still streamGuard, closing <-chan struct{}) {
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
	// rc carries the two capabilities plain http.Flusher lacks: SetWriteDeadline (to
	// bound a stalled write) and a Flush whose error is observable — http.Flusher.Flush
	// swallows the deadline-exceeded error that a stalled socket flush returns. The
	// per-write deadline set inside writeChunk is what severs an unresponsive client;
	// the initial header flush below stays a plain best-effort flush (no deadline).
	rc := http.NewResponseController(w)
	// Clear any per-write deadline on return. Server.WriteTimeout is deliberately UNSET
	// (cmd/felis api.go — a WriteTimeout would sever a healthy long SSE stream), and
	// with it unset net/http never resets the write deadline between keep-alive
	// requests. So a deadline left set by the last writeChunk would leak onto the NEXT
	// request that reuses this pooled connection and fail its first write for no reason.
	// The zero time clears it; best-effort, a no-op on writers without deadline support.
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Defeat proxy buffering (nginx / ingress) so events arrive promptly.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Best-effort header flush, deliberately WITHOUT a write deadline. A client that
	// stalls its receive window BEFORE these headers drain is therefore NOT severed by
	// writeTimeout at connect time — routing this flush through the deadline guard would
	// break the guard's test specificity, and the connect-time stall is already bounded
	// by the per-principal stream cap (#44). Only the mid-stream stall (every writeChunk
	// below) is CLOSED by the deadline guard, not merely bounded.
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
	recheck := time.NewTicker(streamRecheckEvery)
	defer recheck.Stop()
	expire := time.NewTimer(streamMaxLifetime)
	defer expire.Stop()

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				// The scan goroutine finished — the stream is over. Return and let
				// the deferred Close release the source.
				return
			}
			// One log line → one SSE "data:" event, written under a per-write deadline
			// so a stalled reader severs the stream (writeChunk → false) instead of
			// pinning this goroutine; the deferred Close then tears the upstream down.
			// The id is the resume point a reconnecting EventSource sends back.
			if !writeChunk(rc, w, "id: "+strconv.FormatInt(time.Now().Unix(), 10)+"\ndata: "+line+"\n\n") {
				return
			}
		case <-recheck.C:
			if still != nil && still(ctx) != nil {
				writeChunk(rc, w, sseRevoked)
				return
			}
		case <-expire.C:
			// A plain close: the client reconnects and is authorized afresh.
			return
		case <-closing:
			// The server is shutting down; the client reconnects to the next one.
			return
		case <-ticker.C:
			// No line for a whole interval: emit a comment so the connection stays
			// warm past the proxy idle timeout — same deadline-guarded write, so a
			// client that has gone silent-but-stalled is torn down here too.
			if !writeChunk(rc, w, sseHeartbeat) {
				return
			}
		case <-ctx.Done():
			// Client disconnected (request context cancelled). Return; defers run.
			return
		}
	}
}

// writeChunk writes one framed SSE chunk to the client under a fresh per-write
// deadline and flushes it, returning false when the client socket is gone so the
// caller tears the relay (and its upstream follow) down. The deadline is the leak
// guard: net/http buffers the small write and only touches the socket at Flush, so a
// stalled reader blocks there — without a deadline that block is unbounded and the
// request context never fires. Both errors are honored: the write error (a line
// larger than the buffer can block mid-write) and the flush error — rc.Flush
// surfaces the os.ErrDeadlineExceeded that plain http.Flusher.Flush swallows.
// SetWriteDeadline and rc.Flush are best-effort: on a writer without deadline
// support (httptest.ResponseRecorder; some HTTP/2 origins) the deadline is ignored
// and rc.Flush reduces to a plain, non-erroring flush, so behaviour is unchanged
// where the guard cannot apply.
func writeChunk(rc *http.ResponseController, w io.Writer, chunk string) bool {
	// Best-effort: an unsupported writer returns an error we ignore, leaving the
	// write unbounded exactly as before the guard existed.
	_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := io.WriteString(w, chunk); err != nil {
		return false
	}
	return rc.Flush() == nil
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
		// No running pod: the server is stopped or not yet scheduled.
		return nil, ErrNotFound
	}

	stream, err := k.clientset.CoreV1().Pods(k.namespace).GetLogs(podName,
		podLogOptions(ctx, serverLogContainer, k.tailLines)).Stream(ctx)
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
// Phase=Pending (the trivy scan and the push container have not started), so a running filter
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

	stream, err := k.clientset.CoreV1().Pods(k.namespace).GetLogs(podName,
		podLogOptions(ctx, build.ContainerKaniko, k.tailLines)).Stream(ctx)
	if err != nil {
		return nil, ErrConsoleUnavailable
	}
	return stream, nil
}
