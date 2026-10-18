package submit

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
)

var ErrDigestMismatch = errors.New("submit: the bytes that arrived do not match the digest they were sent with")

// VerifyDigest substitutes ErrDigestMismatch for EOF unless the full stream matches want; downstream stores roll back failed reads.
func VerifyDigest(r io.Reader, want []byte) io.Reader {
	return &digestReader{r: r, h: sha256.New(), want: want}
}

type digestReader struct {
	r    io.Reader
	h    hash.Hash
	want []byte
}

func (d *digestReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	d.h.Write(p[:n])
	if err == io.EOF {
		if got := d.h.Sum(nil); !bytes.Equal(got, d.want) {
			return n, fmt.Errorf("%w: they hash to %x, sent as %x", ErrDigestMismatch, got, d.want)
		}
	}
	return n, err
}
