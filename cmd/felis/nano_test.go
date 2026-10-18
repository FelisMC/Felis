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

func TestNanoListensOnLoopbackByDefault(t *testing.T) {
	host, _, err := net.SplitHostPort(nanoDefaultListen)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		t.Fatalf("default -listen %q is not a loopback address", nanoDefaultListen)
	}
}

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
