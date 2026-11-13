package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNanoTellsADatabaseFreeInstallHowToMoveOn(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "felis.toml")
	old := "[[auth_source]]\ntag = \"littleskin\"\nprefix = \"ls\"\nurl = \"https://littleskin.cn/api/yggdrasil/sessionserver/session/minecraft/hasJoined\"\n"
	if err := os.WriteFile(cfg, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if rc := cmdNano([]string{"-config", cfg}, io.Discard, &stderr); rc != 1 {
		t.Fatalf("cmdNano = %d on a config without [database], want 1", rc)
	}
	if !strings.Contains(stderr.String(), "FELIS_INSTALL_MODE=nano") || !strings.Contains(stderr.String(), "[[auth_source]] entries carry over") {
		t.Fatalf("stderr %q should say how to move a database-free install on", stderr.String())
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
	go func() { done <- serveNano(ctx, io.Discard, nanoListener{srv, ln, nil}) }()

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

func TestNanoListensOnLoopbackByDefault(t *testing.T) {
	host, _, err := net.SplitHostPort(nanoDefaultListen)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		t.Fatalf("default -listen %q is not a loopback address", nanoDefaultListen)
	}
}
