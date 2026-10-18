// Verify signed forwarding preserves the supplied UUID; unsupported servers instead use offline name UUIDs.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // offline-mode UUIDs are md5 by definition
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"felis.lolicon.best/internal/rcon"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:25565", "server address")
	secret := flag.String("secret", "", "Velocity forwarding secret")
	name := flag.String("name", "FelisCheck", "player name")
	id := flag.String("uuid", "5b8d2f3a-7c41-4e2a-9f0d-1e6c3b7a9d42", "player UUID to forward")
	timeout := flag.Duration("timeout", 30*time.Second, "overall timeout")
	rconAddr := flag.String("rcon", "", "also run `list` over RCON at this address (the operator's readiness probe)")
	rconPassword := flag.String("rcon-password", "", "RCON password")
	flag.Parse()
	res, err := check(*addr, *secret, *name, *id, *timeout)
	if err == nil && *rconAddr != "" {
		res.RCON, err = rconList(*rconAddr, *rconPassword)
	}
	out, _ := json.Marshal(res)
	fmt.Println(string(out))
	if err != nil {
		fmt.Fprintln(os.Stderr, "forwardcheck:", err)
		os.Exit(1)
	}
}

// Result is what the server did with the login.
type Result struct {
	Protocol  int    `json:"protocol"`
	Version   string `json:"version"`
	Forwarded bool   `json:"forwarded"`
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	// Identity is "forwarded" when the server accepted the forwarded UUID,
	// "offline" when it used the offline-mode UUID of the name, else "other".
	Identity string `json:"identity"`
	// RCON is the reply to `list`, when -rcon is given.
	RCON string `json:"rcon,omitempty"`
}

func rconList(addr, password string) (string, error) {
	c, err := rcon.Dial(addr, password, 10*time.Second)
	if err != nil {
		return "", fmt.Errorf("rcon: %w", err)
	}
	defer c.Close()
	out, err := c.Execute("list")
	if err != nil {
		return "", fmt.Errorf("rcon list: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func check(addr, secret, name, id string, timeout time.Duration) (Result, error) {
	var res Result
	deadline := time.Now().Add(timeout)
	proto, version, err := statusProtocol(addr, deadline)
	if err != nil {
		return res, fmt.Errorf("status ping: %w", err)
	}
	res.Protocol, res.Version = proto, version
	uuid, err := parseUUID(id)
	if err != nil {
		return res, err
	}

	conn, err := net.DialTimeout("tcp", addr, time.Until(deadline))
	if err != nil {
		return res, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	c := &mcConn{rw: conn, threshold: -1}

	host, port, _ := net.SplitHostPort(addr)
	var hs bytes.Buffer
	writeVarInt(&hs, proto)
	writeString(&hs, host)
	_ = binary.Write(&hs, binary.BigEndian, uint16(atoiPort(port)))
	writeVarInt(&hs, 2)
	if err := c.send(0x00, hs.Bytes()); err != nil {
		return res, err
	}
	var ls bytes.Buffer
	writeString(&ls, name)
	switch {
	case proto >= 764: // 1.20.2+: name, UUID
		ls.Write(uuid[:])
	case proto >= 761: // 1.19.3-1.20.1: name, has UUID, UUID
		ls.WriteByte(1)
		ls.Write(uuid[:])
	case proto == 760: // 1.19.1-1.19.2: name, no signature data, has UUID, UUID
		ls.WriteByte(0)
		ls.WriteByte(1)
		ls.Write(uuid[:])
	case proto == 759: // 1.19: name, no signature data
		ls.WriteByte(0)
	}
	if err := c.send(0x00, ls.Bytes()); err != nil {
		return res, err
	}

	for {
		pid, body, err := c.recv()
		if err != nil {
			return res, fmt.Errorf("login: %w", err)
		}
		r := bytes.NewReader(body)
		switch pid {
		case 0x00: // disconnect
			reason, _ := readString(r)
			return res, fmt.Errorf("disconnected: %s", reason)
		case 0x01:
			return res, errors.New("the server asked for encryption: it runs online mode behind the proxy")
		case 0x03: // set compression
			t, err := readVarInt(r)
			if err != nil {
				return res, err
			}
			c.threshold = t
		case 0x04: // login plugin request
			msgID, err := readVarInt(r)
			if err != nil {
				return res, err
			}
			channel, err := readString(r)
			if err != nil {
				return res, err
			}
			var reply bytes.Buffer
			writeVarInt(&reply, msgID)
			if channel != "velocity:player_info" || secret == "" {
				reply.WriteByte(0) // not understood
			} else {
				res.Forwarded = true
				reply.WriteByte(1)
				reply.Write(forwardingData(secret, uuid, name))
			}
			if err := c.send(0x02, reply.Bytes()); err != nil {
				return res, err
			}
		case 0x02: // login success
			if proto >= 735 { // 1.16+: binary UUID
				var got [16]byte
				if _, err := io.ReadFull(r, got[:]); err != nil {
					return res, err
				}
				res.UUID = formatUUID(got)
			} else {
				s, err := readString(r)
				if err != nil {
					return res, err
				}
				res.UUID = strings.ToLower(s)
			}
			res.Name, _ = readString(r)
			switch res.UUID {
			case formatUUID(uuid):
				res.Identity = "forwarded"
			case formatUUID(offlineUUID(name)):
				res.Identity = "offline"
			default:
				res.Identity = "other"
			}
			return res, nil
		default:
			return res, fmt.Errorf("unexpected login packet 0x%02x", pid)
		}
	}
}

// forwardingData is Velocity's MODERN_DEFAULT (version 1) player info, signed.
func forwardingData(secret string, uuid [16]byte, name string) []byte {
	var data bytes.Buffer
	writeVarInt(&data, 1)
	writeString(&data, "127.0.0.1")
	data.Write(uuid[:])
	writeString(&data, name)
	writeVarInt(&data, 0) // no profile properties
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data.Bytes())
	return append(mac.Sum(nil), data.Bytes()...)
}

// offlineUUID is the UUID a server in offline mode gives a name: a version 3
// UUID of "OfflinePlayer:<name>".
func offlineUUID(name string) [16]byte {
	sum := md5.Sum([]byte("OfflinePlayer:" + name)) //nolint:gosec // see import
	sum[6] = sum[6]&0x0f | 0x30
	sum[8] = sum[8]&0x3f | 0x80
	return sum
}

func statusProtocol(addr string, deadline time.Time) (int, string, error) {
	conn, err := net.DialTimeout("tcp", addr, time.Until(deadline))
	if err != nil {
		return 0, "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	c := &mcConn{rw: conn, threshold: -1}
	host, port, _ := net.SplitHostPort(addr)
	var hs bytes.Buffer
	writeVarInt(&hs, -1)
	writeString(&hs, host)
	_ = binary.Write(&hs, binary.BigEndian, uint16(atoiPort(port)))
	writeVarInt(&hs, 1)
	if err := c.send(0x00, hs.Bytes()); err != nil {
		return 0, "", err
	}
	if err := c.send(0x00, nil); err != nil {
		return 0, "", err
	}
	_, body, err := c.recv()
	if err != nil {
		return 0, "", err
	}
	s, err := readString(bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	var st struct {
		Version struct {
			Name     string `json:"name"`
			Protocol int    `json:"protocol"`
		} `json:"version"`
	}
	if err := json.Unmarshal([]byte(s), &st); err != nil {
		return 0, "", err
	}
	return st.Version.Protocol, st.Version.Name, nil
}

type mcConn struct {
	rw        io.ReadWriter
	threshold int
}

func (c *mcConn) send(id int, payload []byte) error {
	var pkt bytes.Buffer
	writeVarInt(&pkt, id)
	pkt.Write(payload)
	body := pkt.Bytes()
	var frame bytes.Buffer
	if c.threshold >= 0 {
		var inner bytes.Buffer
		if len(body) >= c.threshold {
			var z bytes.Buffer
			zw := zlib.NewWriter(&z)
			_, _ = zw.Write(body)
			_ = zw.Close()
			writeVarInt(&inner, len(body))
			inner.Write(z.Bytes())
		} else {
			writeVarInt(&inner, 0)
			inner.Write(body)
		}
		body = inner.Bytes()
	}
	writeVarInt(&frame, len(body))
	frame.Write(body)
	_, err := c.rw.Write(frame.Bytes())
	return err
}

func (c *mcConn) recv() (int, []byte, error) {
	br := byteReader{c.rw}
	n, err := readVarInt(br)
	if err != nil {
		return 0, nil, err
	}
	if n < 0 || n > 1<<21 {
		return 0, nil, fmt.Errorf("bad packet length %d", n)
	}
	frame := make([]byte, n)
	if _, err := io.ReadFull(c.rw, frame); err != nil {
		return 0, nil, err
	}
	r := bytes.NewReader(frame)
	if c.threshold >= 0 {
		dataLen, err := readVarInt(r)
		if err != nil {
			return 0, nil, err
		}
		if dataLen > 0 {
			zr, err := zlib.NewReader(r)
			if err != nil {
				return 0, nil, err
			}
			plain, err := io.ReadAll(io.LimitReader(zr, 1<<23))
			if err != nil {
				return 0, nil, err
			}
			r = bytes.NewReader(plain)
		}
	}
	id, err := readVarInt(r)
	if err != nil {
		return 0, nil, err
	}
	rest, _ := io.ReadAll(r)
	return id, rest, nil
}

type byteReader struct{ r io.Reader }

func (b byteReader) ReadByte() (byte, error) {
	var one [1]byte
	_, err := io.ReadFull(b.r, one[:])
	return one[0], err
}

func writeVarInt(w *bytes.Buffer, v int) {
	u := uint32(v)
	for {
		if u&^0x7f == 0 {
			w.WriteByte(byte(u))
			return
		}
		w.WriteByte(byte(u&0x7f | 0x80))
		u >>= 7
	}
}

func readVarInt(r io.ByteReader) (int, error) {
	var v uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint32(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return int(int32(v)), nil
		}
	}
	return 0, errors.New("varint too long")
}

func writeString(w *bytes.Buffer, s string) {
	writeVarInt(w, len(s))
	w.WriteString(s)
}

func readString(r *bytes.Reader) (string, error) {
	n, err := readVarInt(r)
	if err != nil {
		return "", err
	}
	if n < 0 || n > r.Len() {
		return "", errors.New("bad string length")
	}
	b := make([]byte, n)
	_, err = io.ReadFull(r, b)
	return string(b), err
}

func parseUUID(s string) ([16]byte, error) {
	var out [16]byte
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != 16 {
		return out, fmt.Errorf("bad uuid %q", s)
	}
	copy(out[:], b)
	return out, nil
}

func formatUUID(u [16]byte) string {
	h := hex.EncodeToString(u[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func atoiPort(p string) int {
	n := 0
	for _, r := range p {
		if r < '0' || r > '9' {
			return 25565
		}
		n = n*10 + int(r-'0')
	}
	return n
}
