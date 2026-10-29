// Package rcon implements authenticated Source RCON with multi-packet reply reassembly.
package rcon

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	typeResponseValue = 0 // SERVERDATA_RESPONSE_VALUE
	typeExecCommand   = 2 // SERVERDATA_EXECCOMMAND
	typeAuthResponse  = 2 // SERVERDATA_AUTH_RESPONSE (same id as EXECCOMMAND)
	typeAuth          = 3 // SERVERDATA_AUTH
)

const authFailedID int32 = -1

// minPacketLen includes the id, type, and two null terminators.
const (
	minPacketLen = 10
	maxPacketLen = 4096
	// maxReplyPacketLen includes a 4096-byte reply body and framing.
	maxReplyPacketLen = maxPacketLen + minPacketLen
	// maxReplyBytes bounds replies from servers that omit the end marker.
	maxReplyBytes = 1 << 20
)

// DefaultCommandTimeout applies when neither context nor SetDeadline provides a deadline.
const DefaultCommandTimeout = 10 * time.Second

var ErrAuthFailed = errors.New("rcon: authentication failed")

const DefaultPort = 25575

// Conn is an authenticated RCON connection. It is not safe for concurrent use.
type Conn struct {
	conn     net.Conn
	r        *bufio.Reader
	reqID    int32
	deadline time.Time
}

// readyWait is how long Ready listens for the peer having closed the connection.
const readyWait = 2 * time.Millisecond

// Dial bounds TCP connect and authentication with timeout, then clears the handshake deadline.
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

	c := &Conn{conn: netConn, r: bufio.NewReader(netConn)}
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
	if err := netConn.SetDeadline(time.Time{}); err != nil {
		netConn.Close()
		return nil, err
	}
	return c, nil
}

// DialContext applies ctx and timeout to dial plus auth, then detaches ctx.
func DialContext(ctx context.Context, addr, password string, timeout time.Duration) (*Conn, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	netConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &Conn{conn: netConn, r: bufio.NewReader(netConn)}
	stop := context.AfterFunc(ctx, func() { _ = netConn.Close() })
	err = c.auth(password)
	stopped := stop()
	if !stopped || ctx.Err() != nil {
		netConn.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		netConn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) SetDeadline(t time.Time) error {
	c.deadline = t
	return c.conn.SetDeadline(t)
}

func (c *Conn) Close() error { return c.conn.Close() }

// Ready reports whether a kept connection can take another command, and clears the deadline its last user set.
func (c *Conn) Ready() bool {
	if err := c.conn.SetReadDeadline(time.Now().Add(readyWait)); err != nil {
		return false
	}
	_, err := c.r.Peek(1)
	var netErr net.Error
	alive := err == nil || errors.As(err, &netErr) && netErr.Timeout()
	c.deadline = time.Time{}
	return alive && c.conn.SetDeadline(time.Time{}) == nil
}

func (c *Conn) Execute(cmd string) (string, error) {
	return c.ExecuteContext(context.Background(), cmd)
}

// ExecuteContext reassembles the reply until cancellation, SetDeadline, or DefaultCommandTimeout.
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
		respID, _, body, err := readPacket(c.r)
		if err != nil {
			return "", c.ctxErr(ctx, err)
		}
		switch {
		case respID == id:
			if len(reply)+len(body) > maxReplyBytes {
				return "", fmt.Errorf("rcon: reply exceeds %d bytes", maxReplyBytes)
			}
			reply = append(reply, body...)
			// Send the marker only after the reply starts; Minecraft may drop connections receiving multiple packets together.
			if !sentEnd {
				if err := writePacket(c.conn, end, typeResponseValue, ""); err != nil {
					return "", c.ctxErr(ctx, err)
				}
				sentEnd = true
			}
		case respID == end:
			return string(reply), nil
		default:
			// Ignore duplicate end-marker responses from earlier requests.
		}
	}
}

func (c *Conn) ctxErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// Skip empty RESPONSE_VALUE packets emitted before the authentication response.
func (c *Conn) auth(password string) error {
	id := c.nextID()
	if err := writePacket(c.conn, id, typeAuth, password); err != nil {
		return err
	}
	for {
		respID, respType, _, err := readPacket(c.r)
		if err != nil {
			return err
		}
		if respType != typeAuthResponse {
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

func writePacket(w io.Writer, id, typ int32, body string) error {
	// Check body length before int32 conversion to prevent overflow bypass.
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
	body = payload[8 : length-2]
	return id, typ, body, nil
}

func appendInt32(buf []byte, v int32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	return append(buf, b[:]...)
}
