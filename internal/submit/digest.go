package submit

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
)

// ErrDigestMismatch reports bytes that do not hash to the SHA-256 their sender
// computed over them (the request's Content-Digest): they changed on the way.
// The API answers 400 digest_mismatch, and the client sends them again.
var ErrDigestMismatch = errors.New("submit: the bytes that arrived do not match the digest they were sent with")

// VerifyDigest passes r through and, where it ends, answers ErrDigestMismatch in
// place of io.EOF unless what went by hashes to want, a SHA-256. The stores
// treat that like any read that breaks off: UploadPart cuts the part back off,
// and UploadContext never lets the blob replace the one before it. So bytes
// changed on the way are never kept.
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
