package software

import (
	"crypto/md5"  //nolint:gosec // Purpur publishes md5 only; see Digest
	"crypto/sha1" //nolint:gosec // Mojang and Maven publish sha1; see Digest
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
)

// Digest names a file by the hash its upstream publishes for it.
//
// The algorithm is whatever that upstream offers: sha256 where it exists, sha1
// for Mojang's manifests and Maven repositories that sign nothing stronger, md5
// for Purpur, the one source with nothing else. A weak algorithm still pins the
// exact file the source published over TLS: replacing it needs a second preimage,
// which md5 and sha1 still resist; their known collision attacks need both files
// chosen by the attacker. The artifact cache files everything under its sha256
// and only consults the weaker index for a request naming one.
type Digest struct {
	Algo string `json:"algo"`
	Hex  string `json:"hex"`
}

// Digest algorithms.
const (
	SHA1   = "sha1"
	SHA256 = "sha256"
	SHA512 = "sha512"
	MD5    = "md5"
)

var digestLen = map[string]int{SHA1: 40, SHA256: 64, SHA512: 128, MD5: 32}

// ParseDigest validates an algorithm and lowercase hex value.
func ParseDigest(algo, hexValue string) (Digest, error) {
	n, ok := digestLen[algo]
	if !ok {
		return Digest{}, fmt.Errorf("software: unknown digest algorithm %q", algo)
	}
	v := strings.ToLower(strings.TrimSpace(hexValue))
	if len(v) != n {
		return Digest{}, fmt.Errorf("software: %s digest must be %d hex characters", algo, n)
	}
	if _, err := hex.DecodeString(v); err != nil {
		return Digest{}, fmt.Errorf("software: %s digest is not hex", algo)
	}
	return Digest{Algo: algo, Hex: v}, nil
}

// Valid reports whether d parses.
func (d Digest) Valid() bool {
	_, err := ParseDigest(d.Algo, d.Hex)
	return err == nil
}

func (d Digest) String() string { return d.Algo + ":" + d.Hex }

// NewHash returns a hash for algo.
func NewHash(algo string) (hash.Hash, error) {
	switch algo {
	case SHA1:
		return sha1.New(), nil //nolint:gosec // see Digest
	case SHA256:
		return sha256.New(), nil
	case SHA512:
		return sha512.New(), nil
	case MD5:
		return md5.New(), nil //nolint:gosec // see Digest
	}
	return nil, fmt.Errorf("software: unknown digest algorithm %q", algo)
}

// ErrDigestMismatch is returned when content does not hash to its digest.
var ErrDigestMismatch = errors.New("software: content does not match its published digest")

// FileMatches reports whether the file at p hashes to d. A missing file is
// (false, nil).
func FileMatches(p string, d Digest) (bool, error) {
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	h, err := NewHash(d.Algo)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == d.Hex, nil
}
