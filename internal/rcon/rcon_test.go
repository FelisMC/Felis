package rcon_test

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"felis.lolicon.best/internal/rcon"
)

// fakeRCON is a minimal Source RCON server used to exercise the client
// hermetically (no real Minecraft server, no network beyond loopback).
type fakeRCON struct {
	ln       net.Listener
	password string
	replies  map[string]string
	wg       sync.WaitGroup
}

func startFakeRCON(t *testing.T, password string, replies map[string]string) *fakeRCON {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeRCON{ln: ln, password: password, replies: replies}
	f.wg.Add(1)
	go f.serve()
	return f
}

func (f *fakeRCON) addr() string { return f.ln.Addr().String() }

func (f *fakeRCON) stop() {
	f.ln.Close()
	f.wg.Wait()
}

func (f *fakeRCON) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeRCON) handle(conn net.Conn) {
	defer conn.Close()
	authed := false
	for {
		id, typ, body, err := readFramePacket(conn)
		if err != nil {
			return
		}
		switch typ {
		case 3: // AUTH
			if body == f.password {
				authed = true
				_ = writeFramePacket(conn, id, 2, "") // AUTH_RESPONSE, echo id
			} else {
				_ = writeFramePacket(conn, -1, 2, "") // failure
			}
		case 2: // EXECCOMMAND
			if !authed {
				_ = writeFramePacket(conn, -1, 0, "")
				continue
			}
			reply := f.replies[body]
			_ = writeFramePacket(conn, id, 0, reply)
		default:
			_ = writeFramePacket(conn, id, 0, "")
		}
	}
}

func writeFramePacket(w io.Writer, id, typ int32, body string) error {
	b := []byte(body)
	length := int32(4 + 4 + len(b) + 2)
	buf := make([]byte, 0, 4+length)
	buf = putI32(buf, length)
	buf = putI32(buf, id)
	buf = putI32(buf, typ)
	buf = append(buf, b...)
	buf = append(buf, 0, 0)
	_, err := w.Write(buf)
	return err
}

func readFramePacket(r io.Reader) (id, typ int32, body string, err error) {
	var lenBuf [4]byte
	if _, err = io.ReadFull(r, lenBuf[:]); err != nil {
		return 0, 0, "", err
	}
	length := int32(binary.LittleEndian.Uint32(lenBuf[:]))
	payload := make([]byte, length)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, 0, "", err
	}
	id = int32(binary.LittleEndian.Uint32(payload[0:4]))
	typ = int32(binary.LittleEndian.Uint32(payload[4:8]))
	body = string(payload[8 : length-2])
	return id, typ, body, nil
}

func putI32(buf []byte, v int32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	return append(buf, b[:]...)
}

func TestDialAndExecute(t *testing.T) {
	want := "There are 0 of a max of 20 players online:"
	f := startFakeRCON(t, "s3cret", map[string]string{"list": want})
	defer f.stop()

	c, err := rcon.Dial(f.addr(), "s3cret", 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	got, err := c.Execute("list")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != want {
		t.Errorf("Execute(list) = %q, want %q", got, want)
	}
}

func TestDialAuthFailure(t *testing.T) {
	f := startFakeRCON(t, "correct-horse", nil)
	defer f.stop()

	_, err := rcon.Dial(f.addr(), "wrong-password", 2*time.Second)
	if !errors.Is(err, rcon.ErrAuthFailed) {
		t.Fatalf("Dial with wrong password: got %v, want ErrAuthFailed", err)
	}
}

func TestDialUnreachable(t *testing.T) {
	// Reserved TEST-NET-1 address: connect should fail fast within the timeout.
	_, err := rcon.Dial("192.0.2.1:25575", "x", 200*time.Millisecond)
	if err == nil {
		t.Fatal("Dial to unreachable host: expected error, got nil")
	}
	if errors.Is(err, rcon.ErrAuthFailed) {
		t.Fatalf("Dial to unreachable host: got ErrAuthFailed, want a dial error")
	}
}

func TestExecuteGracefulShutdownSequence(t *testing.T) {
	// Mirrors the operator preStop hook: save then stop.
	f := startFakeRCON(t, "pw", map[string]string{
		"save-all flush": "Saved the game",
		"stop":           "Stopping the server",
	})
	defer f.stop()

	c, err := rcon.Dial(f.addr(), "pw", 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if out, err := c.Execute("save-all flush"); err != nil || out != "Saved the game" {
		t.Fatalf("save-all flush = %q, %v", out, err)
	}
	if out, err := c.Execute("stop"); err != nil || out != "Stopping the server" {
		t.Fatalf("stop = %q, %v", out, err)
	}
}
