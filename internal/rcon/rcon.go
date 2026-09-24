// Package rcon implements a minimal Source RCON client (spec §5, §7).
//
// The operator uses it for two purposes:
//   - Readiness probing: a successful Dial (TCP connect + auth) is the
//     loader-agnostic "RCON 探通" gate. A status ping is never sufficient.
//   - Graceful shutdown: Execute("save-all flush") right before the operator
//     scales a server to zero.
//
// Multi-packet responses (a single command whose reply exceeds one ~4 KiB
// packet) are not reassembled; Phase-1 commands ("list", "save-all", "stop")
// always fit in one packet. This is intentional and documented rather than
// silently truncating large replies.
package rcon

import (
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
)

// ErrAuthFailed is returned by Dial when the RCON password is rejected.
var ErrAuthFailed = errors.New("rcon: authentication failed")

// DefaultPort is the conventional RCON port.
const DefaultPort = 25575

// Conn is an authenticated RCON connection. It is not safe for concurrent use.
type Conn struct {
	conn  net.Conn
	reqID int32
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
func (c *Conn) SetDeadline(t time.Time) error { return c.conn.SetDeadline(t) }

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.conn.Close() }

// Execute runs a single command and returns the server's reply body.
func (c *Conn) Execute(cmd string) (string, error) {
	id := c.nextID()
	if err := writePacket(c.conn, id, typeExecCommand, cmd); err != nil {
		return "", err
	}
	respID, _, body, err := readPacket(c.conn)
	if err != nil {
		return "", err
	}
	if respID != id {
		return "", fmt.Errorf("rcon: response id mismatch: got %d want %d", respID, id)
	}
	return body, nil
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
	bodyBytes := []byte(body)
	length := int32(4 + 4 + len(bodyBytes) + 2)
	if length > maxPacketLen {
		return fmt.Errorf("rcon: outgoing packet too large: %d bytes", length)
	}
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
func readPacket(r io.Reader) (id, typ int32, body string, err error) {
	var lenBuf [4]byte
	if _, err = io.ReadFull(r, lenBuf[:]); err != nil {
		return 0, 0, "", err
	}
	length := int32(binary.LittleEndian.Uint32(lenBuf[:]))
	if length < minPacketLen || length > maxPacketLen {
		return 0, 0, "", fmt.Errorf("rcon: invalid packet length %d", length)
	}
	payload := make([]byte, length)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, 0, "", err
	}
	id = int32(binary.LittleEndian.Uint32(payload[0:4]))
	typ = int32(binary.LittleEndian.Uint32(payload[4:8]))
	// Strip the two trailing null bytes from the body.
	body = string(payload[8 : length-2])
	return id, typ, body, nil
}

func appendInt32(buf []byte, v int32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	return append(buf, b[:]...)
}
