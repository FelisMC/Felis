package rcon_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"felis.lolicon.best/internal/rcon"
)

func TestDialContextCancelsAuthentication(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "handshake timeout"} {
		t.Run(mode, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			auth := make(chan struct{})
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				if _, typ, _, err := readFramePacket(conn); err != nil || typ != 3 {
					return
				}
				close(auth)
				_, _ = io.Copy(io.Discard, conn)
			}()
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			dialTimeout := time.Second
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
				want = context.DeadlineExceeded
			}
			if mode == "handshake timeout" {
				dialTimeout = 150 * time.Millisecond
				want = context.DeadlineExceeded
			}
			defer cancel()
			done := make(chan error, 1)
			go func() {
				conn, err := rcon.DialContext(ctx, ln.Addr().String(), "fake-password", dialTimeout)
				if conn != nil {
					conn.Close()
				}
				done <- err
			}()
			select {
			case <-auth:
			case <-time.After(time.Second):
				t.Fatal("authentication was not attempted")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("DialContext error = %v, want %v", err, want)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("authentication outlived its context")
			}
			select {
			case <-closed:
			case <-time.After(500 * time.Millisecond):
				t.Fatal("canceled authentication left the connection open")
			}
		})
	}
}

func TestDialContextRejectsAuthentication(t *testing.T) {
	f := startFakeRCON(t, "fake-password", nil)
	defer f.stop()
	conn, err := rcon.DialContext(context.Background(), f.addr(), "wrong-password", time.Second)
	if conn != nil {
		conn.Close()
		t.Fatal("rejected authentication produced a connection")
	}
	if !errors.Is(err, rcon.ErrAuthFailed) {
		t.Fatalf("DialContext error = %v, want authentication failure", err)
	}
}

func TestDialContextReleasesHandshakeCancellation(t *testing.T) {
	f := startFakeRCON(t, "fake-password", map[string]string{"list": "ok"})
	defer f.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	conn, err := rcon.DialContext(ctx, f.addr(), "fake-password", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	<-ctx.Done()
	if got, err := conn.Execute("list"); err != nil || got != "ok" {
		t.Fatalf("authenticated connection inherited handshake cancellation: reply=%q error=%v", got, err)
	}
}

func TestDialContextRejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := rcon.DialContext(ctx, "127.0.0.1:1", "fake-password", time.Second)
	if conn != nil {
		conn.Close()
		t.Fatal("canceled context produced a connection")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext error = %v, want canceled", err)
	}
}
