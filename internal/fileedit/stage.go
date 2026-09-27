package fileedit

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sync"
	"syscall"
)

// Stage holds uploads between the request that brought them and the Job that
// lands them. The bytes cannot ride the Job spec the way an edit does (etcd caps
// an object near 1.5 MiB, and execve one environment string at 128 KiB), and
// felis-api cannot mount the world volume, so felis-api keeps the upload on its
// own disk and serves it once, on its internal face, to the Job it created for it
// (cmd/felis files, fetchUpload).
//
// Each staged upload is opened by an unguessable id in the URL plus a token the
// Job carries in its environment. Only a digest of the token is kept, compared in
// constant time, and the first successful Open spends it: the Job never retries,
// so a second Open could only be someone else. The release func Put returns
// deletes the file once the Job has answered, whatever it answered.
//
// Nothing here outlives the process: the index is in memory, so Sweep empties
// Dir at startup of whatever a previous process left behind.
type Stage struct {
	// Dir holds the staged files. cmd/felis puts it on the uploads volume, which
	// has a real capacity; the pod's /tmp is the node's own disk.
	Dir string
	// MinFree is the share of Dir's filesystem an upload must leave free
	// (DefaultStageMinFree when zero), so a burst of uploads cannot fill the disk
	// the submission store shares.
	MinFree float64

	mu       sync.Mutex
	items    map[string]*stagedFile
	reserved int64
}

// DefaultStageMinFree matches the submission store's own floor
// (submit.DefaultUploadsMinFree): the two share the uploads volume.
const DefaultStageMinFree = 0.10

type stagedFile struct {
	path      string
	tokenHash [sha256.Size]byte
	size      int64
	used      bool
}

// Staged is one upload on the stage: where the Job fetches it and what it must
// be.
type Staged struct {
	ID     string
	Token  string
	SHA256 string
	Size   int64
}

var (
	// ErrStageFull is an upload that would leave less than MinFree of the staging
	// filesystem free, or that ran it out of space outright.
	ErrStageFull = errors.New("fileedit: no room to stage the upload")
	// ErrShortUpload is a body that ended, or broke, before its declared length.
	ErrShortUpload = errors.New("fileedit: the upload ended before its declared length")
	// ErrNotStaged is an Open with an unknown id, a wrong token, or a spent one.
	// They are one error on purpose: the internal face answers all three the same.
	ErrNotStaged = errors.New("fileedit: no such staged upload")
)

// statfs reports a filesystem's available and total bytes. A var so a test can
// stage against a disk of a chosen size.
var statfs = func(dir string) (avail, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize) // uint32 on darwin
	return uint64(st.Bavail) * bsize, uint64(st.Blocks) * bsize, nil
}

// Sweep deletes everything under Dir. Call it once, before the first Put.
func (s *Stage) Sweep() error {
	if err := os.RemoveAll(s.Dir); err != nil {
		return fmt.Errorf("fileedit: clear the upload stage: %w", err)
	}
	return nil
}

// Put stages exactly size bytes from body and returns the handle a Job fetches
// it by, plus the func that deletes it. body must end right after size bytes (an
// HTTP body with that Content-Length does): Put reads to its end, which is also
// what tells the server the body is done.
func (s *Stage) Put(body io.Reader, size int64) (Staged, func(), error) {
	if size < 0 {
		return Staged{}, nil, fmt.Errorf("fileedit: an upload of %d bytes", size)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return Staged{}, nil, fmt.Errorf("fileedit: create the upload stage: %w", err)
	}
	if err := s.reserve(size); err != nil {
		return Staged{}, nil, err
	}
	defer s.unreserve(size)

	f, err := os.CreateTemp(s.Dir, "upload-*")
	if err != nil {
		return Staged{}, nil, fmt.Errorf("fileedit: stage the upload: %w", err)
	}
	h := sha256.New()
	src := &bodyReader{r: body}
	// One byte past size, so the read that finds the end happens here.
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(src, size+1))
	closeErr := f.Close()
	if err := stageFailure(src.err, copyErr, closeErr, n, size); err != nil {
		os.Remove(f.Name())
		return Staged{}, nil, err
	}

	st, tokenHash, err := newHandle(h, size)
	if err != nil {
		os.Remove(f.Name())
		return Staged{}, nil, err
	}
	s.mu.Lock()
	if s.items == nil {
		s.items = map[string]*stagedFile{}
	}
	s.items[st.ID] = &stagedFile{path: f.Name(), tokenHash: tokenHash, size: size}
	s.mu.Unlock()

	release := func() {
		s.mu.Lock()
		delete(s.items, st.ID)
		s.mu.Unlock()
		os.Remove(f.Name())
	}
	return st, release, nil
}

// stageFailure decides what a finished copy means. The body's own error, or a
// body shorter than promised, is the caller's; a body longer than promised
// cannot come through net/http, which stops at Content-Length, but a direct
// caller could send one and it is refused all the same.
func stageFailure(readErr, copyErr, closeErr error, n, size int64) error {
	switch {
	case readErr != nil:
		return fmt.Errorf("%w: %v", ErrShortUpload, readErr)
	case copyErr == nil && n < size:
		return fmt.Errorf("%w: got %d of %d bytes", ErrShortUpload, n, size)
	case copyErr == nil && n > size:
		return fmt.Errorf("fileedit: the upload is longer than its declared %d bytes", size)
	}
	err := copyErr
	if err == nil {
		err = closeErr
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return fmt.Errorf("%w: %v", ErrStageFull, err)
	}
	return fmt.Errorf("fileedit: stage the upload: %w", err)
}

// newHandle mints the id and token for a staged upload whose bytes h hashed.
func newHandle(h hash.Hash, size int64) (Staged, [sha256.Size]byte, error) {
	var id [16]byte
	var token [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Staged{}, [sha256.Size]byte{}, fmt.Errorf("fileedit: generate an upload id: %w", err)
	}
	if _, err := rand.Read(token[:]); err != nil {
		return Staged{}, [sha256.Size]byte{}, fmt.Errorf("fileedit: generate an upload token: %w", err)
	}
	st := Staged{
		ID:     hex.EncodeToString(id[:]),
		Token:  hex.EncodeToString(token[:]),
		SHA256: hex.EncodeToString(h.Sum(nil)),
		Size:   size,
	}
	return st, sha256.Sum256([]byte(st.Token)), nil
}

// reserve admits an upload of size bytes if the disk keeps MinFree free after it
// and after every upload still being written. Those have not reached the disk
// yet, so statfs alone would let two of them through on room for one.
func (s *Stage) reserve(size int64) error {
	avail, total, err := statfs(s.Dir)
	if err != nil {
		return fmt.Errorf("fileedit: measure the upload stage: %w", err)
	}
	minFree := s.MinFree
	if minFree <= 0 {
		minFree = DefaultStageMinFree
	}
	floor := uint64(float64(total) * minFree)
	s.mu.Lock()
	defer s.mu.Unlock()
	need := uint64(s.reserved) + uint64(size)
	if avail < need || avail-need < floor {
		return fmt.Errorf("%w: %d MiB free of %d MiB, and staging %d MiB would leave less than %.0f%% free",
			ErrStageFull, avail>>20, total>>20, need>>20, minFree*100)
	}
	s.reserved += size
	return nil
}

func (s *Stage) unreserve(size int64) {
	s.mu.Lock()
	s.reserved -= size
	s.mu.Unlock()
}

// Open spends a staged upload's token and returns its file and size. Any
// mismatch is ErrNotStaged.
func (s *Stage) Open(id, token string) (*os.File, int64, error) {
	sum := sha256.Sum256([]byte(token))
	s.mu.Lock()
	it, ok := s.items[id]
	if !ok || it.used || subtle.ConstantTimeCompare(sum[:], it.tokenHash[:]) != 1 {
		s.mu.Unlock()
		return nil, 0, ErrNotStaged
	}
	it.used = true
	s.mu.Unlock()
	f, err := os.Open(it.path)
	if err != nil {
		return nil, 0, fmt.Errorf("fileedit: open the staged upload: %w", err)
	}
	return f, it.size, nil
}

// bodyReader remembers the body's own read error, so Put can tell a client that
// went away from the disk filling up.
type bodyReader struct {
	r   io.Reader
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF {
		b.err = err
	}
	return n, err
}
