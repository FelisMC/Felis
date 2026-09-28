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

// A file too big for one request body arrives as a session: Begin declares its
// size and where it goes, Append adds one part at a time in order, and Seal
// hands the finished file to the Job that lands it, which fetches it through
// Open like any other staged upload. The Cloudflare edge refuses bodies over
// 100 MB, so a part is at most PartBytes; the file itself has no ceiling but the
// room on the staging disk, and Begin reserves all of it up front, so an upload
// that starts is one the disk can finish.
//
// Every call names the user and the server the session was begun for, and a
// session answers no one else: an id that is someone else's reads as unknown.
//
// Each part carries the SHA-256 the client computed over it, and a part whose
// bytes hash to anything else was changed on the way and is refused. A part that
// fails midway (the connection dropped, the edge cut it off, the digest did not
// match) is rolled back to where it started, so the session's length is always
// the resume point. The session keeps each part's size and digest, so a client
// resuming with a file from disk can check the file still holds the bytes
// already sent before it sends the rest.
//
// A sealed session stays until the Job that fetched it says its file has landed
// (Landed), so a Job that failed at any point before that (a broken fetch, a
// full volume, a file in the way) can be started again without the file being
// sent again; one left idle for SessionIdle is dropped (Expire).

// PartBytes is the largest part Append takes, matching the modpack upload's
// parts (submit.DefaultPartMaxBytes).
const PartBytes = 32 << 20

// SessionIdle is how long a session may sit untouched before Expire drops it:
// long enough to resume after a lost connection or a laptop lid, short enough
// that an abandoned upload gives its room back the same day.
const SessionIdle = 6 * time.Hour

// MaxSessionsPerUser bounds the sessions one user holds open at once. Each
// reserves its whole size on the staging disk, so without a bound one user
// could reserve the disk out from under everyone for SessionIdle.
const MaxSessionsPerUser = 4

var (
	// ErrTooManySessions is a Begin by a user who already holds
	// MaxSessionsPerUser sessions.
	ErrTooManySessions = errors.New("fileedit: too many uploads in progress")
	// ErrUploadBusy is a call on a session another request is still appending
	// to. Parts go one at a time.
	ErrUploadBusy = errors.New("fileedit: another request is still writing this upload")
	// ErrPartTooLarge is a part over PartBytes, or one that runs past the size
	// the session was begun with.
	ErrPartTooLarge = errors.New("fileedit: the part is too large")
	// ErrUploadIncomplete is a Seal before every byte has arrived.
	ErrUploadIncomplete = errors.New("fileedit: the upload has not finished arriving")
)

// OffsetError is a part that does not start where the session ends. Received is
// where it does end, so the client resumes from there.
type OffsetError struct{ Received int64 }

func (e *OffsetError) Error() string {
	return fmt.Sprintf("fileedit: the upload holds %d bytes; send the part that starts there", e.Received)
}

// Session is where one session stands.
type Session struct {
	ID       string
	Path     string
	Size     int64
	Received int64
	// Parts are the parts that make up Received, in order.
	Parts []Part
}

// Part is one part a session took: its length and the SHA-256 (lowercase hex)
// it arrived with and matched.
type Part struct {
	Size   int64
	SHA256 string
}

type session struct {
	user, server, path string
	file               string
	size, received     int64
	h                  hash.Hash
	parts              []Part
	busy               bool
	touched            time.Time

	// armed is set by Seal with the digest of the token it minted and cleared by
	// the Open that spends it. tokenHash stays until the next Seal, so the Job
	// holding that token can still report its file landed (Landed).
	armed     bool
	tokenHash [sha256.Size]byte
}

func (s *Stage) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Begin opens a session for a file of size bytes that will land at path on
// server, reserving room for all of it.
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

// lookup finds the caller's session. s.mu must be held.
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

// Status reports where the caller's session stands.
func (s *Stage) Status(user, server, id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, err := s.lookup(user, server, id)
	if err != nil {
		return Session{}, err
	}
	return ss.view(id), nil
}

// Append adds the n bytes of body at offset, which must be where the session
// ends. body must end right after them (an HTTP body of that Content-Length
// does), and they must hash to want, the SHA-256 the client computed over them
// (ErrDigestMismatch otherwise). On any failure the session is left as it was
// before the call.
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

	// The session is ours until busy is cleared, so the file and the hash are
	// touched without the lock. The hash's state is kept to undo a failed part.
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

// appendPart writes exactly n bytes of body at offset in the file named file,
// feeding them to h as well.
func appendPart(file string, offset int64, body io.Reader, n int64, h io.Writer) error {
	f, err := os.OpenFile(file, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("fileedit: open the staged upload: %w", err)
	}
	src := &bodyReader{r: body}
	// One byte past n, so the read that finds the end happens here.
	got, copyErr := io.Copy(io.MultiWriter(io.NewOffsetWriter(f, offset), h), io.LimitReader(src, n+1))
	closeErr := f.Close()
	return stageFailure(src.err, copyErr, closeErr, got, n)
}

// Seal ends the caller's session and arms it for one fetch: the Staged it
// returns carries a fresh token, and any token an earlier Seal minted stops
// working. Every byte must have arrived.
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

// openSession is Open for a sealed session. s.mu must be held; ok is false when
// id names no session.
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

// Landed tells the stage the Job holding token has put session id's file in
// place, and deletes the session: nothing will fetch it again. Only the token
// the latest Seal minted says so, spent or not, so a Job an earlier commit
// started, or anyone else, changes nothing (ErrNotStaged). An upload staged by
// Put answers to its own token too and is left to the release that deletes it.
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
	// Before the first Seal tokenHash is zero, which no token hashes to. A sealed
	// session has every byte, so a part arriving now could only be an empty one
	// and leaves nothing to account for: no busy check, unlike Drop.
	if subtle.ConstantTimeCompare(sum[:], ss.tokenHash[:]) != 1 {
		s.mu.Unlock()
		return ErrNotStaged
	}
	s.dropLocked(id, ss)
	s.mu.Unlock()
	os.Remove(ss.file)
	return nil
}

// Drop cancels the caller's session and deletes what it holds.
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

// dropLocked forgets a session and gives back the room it still had reserved.
// s.mu must be held; the caller deletes the file.
func (s *Stage) dropLocked(id string, ss *session) {
	delete(s.sessions, id)
	s.reserved -= ss.size - ss.received
}

// Expire drops every session untouched for SessionIdle, sealed or not, and
// reports how many it dropped. A session a part is arriving for is never idle.
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
