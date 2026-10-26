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
	"math"
	"os"
	"sync"
	"syscall"
	"time"
)

// Stage serves uploads once by token hash; its in-memory index does not survive restart, so Sweep clears leftover files at startup.
type Stage struct {
	Dir string
	// MinFree defaults to DefaultStageMinFree and bounds usage on the shared uploads filesystem.
	MinFree float64

	Now func() time.Time

	mu       sync.Mutex
	items    map[string]*stagedFile
	sessions map[string]*session
	reserved int64
}

// DefaultStageMinFree must match submit.DefaultUploadsMinFree because both share the uploads volume.
const DefaultStageMinFree = 0.10

type stagedFile struct {
	path      string
	tokenHash [sha256.Size]byte
	size      int64
	used      bool
}

type Staged struct {
	ID     string
	Token  string
	SHA256 string
	Size   int64
}

var (
	ErrStageFull   = errors.New("fileedit: no room to stage the upload")
	ErrShortUpload = errors.New("fileedit: the upload ended before its declared length")
	// ErrNotStaged covers unknown IDs and invalid or spent tokens without exposing which state occurred.
	ErrNotStaged      = errors.New("fileedit: no such staged upload")
	ErrNoDigest       = errors.New("fileedit: the upload carries no SHA-256 digest")
	ErrDigestMismatch = errors.New("fileedit: the bytes that arrived do not match the digest they were sent with")
)

func checkDigest(got, want []byte) error {
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return fmt.Errorf("%w: they hash to %x, sent as %x", ErrDigestMismatch, got, want)
	}
	return nil
}

var statfs = func(dir string) (avail, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	return uint64(st.Bavail) * bsize, uint64(st.Blocks) * bsize, nil
}

// Sweep must run before the first Put; it deletes all staged files.
func (s *Stage) Sweep() error {
	if err := os.RemoveAll(s.Dir); err != nil {
		return fmt.Errorf("fileedit: clear the upload stage: %w", err)
	}
	return nil
}

// Put requires exact size and SHA-256, reads to EOF, and returns a cleanup callback; mismatches stage nothing.
func (s *Stage) Put(body io.Reader, size int64, want []byte) (Staged, func(), error) {
	if size < 0 {
		return Staged{}, nil, fmt.Errorf("fileedit: an upload of %d bytes", size)
	}
	if len(want) != sha256.Size {
		return Staged{}, nil, ErrNoDigest
	}
	st, path, _, err := s.stage(body, size, want)
	if err != nil {
		return Staged{}, nil, err
	}
	s.mu.Lock()
	if s.items == nil {
		s.items = map[string]*stagedFile{}
	}
	s.items[st.ID] = &stagedFile{path: path, tokenHash: sha256.Sum256([]byte(st.Token)), size: st.Size}
	s.mu.Unlock()
	return st, func() {
		s.mu.Lock()
		delete(s.items, st.ID)
		s.mu.Unlock()
		os.Remove(path)
	}, nil
}

// A size of -1 makes Receive reserve the remaining staging budget.
func (s *Stage) Receive(user, server, path string, body io.Reader, size int64) (Staged, func(), error) {
	id, err := randomHex(16)
	if err != nil {
		return Staged{}, nil, fmt.Errorf("fileedit: generate an upload id: %w", err)
	}
	s.mu.Lock()
	n := 0
	for _, ss := range s.sessions {
		if ss.user == user {
			n++
		}
	}
	if n >= MaxSessionsPerUser {
		s.mu.Unlock()
		return Staged{}, nil, ErrTooManySessions
	}
	if s.sessions == nil {
		s.sessions = map[string]*session{}
	}
	s.sessions[id] = &session{user: user, server: server, path: path, busy: true, touched: s.now()}
	s.mu.Unlock()

	st, file, h, err := s.stage(body, size, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		delete(s.sessions, id)
		return Staged{}, nil, err
	}
	st.ID = id
	s.sessions[id] = &session{
		user: user, server: server, path: path, file: file, size: st.Size, received: st.Size,
		armed: true, tokenHash: sha256.Sum256([]byte(st.Token)), h: h, touched: s.now(),
	}
	return st, func() { _ = s.Drop(user, server, id) }, nil
}

func (s *Stage) stage(body io.Reader, size int64, want []byte) (Staged, string, hash.Hash, error) {
	if size < -1 || size == math.MaxInt64 {
		return Staged{}, "", nil, fmt.Errorf("fileedit: an upload of %d bytes", size)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return Staged{}, "", nil, fmt.Errorf("fileedit: create the upload stage: %w", err)
	}
	budget := size
	var err error
	if size < 0 {
		budget, err = s.reserveAvailable()
	} else {
		err = s.reserve(size)
	}
	if err != nil {
		return Staged{}, "", nil, err
	}
	defer s.unreserve(budget)
	f, err := os.CreateTemp(s.Dir, "upload-*")
	if err != nil {
		return Staged{}, "", nil, fmt.Errorf("fileedit: stage the upload: %w", err)
	}
	h := sha256.New()
	src := &bodyReader{r: body}
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(src, budget+1))
	closeErr := f.Close()
	expected := size
	if size < 0 {
		expected = n
	}
	err = stageFailure(src.err, copyErr, closeErr, n, expected)
	if err == nil && n > budget {
		err = ErrStageFull
	}
	if err == nil && want != nil {
		err = checkDigest(h.Sum(nil), want)
	}
	if err != nil {
		os.Remove(f.Name())
		return Staged{}, "", nil, err
	}
	st, _, err := newHandle(h, n)
	if err != nil {
		os.Remove(f.Name())
	}
	return st, f.Name(), h, err
}

// stageFailure rejects both short bodies and oversized direct callers, even though net/http caps bodies at Content-Length.
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

func newHandle(h hash.Hash, size int64) (Staged, [sha256.Size]byte, error) {
	id, err := randomHex(16)
	if err != nil {
		return Staged{}, [sha256.Size]byte{}, fmt.Errorf("fileedit: generate an upload id: %w", err)
	}
	token, err := randomHex(32)
	if err != nil {
		return Staged{}, [sha256.Size]byte{}, fmt.Errorf("fileedit: generate an upload token: %w", err)
	}
	st := Staged{ID: id, Token: token, SHA256: hex.EncodeToString(h.Sum(nil)), Size: size}
	return st, sha256.Sum256([]byte(st.Token)), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// reserve includes in-flight uploads not yet visible in statfs so concurrent uploads cannot overcommit disk space.
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

func (s *Stage) reserveAvailable() (int64, error) {
	avail, total, err := statfs(s.Dir)
	if err != nil {
		return 0, fmt.Errorf("fileedit: measure the upload stage: %w", err)
	}
	minFree := s.MinFree
	if minFree <= 0 {
		minFree = DefaultStageMinFree
	}
	floor := uint64(float64(total) * minFree)
	s.mu.Lock()
	defer s.mu.Unlock()
	reserved := uint64(s.reserved)
	if avail <= floor || avail-floor <= reserved {
		return 0, ErrStageFull
	}
	budget := min(avail-floor-reserved, uint64(math.MaxInt64-1))
	s.reserved += int64(budget)
	return int64(budget), nil
}

func (s *Stage) unreserve(size int64) {
	s.mu.Lock()
	s.reserved -= size
	s.mu.Unlock()
}

// Open atomically spends the staged upload or sealed-session token; all mismatches return ErrNotStaged.
func (s *Stage) Open(id, token string) (*os.File, int64, error) {
	sum := sha256.Sum256([]byte(token))
	s.mu.Lock()
	name, size, found, err := s.openSession(id, sum)
	if !found {
		it, ok := s.items[id]
		if !ok || it.used || subtle.ConstantTimeCompare(sum[:], it.tokenHash[:]) != 1 {
			err = ErrNotStaged
		} else {
			it.used = true
			name, size = it.path, it.size
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, 0, fmt.Errorf("fileedit: open the staged upload: %w", err)
	}
	return f, size, nil
}

// bodyReader retains source errors so Put can distinguish disconnects from full-disk failures.
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
