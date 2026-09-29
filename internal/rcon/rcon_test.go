package rcon_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
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
	// hang leaves every command unanswered; doubleEnd answers the end marker
	// twice, as a Source-engine server does.
	hang, doubleEnd bool
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
			if f.hang {
				continue
			}
			// Minecraft drops a connection whose read holds more than the one
			// packet: a client that pipelines its next packet is cut off.
			if pipelined(conn) {
				return
			}
			// Minecraft splits a reply into 4096-byte bodies, even mid-rune.
			reply := f.replies[body]
			for len(reply) > 4096 {
				_ = writeFramePacket(conn, id, 0, reply[:4096])
				reply = reply[4096:]
			}
			_ = writeFramePacket(conn, id, 0, reply)
		default:
			if f.hang {
				continue
			}
			_ = writeFramePacket(conn, id, 0, "Unknown request 0")
			if f.doubleEnd {
				_ = writeFramePacket(conn, id, 0, "")
			}
		}
	}
}

// pipelined reports whether the client already sent more bytes behind the
// packet just read.
func pipelined(conn net.Conn) bool {
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	defer conn.SetReadDeadline(time.Time{})
	var b [1]byte
	n, _ := conn.Read(b[:])
	return n > 0
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

// A command goes out in one packet of at most 4096 bytes; a longer one is
// refused before anything is sent.
func TestExecuteRefusesAnOversizeCommand(t *testing.T) {
	longest := strings.Repeat("a", 4096-10) // 10: id, type, two terminators
	f := startFakeRCON(t, "s3cret", map[string]string{longest: "ok"})
	defer f.stop()

	c, err := rcon.Dial(f.addr(), "s3cret", 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if got, err := c.Execute(longest); err != nil || got != "ok" {
		t.Fatalf("Execute(%d bytes) = %q, %v; want ok", len(longest), got, err)
	}
	if _, err := c.Execute(longest + "a"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("Execute(%d bytes) err = %v, want the too-large refusal", len(longest)+1, err)
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
	// A save followed by a second command on the same connection: the reply
	// ids must line up across consecutive Executes.
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

func TestExecuteReassemblesALongReply(t *testing.T) {
	// 3000 three-byte runes: 9000 bytes over three packets, split mid-rune.
	long := strings.Repeat("封", 3000)
	f := startFakeRCON(t, "pw", map[string]string{"banlist": long})
	defer f.stop()
	c, err := rcon.Dial(f.addr(), "pw", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	got, err := c.Execute("banlist")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(got) != 9000 || got != long {
		t.Fatalf("reply = %d bytes, want the 9000-byte original", len(got))
	}
}

func TestExecuteSkipsALeftoverEndMarker(t *testing.T) {
	f := startFakeRCON(t, "pw", map[string]string{"list": "There are 0 of a max of 20 players online", "seed": "Seed: [42]"})
	f.doubleEnd = true
	defer f.stop()
	c, err := rcon.Dial(f.addr(), "pw", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if got, err := c.Execute("list"); err != nil || got != "There are 0 of a max of 20 players online" {
		t.Fatalf("first = %q, %v", got, err)
	}
	if got, err := c.Execute("seed"); err != nil || got != "Seed: [42]" {
		t.Fatalf("second = %q, %v", got, err)
	}
}

func TestExecuteContextGivesUpOnAHungServer(t *testing.T) {
	f := startFakeRCON(t, "pw", nil)
	f.hang = true
	defer f.stop()
	c, err := rcon.Dial(f.addr(), "pw", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.ExecuteContext(ctx, "banlist"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: err = %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("deadline: took %v", took)
	}

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start = time.Now()
	if _, err := c.ExecuteContext(ctx, "banlist"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("cancel: took %v", took)
	}

	// A caller's own SetDeadline bounds a context-free Execute.
	if err := c.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	var ne net.Error
	if _, err := c.Execute("banlist"); !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("set deadline: err = %v, want a timeout", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("set deadline: took %v", took)
	}
}
