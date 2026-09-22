package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"felis.lolicon.best/internal/api"
)

// [server] listen in a nano config reads like the bind address but is not one; nano must
// say so. The -listen value cannot be bound, so cmdNano returns right after loading.
func TestNanoWarnsThatServerListenIsIgnored(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "felis.toml")
	if err := os.WriteFile(cfg, []byte("[server]\nlisten = \"0.0.0.0:9999\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if rc := cmdNano([]string{"-config", cfg, "-listen", "127.0.0.1:-1"}, io.Discard, &stderr); rc != 1 {
		t.Fatalf("cmdNano = %d, want 1 from the unbindable -listen", rc)
	}
	if !strings.Contains(stderr.String(), `listen = "0.0.0.0:9999" is ignored`) {
		t.Fatalf("stderr %q should say the configured listen is ignored", stderr.String())
	}

	// With no [server] table at all there is nothing to warn about.
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	_ = cmdNano([]string{"-config", cfg, "-listen", "127.0.0.1:-1"}, io.Discard, &stderr)
	if strings.Contains(stderr.String(), "is ignored") {
		t.Fatalf("stderr %q warns about a listen the operator never set", stderr.String())
	}
}

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

// The nano delivery path: the shared handler behind nano's stub store must admit a login its
// source validated. nanoStubRepo implements only the bar-list lookup, so a new store call in
// handleHasJoined would reach its nil embedded Repo and panic here, while the full-api tests,
// which use a complete fake store, stay green.
func TestNanoAdmitsAValidatedLogin(t *testing.T) {
	const id = "069a79f444e94726a5befca90e38aaf5"
	ygg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"`+id+`","name":"Notch"}`)
	}))
	defer ygg.Close()
	h := api.HasJoinedHandler([]api.AuthSource{{Tag: "mojang", URL: ygg.URL, Identity: true}}, nanoStubRepo{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/session/minecraft/hasJoined?username=Notch&serverId=abc", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), id) {
		t.Fatalf("code = %d body = %q, want the validated profile", w.Code, w.Body.String())
	}
}

// An unauthenticated relay on a public address spends this host's Mojang rate limit for
// anyone who finds it, so the default bind has to stay loopback.
func TestNanoListensOnLoopbackByDefault(t *testing.T) {
	host, _, err := net.SplitHostPort(nanoDefaultListen)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		t.Fatalf("default -listen %q is not a loopback address", nanoDefaultListen)
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
