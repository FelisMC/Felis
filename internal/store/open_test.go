package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// servePG answers every connection on ln like a PostgreSQL server would: the i-th
// connection (from 1) gets a FATAL error with the SQLSTATE answer(i) returns, or,
// for "", a trust login and an empty reply to each simple query, which is all a
// ping sends. It counts the connections in accepts.
func servePG(ln net.Listener, answer func(i int32) string, accepts *atomic.Int32) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		code := answer(accepts.Add(1))
		go func() {
			defer conn.Close()
			be := pgproto3.NewBackend(conn, conn)
			if _, err := be.ReceiveStartupMessage(); err != nil {
				return
			}
			if code != "" {
				be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: code, Message: "answered " + code})
				_ = be.Flush()
				return
			}
			be.Send(&pgproto3.AuthenticationOk{})
			be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if be.Flush() != nil {
				return
			}
			for {
				msg, err := be.Receive()
				if err != nil {
					return
				}
				switch msg.(type) {
				case *pgproto3.Query:
					be.Send(&pgproto3.EmptyQueryResponse{})
					be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
					if be.Flush() != nil {
						return
					}
				case *pgproto3.Terminate:
					return
				}
			}
		}()
	}
}

// freeAddr returns a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func testDSN(addr string) string {
	return fmt.Sprintf("postgres://felis@%s/felis?sslmode=disable", addr)
}

// A pod's first dials are refused until the network policy admits it; the open
// waits that out and connects once the server is reachable.
func TestOpenRetryingWaitsOutARefusedDial(t *testing.T) {
	addr := freeAddr(t)
	var accepts atomic.Int32
	go func() {
		time.Sleep(150 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		t.Cleanup(func() { ln.Close() })
		servePG(ln, func(int32) string { return "" }, &accepts)
	}()
	var retries []error
	drv, err := OpenRetrying(context.Background(), testDSN(addr), 5*time.Second, 20*time.Millisecond, func(err error) { retries = append(retries, err) })
	if err != nil {
		t.Fatalf("open: %v (after %d retries)", err, len(retries))
	}
	drv.Close()
	if len(retries) == 0 {
		t.Fatal("connected without a retry: the listener was up too early for this test to mean anything")
	}
	for _, r := range retries {
		if !errors.Is(r, syscall.ECONNREFUSED) {
			t.Fatalf("retried on %v, want only refused dials", r)
		}
	}
	if n := accepts.Load(); n != 1 {
		t.Fatalf("server saw %d connections, want 1", n)
	}
}

// The window bounds the wait, and Open itself tries once.
func TestOpenRetryingGivesUpAtTheWindow(t *testing.T) {
	addr := freeAddr(t)
	retries := 0
	start := time.Now()
	_, err := OpenRetrying(context.Background(), testDSN(addr), 200*time.Millisecond, 20*time.Millisecond, func(error) { retries++ })
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("err = %v, want a refused dial", err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("gave up after %s, want the 200ms window", elapsed)
	}
	if retries < 2 || retries > 11 {
		t.Fatalf("%d retries in a 200ms window at 20ms, want several and at most one per interval", retries)
	}

	// Open has no window: one refused dial is its answer (a retry would call the nil
	// onRetry and panic).
	if _, err := Open(context.Background(), testDSN(addr)); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("Open err = %v, want a refused dial", err)
	}
}

// What the server says is final, except that it is still starting.
func TestOpenRetryingTakesTheServersAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answer  func(i int32) string
		wantErr string // SQLSTATE of the final error; "" for a connection
		accepts int32
	}{
		{"bad password", func(int32) string { return "28P01" }, "28P01", 1},
		{"no such database", func(int32) string { return "3D000" }, "3D000", 1},
		{"starting up", func(i int32) string {
			if i <= 2 {
				return "57P03"
			}
			return ""
		}, "", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			var accepts atomic.Int32
			go servePG(ln, tc.answer, &accepts)
			retries := 0
			drv, err := OpenRetrying(context.Background(), testDSN(ln.Addr().String()), 2*time.Second, 10*time.Millisecond, func(error) { retries++ })
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				drv.Close()
			} else {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != tc.wantErr {
					t.Fatalf("err = %v, want SQLSTATE %s", err, tc.wantErr)
				}
			}
			if n := accepts.Load(); n != tc.accepts || retries != int(tc.accepts)-1 {
				t.Fatalf("server saw %d connections after %d retries, want %d", n, retries, tc.accepts)
			}
		})
	}
}

// A cancelled context ends the wait without another retry.
func TestOpenRetryingStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	retries := 0
	start := time.Now()
	if _, err := OpenRetrying(ctx, testDSN(freeAddr(t)), 5*time.Second, 20*time.Millisecond, func(error) { retries++ }); err == nil {
		t.Fatal("opened on a cancelled context")
	}
	if retries != 0 || time.Since(start) > time.Second {
		t.Fatalf("%d retries over %s on a cancelled context, want none", retries, time.Since(start))
	}
}
