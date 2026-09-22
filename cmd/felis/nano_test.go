package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// A stop signal that lands while a login is waiting on an upstream must let that login
// finish: the request is answered, and serveNano returns only afterwards.
func TestNanoDrainsInFlightLoginOnShutdown(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	srv := newAPIServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serveNano(ctx, srv, ln, io.Discard) }()

	got := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/session/minecraft/hasJoined")
		if err != nil {
			got <- -1
			return
		}
		resp.Body.Close()
		got <- resp.StatusCode
	}()
	<-entered
	stop()
	select {
	case <-done:
		t.Fatal("serveNano returned while a login was still in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if code := <-got; code != http.StatusNoContent {
		t.Fatalf("in-flight login got %d, want its answer (204)", code)
	}
	if rc := <-done; rc != 0 {
		t.Fatalf("serveNano = %d after a clean drain, want 0", rc)
	}
}

// The request log prints text the caller chose. A bidi override must not reorder the line,
// an invalid byte must not make journald store the entry as a blob, and a huge query must
// not become a huge log line. serverId is left out so the handler answers without asking
// any source.
func TestNanoRequestLogIsQuotedAndCapped(t *testing.T) {
	const rlo = rune(0x202e) // RIGHT-TO-LEFT OVERRIDE
	var log bytes.Buffer
	h := nanoHandler(nil, &log)
	target := "/session/minecraft/hasJoined?username=" + string(rlo) + "evil" + string([]byte{0x9b}) + "31m" + strings.Repeat("a", 4096)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))

	line := log.String()
	if strings.ContainsRune(line, rlo) || !utf8.ValidString(line) {
		t.Fatalf("raw caller bytes reached the log: %q", line)
	}
	if escaped := strings.Trim(strconv.QuoteRune(rlo), "'"); !strings.Contains(line, escaped) {
		t.Fatalf("log line %q should show the override escaped as %s", line, escaped)
	}
	if len(line) > 2*nanoLogURIMax {
		t.Fatalf("log line is %d bytes for a %d-byte URI; want it capped", len(line), len(target))
	}
}
