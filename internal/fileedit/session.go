package fileedit

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"slices"
	"time"
)

const PartBytes = 32 << 20 // parts stay below the Cloudflare body limit

const SessionIdle = 6 * time.Hour

const MaxSessionsPerUser = 4

var (
	ErrTooManySessions  = errors.New("fileedit: too many uploads in progress")
	ErrUploadBusy       = errors.New("fileedit: another request is still writing this upload")
	ErrPartTooLarge     = errors.New("fileedit: the part is too large")
	ErrUploadIncomplete = errors.New("fileedit: the upload has not finished arriving")
)

type OffsetError struct{ Received int64 }

func (e *OffsetError) Error() string {
	return fmt.Sprintf("fileedit: the upload holds %d bytes; send the part that starts there", e.Received)
}

type Session struct {
	ID       string
	Path     string
	Size     int64
	Received int64
	Parts    []Part
}

type Part struct {
	Size   int64
	SHA256 string // lowercase hex
}

type session struct {
	user, server, path string
	file               string
	size, received     int64
	h                  hash.Hash
	parts              []Part
	busy               bool
	touched            time.Time

	// armed is cleared by Open; tokenHash remains so Landed can authenticate completion.
	armed     bool
	tokenHash [sha256.Size]byte
}

func (s *Stage) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Stage) Begin(user, server, path string, size int64) (Session, error) {
	if size < 0 {
		return Session{}, fmt.Errorf("fileedit: an upload of %d bytes", size)
	}
	id, err := randomHex(16)
	if err != nil {
		return Session{}, fmt.Errorf("fileedit: generate an upload id: %w", err)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return Session{}, fmt.Errorf("fileedit: create the upload stage: %w", err)
	}
	if err := s.reserve(size); err != nil {
		return Session{}, err
	}
	f, err := os.CreateTemp(s.Dir, "session-*")
	if err != nil {
		s.unreserve(size)
		return Session{}, fmt.Errorf("fileedit: stage the upload: %w", err)
	}
	f.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ss := range s.sessions {
		if ss.user == user {
			n++
		}
	}
	if n >= MaxSessionsPerUser {
		s.reserved -= size
		os.Remove(f.Name())
		return Session{}, fmt.Errorf("%w: finish or cancel one of your %d uploads first", ErrTooManySessions, n)
	}
	if s.sessions == nil {
		s.sessions = map[string]*session{}
	}
	s.sessions[id] = &session{
		user: user, server: server, path: path, file: f.Name(),
		size: size, h: sha256.New(), touched: s.now(),
	}
	return Session{ID: id, Path: path, Size: size}, nil
}

// lookup requires s.mu and scopes sessions to the caller.
func (s *Stage) lookup(user, server, id string) (*session, error) {
	ss, ok := s.sessions[id]
	if !ok || ss.user != user || ss.server != server {
		return nil, ErrNotStaged
	}
	return ss, nil
}

func (ss *session) view(id string) Session {
	return Session{ID: id, Path: ss.path, Size: ss.size, Received: ss.received, Parts: slices.Clone(ss.parts)}
}

func (s *Stage) Status(user, server, id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, err := s.lookup(user, server, id)
	if err != nil {
		return Session{}, err
	}
	return ss.view(id), nil
}

// Append requires the current offset, exact length and matching digest; failure restores the previous session state.
func (s *Stage) Append(user, server, id string, offset int64, body io.Reader, n int64, want []byte) (Session, error) {
	s.mu.Lock()
	ss, err := s.lookup(user, server, id)
	switch {
	case err != nil:
	case len(want) != sha256.Size:
		err = ErrNoDigest
	case ss.busy:
		err = ErrUploadBusy
	case offset != ss.received:
		err = &OffsetError{Received: ss.received}
	case n < 0 || n > PartBytes || n > ss.size-ss.received:
		err = fmt.Errorf("%w: %d bytes at %d of a %d-byte upload; parts are at most %d bytes",
			ErrPartTooLarge, n, offset, ss.size, PartBytes)
	}
	if err != nil {
		var view Session
		if ss != nil {
			view = ss.view(id)
		}
		s.mu.Unlock()
		return view, err
	}
	ss.busy = true
	s.mu.Unlock()

	// busy grants exclusive file and hash access; saving hash state allows a failed part to roll back.
	before, err := ss.h.(encoding.BinaryMarshaler).MarshalBinary()
	if err == nil {
		part := sha256.New()
		err = appendPart(ss.file, offset, body, n, io.MultiWriter(ss.h, part))
		if err == nil {
			err = checkDigest(part.Sum(nil), want)
		}
		if err != nil {
			_ = os.Truncate(ss.file, offset)
			_ = ss.h.(encoding.BinaryUnmarshaler).UnmarshalBinary(before)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ss.busy = false
	ss.touched = s.now()
	if err != nil {
		return ss.view(id), err
	}
	ss.received += n
	s.reserved -= n
	ss.parts = append(ss.parts, Part{Size: n, SHA256: hex.EncodeToString(want)})
	return ss.view(id), nil
}

func appendPart(file string, offset int64, body io.Reader, n int64, h io.Writer) error {
	f, err := os.OpenFile(file, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("fileedit: open the staged upload: %w", err)
	}
	src := &bodyReader{r: body}
	got, copyErr := io.Copy(io.MultiWriter(io.NewOffsetWriter(f, offset), h), io.LimitReader(src, n+1))
	closeErr := f.Close()
	return stageFailure(src.err, copyErr, closeErr, got, n)
}

// Seal rotates the token and arms a complete session for one fetch.
func (s *Stage) Seal(user, server, id string) (Staged, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, err := s.lookup(user, server, id)
	if err != nil {
		return Staged{}, err
	}
	if ss.busy {
		return Staged{}, ErrUploadBusy
	}
	if ss.received != ss.size {
		return Staged{}, fmt.Errorf("%w: %d of %d bytes are here", ErrUploadIncomplete, ss.received, ss.size)
	}
	st, tokenHash, err := newHandle(ss.h, ss.size)
	if err != nil {
		return Staged{}, err
	}
	st.ID = id
	ss.armed, ss.tokenHash = true, tokenHash
	ss.touched = s.now()
	return st, nil
}

// openSession requires s.mu; ok=false means the ID is not a session.
func (s *Stage) openSession(id string, sum [sha256.Size]byte) (path string, size int64, ok bool, err error) {
	ss, found := s.sessions[id]
	if !found {
		return "", 0, false, nil
	}
	if !ss.armed || subtle.ConstantTimeCompare(sum[:], ss.tokenHash[:]) != 1 {
		return "", 0, true, ErrNotStaged
	}
	ss.armed = false
	ss.touched = s.now()
	return ss.file, ss.size, true, nil
}

// Landed requires the latest Seal token; uploads staged by Put retain their separate cleanup callback.
func (s *Stage) Landed(id, token string) error {
	sum := sha256.Sum256([]byte(token))
	s.mu.Lock()
	ss, ok := s.sessions[id]
	if !ok {
		it, found := s.items[id]
		s.mu.Unlock()
		if !found || subtle.ConstantTimeCompare(sum[:], it.tokenHash[:]) != 1 {
			return ErrNotStaged
		}
		return nil
	}
	if subtle.ConstantTimeCompare(sum[:], ss.tokenHash[:]) != 1 {
		s.mu.Unlock()
		return ErrNotStaged
	}
	s.dropLocked(id, ss)
	s.mu.Unlock()
	os.Remove(ss.file)
	return nil
}

func (s *Stage) Drop(user, server, id string) error {
	s.mu.Lock()
	ss, err := s.lookup(user, server, id)
	if err == nil && ss.busy {
		err = ErrUploadBusy
	}
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.dropLocked(id, ss)
	s.mu.Unlock()
	os.Remove(ss.file)
	return nil
}

// dropLocked requires s.mu and leaves file deletion to the caller; unreceived disk reservations are released.
func (s *Stage) dropLocked(id string, ss *session) {
	delete(s.sessions, id)
	s.reserved -= ss.size - ss.received
}

// Expire removes idle sealed or unsealed sessions; an active part write is never idle.
func (s *Stage) Expire() int {
	cutoff := s.now().Add(-SessionIdle)
	var files []string
	s.mu.Lock()
	for id, ss := range s.sessions {
		if !ss.busy && ss.touched.Before(cutoff) {
			s.dropLocked(id, ss)
			files = append(files, ss.file)
		}
	}
	s.mu.Unlock()
	for _, f := range files {
		os.Remove(f)
	}
	return len(files)
}
