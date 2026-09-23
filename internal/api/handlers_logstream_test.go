package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLogStreamer drives the §8 read-side handler without a cluster: it returns a
// canned source (or error) and records what it was asked to stream. The real
// K8sLogStreamer's pod selection and pods/log follow are integration-only, so the
// handler is tested against this fake (spec §8 读=pods/log follow).
type fakeLogStreamer struct {
	src     io.ReadCloser
	err     error
	calls   int
	gotName string
	gotCtx  context.Context

	// srcFromCtx, when set, builds the returned source from the context
	// StreamLogs actually receives, instead of returning the pre-baked src. The
	// teardown test uses it to prove the handler threads r.Context() through:
	// the real K8sLogStreamer opens the pods/log follow under the passed ctx, so
	// a source whose only cancellation path is that ctx faithfully models the
	// leak surface. A handler that passed context.Background() would hand this a
	// never-cancelled context and hang.
	srcFromCtx func(ctx context.Context) io.ReadCloser
}

func (f *fakeLogStreamer) StreamLogs(ctx context.Context, name string) (io.ReadCloser, error) {
	f.calls++
	f.gotName = name
	f.gotCtx = ctx
	if f.err != nil {
		return nil, f.err
	}
	if f.srcFromCtx != nil {
		return f.srcFromCtx(ctx), nil
	}
	return f.src, nil
}

// recordReadCloser is a finite log source that records whether Close ran, so the
// relay's teardown (deferred src.Close) can be asserted.
type recordReadCloser struct {
	r      *strings.Reader
	closed bool
}

func (s *recordReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *recordReadCloser) Close() error               { s.closed = true; return nil }

// ctxBlockingReadCloser emits one line, then blocks until its context is
// cancelled — modeling a live `pods/log` follow stream that yields boot output
// and then waits. It is how the disconnect-teardown test proves a client
// disconnect (request-context cancel) unblocks the relay and releases the stream.
type ctxBlockingReadCloser struct {
	ctx       context.Context
	first     []byte
	firstRead chan struct{}
	sentFirst bool
	closed    chan struct{}
}

func (b *ctxBlockingReadCloser) Read(p []byte) (int, error) {
	if !b.sentFirst {
		b.sentFirst = true
		n := copy(p, b.first)
		close(b.firstRead) // signal the relay has begun streaming
		return n, nil
	}
	// No more data until the request context is cancelled (client disconnect).
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *ctxBlockingReadCloser) Close() error {
	close(b.closed)
	return nil
}

// TestServerConsoleStream exercises the §8 read-side SSE relay end-to-end through
// the external app-tier router: ownership (owner/admin), the nil-Logs and
// open-stream failure surface, and the SSE framing itself. The real pod-log
// follow is integration-only; this drives the handler against a fakeLogStreamer.
func TestServerConsoleStream(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}

	// mk builds an API whose "survival" server is owned by owner1, with a fresh
	// fakeLogStreamer wired. Subtests override src/err as needed.
	mk := func() (*API, *fakeRepo, *fakeLogStreamer) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		streamer := &fakeLogStreamer{}
		api := newTestAPI(repo, newFakeCluster())
		api.Logs = streamer
		return api, repo, streamer
	}

	t.Run("owner attaches -> 200 SSE framing + flush + audit", func(t *testing.T) {
		api, repo, streamer := mk()
		streamer.src = &recordReadCloser{r: strings.NewReader("line one\nline two\n")}
		api.External = staticExternal{p: owner}

		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/console", "", nil)

		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("Content-Type = %q, want text/event-stream", ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("Cache-Control = %q, want no-cache", cc)
		}
		// Each log line is framed as a single SSE data event.
		for _, want := range []string{"data: line one\n\n", "data: line two\n\n"} {
			if !strings.Contains(w.Body.String(), want) {
				t.Fatalf("body missing %q; got %q", want, w.Body.String())
			}
		}
		if !w.Flushed {
			t.Fatal("SSE relay must flush per event (recorder not flushed)")
		}
		if !streamer.src.(*recordReadCloser).closed {
			t.Fatal("relay must Close the log source when the stream ends")
		}
		if streamer.gotName != "survival" {
			t.Fatalf("streamer saw name %q, want survival", streamer.gotName)
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "console.attach" || repo.audits[0].Actor != "owner1@example.net" {
			t.Fatalf("audit not written as expected: %+v", repo.audits)
		}
	})

	t.Run("admin attaches to another's server -> 200", func(t *testing.T) {
		api, _, streamer := mk()
		streamer.src = &recordReadCloser{r: strings.NewReader("boot\n")}
		api.External = staticExternal{p: &Principal{UserID: "admin1", Email: "admin1@example.net",
			Role: "admin", ViaAdminAccess: true}}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/console", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if streamer.calls != 1 {
			t.Fatalf("admin attach reached the streamer %d times, want 1", streamer.calls)
		}
	})

	t.Run("non-owner -> 403, no stream opened", func(t *testing.T) {
		api, _, streamer := mk()
		api.External = staticExternal{p: &Principal{UserID: "stranger", Role: "user"}}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/console", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if streamer.calls != 0 {
			t.Fatal("a forbidden caller must not open a log stream")
		}
	})

	t.Run("unknown server -> 404", func(t *testing.T) {
		api, _, streamer := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/missing/console", "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
		if streamer.calls != 0 {
			t.Fatal("an unknown server must not open a log stream")
		}
	})

	t.Run("nil Logs -> 503 console_unavailable", func(t *testing.T) {
		api, _, _ := mk()
		api.Logs = nil
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/console", "", nil)
		if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "console_unavailable" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})

	t.Run("no running pod -> 409 not_running, no audit", func(t *testing.T) {
		api, repo, streamer := mk()
		streamer.err = ErrNotFound
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/console", "", nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "not_running" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if len(repo.audits) != 0 {
			t.Fatalf("a failed attach must not audit: %+v", repo.audits)
		}
	})

	t.Run("stream unavailable -> 503 console_unavailable", func(t *testing.T) {
		api, _, streamer := mk()
		streamer.err = ErrConsoleUnavailable
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/console", "", nil)
		if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "console_unavailable" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
}

// TestServerConsoleStreamPerPrincipalCap pins the audit-hardening bound: a single
// principal may hold at most MaxStreamsPerPrincipal concurrent SSE streams, and an
// attach past that is shed with 429 too_many_streams BEFORE any upstream follow is
// opened. (Honest scope: this bounds the blast radius of the stalled-stream leak, it
// does NOT close the leak — the per-write deadline that severs a stalled stream is a
// separate slice.) With the principal already at its one-stream cap the next attach is
// refused without reaching the streamer; releasing the held slot lets an identical
// attach through, proving the 429 was the cap and not something else.
func TestServerConsoleStreamPerPrincipalCap(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	api := newTestAPI(repo, newFakeCluster())
	api.MaxStreamsPerPrincipal = 1
	streamer := &fakeLogStreamer{}
	api.Logs = streamer
	api.External = staticExternal{p: owner}

	// Occupy the principal's one stream slot, mimicking a live attach in flight. This
	// lazily builds the same one-slot limiter the handler consults.
	release, ok := api.streamGate().acquire(streamKey(owner))
	if !ok {
		t.Fatal("could not acquire the sole stream slot in test setup")
	}

	// A second concurrent attach is shed with 429 and never reaches the streamer.
	w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/console", "", nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap attach: code = %d, want 429 (%s)", w.Code, w.Body.String())
	}
	if code := decodeErr(t, w); code != "too_many_streams" {
		t.Fatalf("over-cap attach: error code = %q, want too_many_streams", code)
	}
	if streamer.calls != 0 {
		t.Fatalf("an over-cap attach must not open an upstream stream (streamer.calls = %d)", streamer.calls)
	}

	// Releasing the held slot lets an identical attach through: the finite source EOFs,
	// so the relay returns immediately with the SSE framing.
	release()
	streamer.src = &recordReadCloser{r: strings.NewReader("boot\n")}
	w = do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/console", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("after releasing the slot: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if streamer.calls != 1 {
		t.Fatalf("after release the attach should reach the streamer once, got %d", streamer.calls)
	}
}

// idleReadCloser is a perfectly quiet log follow: every Read blocks until the
// context is cancelled, yielding no line at all. It models a Minecraft server
// that has booted and gone silent (no chat, no log output), which is exactly the
// case the SSE heartbeat exists to keep alive.
type idleReadCloser struct {
	ctx    context.Context
	closed chan struct{}
}

func (b *idleReadCloser) Read(p []byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *idleReadCloser) Close() error {
	if b.closed != nil {
		close(b.closed)
	}
	return nil
}

// signalWriter wraps the recorder so the test learns the moment the relay makes
// its first body write WITHOUT racing on the recorder's buffer: on an idle stream
// that first write can only be a heartbeat (no data line will ever arrive), so
// closing `fired` there lets the test cancel deterministically instead of sleeping
// a guessed interval. Flush is forwarded because relayLogStream type-asserts
// http.Flusher and flushes every event.
type signalWriter struct {
	http.ResponseWriter
	fired chan struct{}
	once  sync.Once
}

func (s *signalWriter) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.fired) })
	return s.ResponseWriter.Write(p)
}

func (s *signalWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// TestServerConsoleHeartbeat proves the §8 relay keeps an idle stream warm: with
// no log line ever arriving, it must still emit SSE comment heartbeats (so an
// intermediary's idle timeout — Cloudflare's ~100s — never tears the console
// down) and must emit NO data event. heartbeatInterval is shrunk so a heartbeat
// lands within the test window; the first body write is necessarily that
// heartbeat, so the test waits for it rather than sleeping a fixed duration.
func TestServerConsoleHeartbeat(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 2 * time.Millisecond
	defer func() { heartbeatInterval = old }()

	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}}

	ctx, cancel := context.WithCancel(context.Background())
	streamer := &fakeLogStreamer{srcFromCtx: func(streamCtx context.Context) io.ReadCloser {
		return &idleReadCloser{ctx: streamCtx}
	}}
	api.Logs = streamer

	rec := httptest.NewRecorder()
	w := &signalWriter{ResponseWriter: rec, fired: make(chan struct{})}
	r := httptest.NewRequest("GET", "/api/v1/servers/survival/console", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		api.ExternalHandler().ServeHTTP(w, r)
		close(done)
	}()

	// Wait for the relay's first body write — on an idle stream, a heartbeat — then
	// disconnect. No sleep-on-a-guessed-interval.
	select {
	case <-w.fired:
	case <-time.After(2 * time.Second):
		t.Fatal("idle relay never emitted a heartbeat")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}

	// Read the body only after <-done, so the relay's writes happen-before this.
	body := rec.Body.String()
	if !strings.Contains(body, sseHeartbeat) {
		t.Fatalf("idle relay emitted no SSE heartbeat comment; body = %q", body)
	}
	// An idle stream carries keep-alives only — never a data event.
	if strings.Contains(body, "data:") {
		t.Fatalf("idle relay must emit only heartbeats, got a data event: %q", body)
	}
}

// interleaveWriter signals once the relay has written BOTH a data event and a
// heartbeat, so a test can prove the two coexist on one stream without racing on
// the recorder. Each relay event is a single Write (fmt.Fprintf / io.WriteString),
// and the relay is the lone writer, so inspecting p per Write is race-free.
type interleaveWriter struct {
	http.ResponseWriter
	sawData bool
	sawBeat bool
	both    chan struct{}
	once    sync.Once
}

func (b *interleaveWriter) Write(p []byte) (int, error) {
	s := string(p)
	if strings.HasPrefix(s, "data:") {
		b.sawData = true
	}
	if s == sseHeartbeat {
		b.sawBeat = true
	}
	if b.sawData && b.sawBeat {
		b.once.Do(func() { close(b.both) })
	}
	return b.ResponseWriter.Write(p)
}

func (b *interleaveWriter) Flush() {
	if f, ok := b.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// TestServerConsoleHeartbeatInterleavesWithData proves a heartbeat following a
// real log line does not corrupt it: the source emits one line then goes idle, so
// the relay writes a data event and then — past the (shrunk) interval — a
// keep-alive. The select serializes the two writes, so the data event must appear
// intact (no comment spliced into the middle of `data: …\n\n`).
func TestServerConsoleHeartbeatInterleavesWithData(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 2 * time.Millisecond
	defer func() { heartbeatInterval = old }()

	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}}

	ctx, cancel := context.WithCancel(context.Background())
	streamer := &fakeLogStreamer{srcFromCtx: func(streamCtx context.Context) io.ReadCloser {
		return &ctxBlockingReadCloser{
			ctx:       streamCtx,
			first:     []byte("boot line\n"),
			firstRead: make(chan struct{}),
			closed:    make(chan struct{}),
		}
	}}
	api.Logs = streamer

	rec := httptest.NewRecorder()
	w := &interleaveWriter{ResponseWriter: rec, both: make(chan struct{})}
	r := httptest.NewRequest("GET", "/api/v1/servers/survival/console", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		api.ExternalHandler().ServeHTTP(w, r)
		close(done)
	}()

	// Wait until the relay has emitted the data event AND a heartbeat after it.
	select {
	case <-w.both:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not produce both a data event and a heartbeat")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after disconnect")
	}

	body := rec.Body.String()
	// The data event must appear intact — a heartbeat must not splice into it.
	if !strings.Contains(body, "data: boot line\n\n") {
		t.Fatalf("data event not intact in interleaved stream; body = %q", body)
	}
	if !strings.Contains(body, sseHeartbeat) {
		t.Fatalf("no heartbeat after the idle gap; body = %q", body)
	}
}

// TestServerConsoleDisconnectTeardown proves the linchpin of the read-side relay:
// a client disconnect (request-context cancel) unblocks the follow stream and the
// deferred Close releases it — no leaked apiserver connection. The source emits
// one line, then blocks on its context; cancelling the request must make the
// handler return AND Close the source.
func TestServerConsoleDisconnectTeardown(t *testing.T) {
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}}

	ctx, cancel := context.WithCancel(context.Background())
	// The channels are owned by the test, but the source itself is built inside
	// StreamLogs from the context the handler passes in — NOT from the test's ctx.
	// This is what actually backs the teardown claim: the source's only
	// cancellation path is the context the handler threaded through, mirroring the
	// real K8sLogStreamer (GetLogs(...).Stream(ctx)). If handleServerConsole
	// streamed under context.Background() instead of r.Context(), this source's
	// second Read would block on a never-cancelled context, the handler would
	// never return, and <-done below would time out — the test fails closed on the
	// exact leak it exists to prevent.
	firstRead := make(chan struct{})
	closed := make(chan struct{})
	streamer := &fakeLogStreamer{srcFromCtx: func(streamCtx context.Context) io.ReadCloser {
		return &ctxBlockingReadCloser{
			ctx:       streamCtx,
			first:     []byte("boot progress 50%\n"),
			firstRead: firstRead,
			closed:    closed,
		}
	}}
	api.Logs = streamer

	rec := httptest.NewRecorder()
	w := &firstDataWriter{ResponseWriter: rec, data: make(chan struct{})}
	r := httptest.NewRequest("GET", "/api/v1/servers/survival/console", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		api.ExternalHandler().ServeHTTP(w, r)
		close(done)
	}()

	// Wait until the relay has streamed the first line and is blocked on the next
	// read, then simulate the client going away.
	select {
	case <-firstRead:
	case <-time.After(2 * time.Second):
		t.Fatal("relay never read the first log line")
	}
	// The first read is not enough: the relay still has to WRITE the event, and
	// cancelling in that gap raced the write against the teardown (a live CI flake
	// left the body empty). Wait for the write itself — the signal is closed after
	// the recorder's Write returns, so the body read below happens-after it.
	select {
	case <-w.data:
	case <-time.After(2 * time.Second):
		t.Fatal("relay never wrote the first data event to the response")
	}
	cancel()

	// The handler must return promptly...
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect (leaked stream)")
	}
	// ...and the deferred Close must have released the upstream stream.
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not Close the log source on disconnect")
	}
	// Belt-and-suspenders: the context StreamLogs received must itself be Done.
	// Read after <-done, so the write inside StreamLogs is happens-before this.
	// Together with the source-from-ctx wiring above, this nails the one
	// production-critical property — the relay streams under the request context.
	select {
	case <-streamer.gotCtx.Done():
	default:
		t.Fatal("StreamLogs did not receive the cancellable request context (gotCtx not Done)")
	}
	if !strings.Contains(rec.Body.String(), "data: boot progress 50%\n\n") {
		t.Fatalf("expected the first event before disconnect, got %q", rec.Body.String())
	}
}

// firstDataWriter signals once the relay has written a `data:` event into the
// wrapped recorder, so a test can disconnect only after the event is actually
// observable in the body — inspecting p per Write is race-free because the relay
// is the lone writer and each relay event is a single Write.
type firstDataWriter struct {
	http.ResponseWriter
	data chan struct{}
	once sync.Once
}

func (b *firstDataWriter) Write(p []byte) (int, error) {
	n, err := b.ResponseWriter.Write(p)
	if strings.HasPrefix(string(p), "data:") {
		b.once.Do(func() { close(b.data) })
	}
	return n, err
}

func (b *firstDataWriter) Flush() {
	if f, ok := b.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// lineOnceThenBlockReadCloser yields exactly one line, then blocks every later Read
// until Close is called. It models a live-but-quiet follow stream: the server printed
// one line and has since gone silent, so nothing on the SOURCE side can end the relay
// — the only thing that can is the client side (here, the write deadline firing on a
// stalled reader). Close unblocks the parked Read so the scan goroutine exits cleanly
// when relayLogStream tears down, mirroring how src.Close aborts a real pods/log Read.
type lineOnceThenBlockReadCloser struct {
	line      []byte
	sentFirst bool // touched only by the single scan goroutine's Read
	block     chan struct{}
	once      sync.Once
}

func newLineOnceThenBlock(line string) *lineOnceThenBlockReadCloser {
	return &lineOnceThenBlockReadCloser{line: []byte(line), block: make(chan struct{})}
}

func (c *lineOnceThenBlockReadCloser) Read(p []byte) (int, error) {
	if !c.sentFirst {
		c.sentFirst = true
		return copy(p, c.line), nil
	}
	<-c.block
	return 0, io.EOF
}

func (c *lineOnceThenBlockReadCloser) Close() error {
	c.once.Do(func() { close(c.block) })
	return nil
}

// closed reports whether Close ran. Reading a channel's closed-ness is race-free, so
// the test may call this from another goroutine once the relay has returned.
func (c *lineOnceThenBlockReadCloser) closed() bool {
	select {
	case <-c.block:
		return true
	default:
		return false
	}
}

// deadlineStallWriter models a client that connected — the header flush went out — and
// then stopped reading. Its Write buffers and returns at once (like net/http's bufio-
// backed *response, a small SSE line never touches the socket at Write); its plain
// Flush — the one-time header flush — returns immediately; but every deadline-gated
// FlushError blocks until the write deadline relayLogStream set, then reports
// os.ErrDeadlineExceeded, exactly how a real socket surfaces a SetWriteDeadline expiry
// on a stalled reader. http.NewResponseController(w).Flush() prefers FlushError over
// plain Flush, so the relay's per-event flush travels the blocking path while the
// header flush does not — which is why the guard has to route flushes through the
// ResponseController, not the bare http.Flusher whose Flush swallows the error.
type deadlineStallWriter struct {
	mu       sync.Mutex
	hdr      http.Header
	deadline time.Time
	sawDL    bool
}

func newDeadlineStallWriter() *deadlineStallWriter {
	return &deadlineStallWriter{hdr: http.Header{}}
}

func (s *deadlineStallWriter) Header() http.Header         { return s.hdr }
func (s *deadlineStallWriter) WriteHeader(int)             {}
func (s *deadlineStallWriter) Write(p []byte) (int, error) { return len(p), nil } // buffered: never blocks
func (s *deadlineStallWriter) Flush()                      {}                     // header flush: instant, best-effort

// FlushError is where the stalled socket bites: it blocks until the deadline the relay
// set via SetWriteDeadline, then returns the same error a real write reports when that
// deadline elapses. With no deadline set it returns nil — a healthy, instant flush.
func (s *deadlineStallWriter) FlushError() error {
	s.mu.Lock()
	d := s.deadline
	s.mu.Unlock()
	if d.IsZero() {
		return nil
	}
	t := time.NewTimer(time.Until(d))
	defer t.Stop()
	<-t.C
	return os.ErrDeadlineExceeded
}

func (s *deadlineStallWriter) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.deadline = t
	s.sawDL = true
	s.mu.Unlock()
	return nil
}

func (s *deadlineStallWriter) deadlineSet() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawDL
}

// TestRelayLogStreamWriteDeadlineSeversStalledReader closes the actual §8 relay leak
// (audit #1): on a client that connected but stopped reading — request context still
// live, socket write blocked — the relay must not pin its goroutine and upstream pod-log
// follow forever. The fix sets a per-write deadline (writeTimeout) via
// http.ResponseController before each event and abandons the stream when a flush exceeds
// it. deadlineStallWriter makes the flush block until that deadline then report
// os.ErrDeadlineExceeded, exactly as a stalled socket does; the source stays open and
// silent (never EOFs), so the ONLY thing that can end the relay is the deadline. Without
// the fix (a bare flusher.Flush that swallows the error), the relay would loop forever
// waiting for a line that never comes and this test would time out — it fails closed on
// the exact leak it guards.
func TestRelayLogStreamWriteDeadlineSeversStalledReader(t *testing.T) {
	orig := writeTimeout
	writeTimeout = 30 * time.Millisecond
	defer func() { writeTimeout = orig }()

	src := newLineOnceThenBlock("boot\n")
	w := newDeadlineStallWriter()
	// A LIVE request context: the client has NOT disconnected. r.Context() never fires
	// here, which is precisely why the write deadline — not a context cancel — has to be
	// what severs the stalled stream.
	r := httptest.NewRequest("GET", "/api/v1/servers/survival/console", nil)

	done := make(chan struct{})
	go func() {
		relayLogStream(w, r, src)
		close(done)
	}()

	select {
	case <-done:
		// The write deadline fired and the relay tore the stalled stream down.
	case <-time.After(2 * time.Second):
		t.Fatal("relayLogStream did not return on a stalled-but-open reader; the write deadline never severed the stream (leak)")
	}

	if !w.deadlineSet() {
		t.Fatal("relay never set a write deadline — the leak guard is not wired into the write path")
	}
	// Returning ran the deferred Close: the upstream follow (a real apiserver
	// connection) is released rather than leaked.
	if !src.closed() {
		t.Fatal("relay returned without closing the source — upstream pod-log follow leaked")
	}
}

// deadlineRecordWriter records the write deadlines the relay sets and never blocks on
// flush — a healthy client whose stream simply ends. It pins the deadline-CLEAR half of
// the leak guard: SetWriteDeadline stores every value, so the test can read back the
// LAST one the relay left behind after it returns. sawPositive proves a real per-write
// deadline was applied during streaming, so a broken fix that never sets a deadline at
// all cannot pass the clear-check by leaving the field zero throughout.
type deadlineRecordWriter struct {
	mu           sync.Mutex
	hdr          http.Header
	lastDeadline time.Time
	sawPositive  bool
}

func newDeadlineRecordWriter() *deadlineRecordWriter {
	return &deadlineRecordWriter{hdr: http.Header{}}
}

func (s *deadlineRecordWriter) Header() http.Header         { return s.hdr }
func (s *deadlineRecordWriter) WriteHeader(int)             {}
func (s *deadlineRecordWriter) Write(p []byte) (int, error) { return len(p), nil }
func (s *deadlineRecordWriter) Flush()                      {}
func (s *deadlineRecordWriter) FlushError() error           { return nil } // healthy: never blocks

func (s *deadlineRecordWriter) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.lastDeadline = t
	if !t.IsZero() {
		s.sawPositive = true
	}
	s.mu.Unlock()
	return nil
}

func (s *deadlineRecordWriter) finalDeadline() (last time.Time, sawPositive bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastDeadline, s.sawPositive
}

// TestRelayLogStreamClearsWriteDeadlineOnReturn pins the keep-alive hygiene half of the
// §8 leak guard (audit #1 follow-up). Server.WriteTimeout is deliberately UNSET so a
// healthy long SSE stream is never severed (cmd/felis api.go), and with it unset net/http
// never resets the connection's write deadline between keep-alive requests. So the
// per-write deadline the relay sets must be CLEARED when the relay returns — otherwise it
// leaks onto the NEXT request that reuses this pooled connection and fails that request's
// first write for no reason. Here a finite source EOFs cleanly; after the relay returns
// the writer's final deadline must be the zero value, and a positive deadline must have
// been set first (so a fix that never sets a deadline at all cannot pass by leaving zero
// the whole time).
func TestRelayLogStreamClearsWriteDeadlineOnReturn(t *testing.T) {
	src := &recordReadCloser{r: strings.NewReader("boot\n")}
	w := newDeadlineRecordWriter()
	r := httptest.NewRequest("GET", "/api/v1/servers/survival/console", nil)

	relayLogStream(w, r, src)

	last, sawPositive := w.finalDeadline()
	if !sawPositive {
		t.Fatal("relay never set a per-write deadline — the leak guard is not wired into the write path")
	}
	if !last.IsZero() {
		t.Fatalf("relay left a write deadline of %v set on return; it must clear it to the zero value so it cannot leak onto a reused keep-alive connection", last)
	}
	if !src.closed {
		t.Fatal("relay returned without closing the source")
	}
}
