package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func adminAPIWithBuildLogs(streamer LogStreamer) *API {
	api := adminAPI(&fakeBuilder{})
	api.BuildLogs = streamer
	return api
}

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
