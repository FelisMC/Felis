package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// adminAPIWithBuildLogs is adminAPI plus a build-log streamer, so the §16
// build-log handler is exercised against a fake (the real K8sBuildLogStreamer's
// pod selection + pods/log follow are integration-only). A bare fakeBuilder
// satisfies the admin gate's interface dependency.
func adminAPIWithBuildLogs(streamer LogStreamer) *API {
	api := adminAPI(&fakeBuilder{})
	api.BuildLogs = streamer
	return api
}

// A valid build id streams the build Pod log as SSE and forwards the id to the
// build-namespace streamer (spec §16, §416 日志流复用 §8). A finite source (one
// line then EOF) lets the relay return without blocking on the heartbeat.
func TestBuildLogsStreamsForAdmin(t *testing.T) {
	streamer := &fakeLogStreamer{src: io.NopCloser(strings.NewReader("building image...\n"))}
	api := adminAPIWithBuildLogs(streamer)
	w := do(api.ExternalHandler(), "GET", "/api/v1/images/build/bld-1/logs", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if streamer.calls != 1 {
		t.Fatalf("streamer called %d times, want 1", streamer.calls)
	}
	if streamer.gotName != "bld-1" {
		t.Errorf("streamer got id %q, want bld-1 (the build id must reach the streamer)", streamer.gotName)
	}
	if !strings.Contains(w.Body.String(), "data: building image...\n\n") {
		t.Errorf("body missing the streamed log line as an SSE data event: %q", w.Body.String())
	}
}

// A malformed build id is rejected up front (400) and never reaches the streamer:
// the id becomes a label-selector value, so a crafted id (here a comma + '=' that
// would inject a second selector requirement) must be refused before selection.
func TestBuildLogsRejectsMalformedID(t *testing.T) {
	streamer := &fakeLogStreamer{src: io.NopCloser(strings.NewReader("x\n"))}
	api := adminAPIWithBuildLogs(streamer)
	w := do(api.ExternalHandler(), "GET", "/api/v1/images/build/bld-1,evil=x/logs", "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if decodeErr(t, w) != "bad_request" {
		t.Errorf("error code = %q, want bad_request", decodeErr(t, w))
	}
	if streamer.calls != 0 {
		t.Errorf("streamer was called %d times for a malformed id; must be 0 (no selector built)", streamer.calls)
	}
}

// With no BuildLogs streamer configured, the route reports 503 — but only after
// the admin gate and id validation, so the boundary is still enforced.
func TestBuildLogsUnconfiguredIs503(t *testing.T) {
	api := adminAPI(&fakeBuilder{}) // BuildLogs deliberately left nil
	w := do(api.ExternalHandler(), "GET", "/api/v1/images/build/bld-1/logs", "", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (%s)", w.Code, w.Body.String())
	}
	if decodeErr(t, w) != "build_logs_unavailable" {
		t.Errorf("error code = %q, want build_logs_unavailable", decodeErr(t, w))
	}
}

// No build Pod for the id (not scheduled yet, or GC'd after completion) is 404 —
// distinct from the 409 the §8 server console returns for a stopped server, since
// a build's pod absence is "not found", not "not running".
func TestBuildLogsNotFoundIs404(t *testing.T) {
	api := adminAPIWithBuildLogs(&fakeLogStreamer{err: ErrNotFound})
	w := do(api.ExternalHandler(), "GET", "/api/v1/images/build/bld-1/logs", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if decodeErr(t, w) != "not_found" {
		t.Errorf("error code = %q, want not_found", decodeErr(t, w))
	}
}

// A pod that exists but whose log stream cannot be opened (e.g. kaniko's image is
// still pulling, container Waiting) is a transient 503 the client retries.
func TestBuildLogsUnavailableIs503(t *testing.T) {
	api := adminAPIWithBuildLogs(&fakeLogStreamer{err: ErrConsoleUnavailable})
	w := do(api.ExternalHandler(), "GET", "/api/v1/images/build/bld-1/logs", "", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (%s)", w.Code, w.Body.String())
	}
	if decodeErr(t, w) != "build_logs_unavailable" {
		t.Errorf("error code = %q, want build_logs_unavailable", decodeErr(t, w))
	}
}
