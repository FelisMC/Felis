package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

	r := httptest.NewRequest("GET", "/api/v1/servers/survival/console", nil).WithContext(ctx)
	w := httptest.NewRecorder()

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
	if !strings.Contains(w.Body.String(), "data: boot progress 50%\n\n") {
		t.Fatalf("expected the first event before disconnect, got %q", w.Body.String())
	}
}
