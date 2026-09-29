// Package rcon implements a minimal Source RCON client (spec §5, §7).
//
// The operator uses it for two purposes:
//   - Readiness probing: a successful Dial (TCP connect + auth) is the
//     loader-agnostic "RCON 探通" gate. A status ping is never sufficient.
//   - Graceful shutdown: Execute("save-all flush") right before the operator
//     scales a server to zero.
//
// A reply longer than one packet (Minecraft splits a command's output into
// 4096-byte bodies: banlist, whitelist list, any console command) is
// reassembled: once the reply begins, Execute sends an empty RESPONSE_VALUE
// packet, which the server answers only after the whole reply ("Unknown
// request 0" on vanilla and Paper), so that answer marks the end.
package rcon

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Source RCON packet types.
const (
	typeResponseValue = 0 // SERVERDATA_RESPONSE_VALUE
	typeExecCommand   = 2 // SERVERDATA_EXECCOMMAND
	typeAuthResponse  = 2 // SERVERDATA_AUTH_RESPONSE (same id as EXECCOMMAND)
	typeAuth          = 3 // SERVERDATA_AUTH
)

// authFailedID is the request id a server returns when auth fails.
const authFailedID int32 = -1

// Packet length bounds. minPacketLen is 4 (id) + 4 (type) + 2 (terminators).
const (
	minPacketLen = 10
	maxPacketLen = 4096
	// maxReplyPacketLen admits a full 4096-byte Minecraft reply body plus the
	// id, type and two terminators.
	maxReplyPacketLen = maxPacketLen + minPacketLen
	// maxReplyBytes bounds one reassembled reply, so a server that never sends
	// the end marker cannot grow it without limit.
	maxReplyBytes = 1 << 20
)

// DefaultCommandTimeout bounds a command when neither the caller's context nor
// SetDeadline gives one, so a hung server cannot hold a console request open.
const DefaultCommandTimeout = 10 * time.Second

// ErrAuthFailed is returned by Dial when the RCON password is rejected.
var ErrAuthFailed = errors.New("rcon: authentication failed")

// DefaultPort is the conventional RCON port.
const DefaultPort = 25575

// Conn is an authenticated RCON connection. It is not safe for concurrent use.
type Conn struct {
	conn  net.Conn
	reqID int32
	// deadline is the caller's SetDeadline, which a command honours in place
	// of DefaultCommandTimeout.
	deadline time.Time
}

// Dial opens a TCP connection to addr and authenticates with password. The
// timeout, if > 0, bounds the whole connect+auth handshake; it is cleared on
// the returned Conn so subsequent calls block unless SetDeadline is used.
func Dial(addr, password string, timeout time.Duration) (*Conn, error) {
	var (
		netConn net.Conn
		err     error
	)
	if timeout > 0 {
		netConn, err = net.DialTimeout("tcp", addr, timeout)
	} else {
		netConn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		return nil, err
	}

	c := &Conn{conn: netConn}
	if timeout > 0 {
		if err := netConn.SetDeadline(time.Now().Add(timeout)); err != nil {
			netConn.Close()
			return nil, err
		}
	}
	if err := c.auth(password); err != nil {
		netConn.Close()
		return nil, err
	}
	// Clear the handshake deadline so the connection is reusable.
	if err := netConn.SetDeadline(time.Time{}); err != nil {
		netConn.Close()
		return nil, err
	}
	return c, nil
}

// SetDeadline sets an absolute deadline for subsequent Execute calls.
func (c *Conn) SetDeadline(t time.Time) error {
	c.deadline = t
	return c.conn.SetDeadline(t)
}

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.conn.Close() }

// Execute runs a single command and returns the server's whole reply.
func (c *Conn) Execute(cmd string) (string, error) {
	return c.ExecuteContext(context.Background(), cmd)
}

// ExecuteContext runs a single command and returns the server's whole reply,
// reassembled across packets. It gives up when ctx is done (cancelled or past
// its deadline), and otherwise at the SetDeadline deadline, or after
// DefaultCommandTimeout when none was set.
func (c *Conn) ExecuteContext(ctx context.Context, cmd string) (string, error) {
	dl := c.deadline
	if dl.IsZero() {
		dl = time.Now().Add(DefaultCommandTimeout)
	}
	if err := c.conn.SetDeadline(dl); err != nil {
		return "", err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	id, end := c.nextID(), c.nextID()
	if err := writePacket(c.conn, id, typeExecCommand, cmd); err != nil {
		return "", c.ctxErr(ctx, err)
	}
	var reply []byte
	sentEnd := false
	for {
		respID, _, body, err := readPacket(c.conn)
		if err != nil {
			return "", c.ctxErr(ctx, err)
		}
		switch {
		case respID == id:
			if len(reply)+len(body) > maxReplyBytes {
				return "", fmt.Errorf("rcon: reply exceeds %d bytes", maxReplyBytes)
			}
			reply = append(reply, body...)
			// The end marker goes out only once the reply has begun: Minecraft
			// drops a connection whose read holds more than one packet, and it
			// sends every fragment before it reads again, so the marker's answer
			// follows the last one.
			if !sentEnd {
				if err := writePacket(c.conn, end, typeResponseValue, ""); err != nil {
					return "", c.ctxErr(ctx, err)
				}
				sentEnd = true
			}
		case respID == end:
			return string(reply), nil
		default:
			// A leftover from an earlier exchange (a server that answers the end
			// marker twice); it belongs to no live request.
		}
	}
}

// ctxErr prefers ctx's error when a cancellation is what cut the exchange.
func (c *Conn) ctxErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// auth performs the SERVERDATA_AUTH handshake. Some servers emit an empty
// RESPONSE_VALUE before the auth response, so non-auth packets are skipped.
func (c *Conn) auth(password string) error {
	id := c.nextID()
	if err := writePacket(c.conn, id, typeAuth, password); err != nil {
		return err
	}
	for {
		respID, respType, _, err := readPacket(c.conn)
		if err != nil {
			return err
		}
		if respType != typeAuthResponse {
			// Empty RESPONSE_VALUE echo; keep reading for the real answer.
			continue
		}
		if respID == authFailedID {
			return ErrAuthFailed
		}
		if respID != id {
			return fmt.Errorf("rcon: auth id mismatch: got %d want %d", respID, id)
		}
		return nil
	}
}

func (c *Conn) nextID() int32 {
	c.reqID++
	if c.reqID < 0 {
		c.reqID = 1
	}
	return c.reqID
}

// writePacket encodes one RCON packet: little-endian length, id, type, the
// null-terminated body, and a trailing null byte.
func writePacket(w io.Writer, id, typ int32, body string) error {
	// Bounded on the body, before the int32 conversion: one past 2 GiB would
	// wrap the length negative and slip under a check made after it.
	if len(body) > maxPacketLen-minPacketLen {
		return fmt.Errorf("rcon: outgoing packet too large: %d bytes", len(body)+minPacketLen)
	}
	bodyBytes := []byte(body)
	length := int32(4 + 4 + len(bodyBytes) + 2)
	buf := make([]byte, 0, 4+length)
	buf = appendInt32(buf, length)
	buf = appendInt32(buf, id)
	buf = appendInt32(buf, typ)
	buf = append(buf, bodyBytes...)
	buf = append(buf, 0, 0)
	_, err := w.Write(buf)
	return err
}

// readPacket decodes one RCON packet.
func readPacket(r io.Reader) (id, typ int32, body []byte, err error) {
	var lenBuf [4]byte
	if _, err = io.ReadFull(r, lenBuf[:]); err != nil {
		return 0, 0, nil, err
	}
	length := int32(binary.LittleEndian.Uint32(lenBuf[:]))
	if length < minPacketLen || length > maxReplyPacketLen {
		return 0, 0, nil, fmt.Errorf("rcon: invalid packet length %d", length)
	}
	payload := make([]byte, length)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, 0, nil, err
	}
	id = int32(binary.LittleEndian.Uint32(payload[0:4]))
	typ = int32(binary.LittleEndian.Uint32(payload[4:8]))
	// Strip the two trailing null bytes from the body.
	body = payload[8 : length-2]
	return id, typ, body, nil
}

func appendInt32(buf []byte, v int32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	return append(buf, b[:]...)
}
