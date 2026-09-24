package offsite

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// An object in the bucket is its file encrypted with AES-256-GCM in fixed
// segments, so a multi-gigabyte world streams through in constant memory:
//
//	magic "FELISOS1" | 7-byte random nonce prefix | segment 0 | segment 1 | ...
//
// Segment i seals up to segmentSize bytes under the nonce
// prefix || uint32(i) || last, where last is 1 on the final segment only. The
// counter stops segments being reordered or dropped, and the last flag stops
// the object being cut short at a segment boundary: either change fails to
// authenticate.
const (
	magic       = "FELISOS1"
	prefixSize  = 7
	segmentSize = 64 << 10
	headerSize  = len(magic) + prefixSize
	// KeySize is the key length: AES-256.
	KeySize = 32
)

// ErrAuth is a segment that does not authenticate: the wrong key, or an object
// damaged in the bucket or on the way.
var ErrAuth = errors.New("offsite: object does not decrypt with this key (wrong key, or the object is damaged)")

// NewKey returns a fresh random key in the text form ParseKey reads.
func NewKey() (string, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k), nil
}

// ParseKey decodes a key written by NewKey (standard base64 of 32 bytes).
func ParseKey(s string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(k) != KeySize {
		return nil, fmt.Errorf("offsite: the key must be %d random bytes in base64 (generate one with `felis offsite keygen`)", KeySize)
	}
	return k, nil
}

// KeyID names a key without revealing it, so status output and logs can say
// which key a bucket's objects were written with.
func KeyID(key []byte) string {
	h := sha256.Sum256(append([]byte("felis-offsite-key-id\x00"), key...))
	return hex.EncodeToString(h[:8])
}

// SealedSize is the size of the object Encrypt makes from n plaintext bytes.
// Uploads need it up front: an S3 upload of unknown length buffers far more.
func SealedSize(n int64) int64 {
	segs := (n + segmentSize - 1) / segmentSize
	if segs == 0 {
		segs = 1
	}
	return int64(headerSize) + n + segs*16
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("offsite: key is %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func segmentNonce(prefix []byte, i uint32, last bool) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint32(n[prefixSize:], i)
	if last {
		n[11] = 1
	}
	return n
}

// Encrypt writes src to dst in the segmented format.
func Encrypt(dst io.Writer, src io.Reader, key []byte) error {
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	prefix := make([]byte, prefixSize)
	if _, err := rand.Read(prefix); err != nil {
		return err
	}
	if _, err := io.WriteString(dst, magic); err != nil {
		return err
	}
	if _, err := dst.Write(prefix); err != nil {
		return err
	}
	in := bufio.NewReaderSize(src, segmentSize+1)
	buf := make([]byte, segmentSize)
	out := make([]byte, 0, segmentSize+aead.Overhead())
	for i := uint32(0); ; i++ {
		n, err := io.ReadFull(in, buf)
		switch {
		case err == io.EOF || err == io.ErrUnexpectedEOF:
			err = nil
		case err != nil:
			return err
		}
		last := n < segmentSize
		if !last {
			if _, perr := in.Peek(1); perr == io.EOF {
				last = true
			} else if perr != nil {
				return perr
			}
		}
		out = aead.Seal(out[:0], segmentNonce(prefix, i, last), buf[:n], []byte(magic))
		if _, err := dst.Write(out); err != nil {
			return err
		}
		if last {
			return nil
		}
		if i == ^uint32(0) {
			return errors.New("offsite: file too large to encrypt")
		}
	}
}

// Decrypt reverses Encrypt. Nothing unauthenticated is written: each segment
// is checked before its plaintext reaches dst, and a missing tail is an error.
// A caller writing to a file still has to discard it on error, since earlier
// segments were already written.
func Decrypt(dst io.Writer, src io.Reader, key []byte) error {
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	hdr := make([]byte, headerSize)
	if _, err := io.ReadFull(src, hdr); err != nil {
		return fmt.Errorf("offsite: object too short for its header: %w", err)
	}
	if string(hdr[:len(magic)]) != magic {
		return errors.New("offsite: not a Felis off-site object (bad magic)")
	}
	prefix := hdr[len(magic):]
	sealed := segmentSize + aead.Overhead()
	in := bufio.NewReaderSize(src, sealed+1)
	buf := make([]byte, sealed)
	var plain []byte
	for i := uint32(0); ; i++ {
		n, err := io.ReadFull(in, buf)
		switch {
		case err == io.EOF:
			return fmt.Errorf("offsite: object is cut short after %d segments", i)
		case err == io.ErrUnexpectedEOF:
			err = nil
		case err != nil:
			return err
		}
		last := n < sealed
		if !last {
			if _, perr := in.Peek(1); perr == io.EOF {
				last = true
			} else if perr != nil {
				return perr
			}
		}
		plain, err = aead.Open(plain[:0], segmentNonce(prefix, i, last), buf[:n], []byte(magic))
		if err != nil {
			return ErrAuth
		}
		if _, err := dst.Write(plain); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}
