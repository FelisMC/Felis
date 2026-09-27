package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// openPodStore retries a refused first dial for podDBWindow, saying so on stderr
// under the calling command's name each time.
func TestOpenPodStoreRetriesARefusedDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	window, interval := podDBWindow, podDBInterval
	podDBWindow, podDBInterval = 200*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { podDBWindow, podDBInterval = window, interval })

	var stderr bytes.Buffer
	start := time.Now()
	_, err = openPodStore(context.Background(), fmt.Sprintf("postgres://felis@%s/felis?sslmode=disable", addr), "reaper", &stderr)
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("err = %v, want a refused dial", err)
	}
	if elapsed := time.Since(start); elapsed < podDBWindow {
		t.Fatalf("gave up after %s, inside the %s window", elapsed, podDBWindow)
	}
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if len(lines) < 2 || len(lines) > 11 {
		t.Fatalf("%d retry lines in a 200ms window at 20ms:\n%s", len(lines), stderr.String())
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "felis reaper: failed to connect to `user=felis database=felis`: ") ||
			!strings.HasSuffix(l, "; retrying (a pod that has just started waits for the network policy to admit it)") {
			t.Fatalf("retry line %q", l)
		}
	}
}
