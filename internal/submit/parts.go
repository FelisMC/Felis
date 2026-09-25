package submit

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultPartMaxBytes caps one part of a chunked upload. A single request body is
// bounded by whatever proxy fronts the API — the Cloudflare edge refuses bodies
// over 100 MB on the Free and Pro plans — so a large modpack arrives as a run of
// parts. 32 MiB stays well under that limit and keeps a retried part cheap.
const DefaultPartMaxBytes = 32 << 20

// StalePartRetention is how long a staged upload may sit untouched before the
// reaper deletes it: long enough to resume after a lost connection or a laptop
// lid, short enough that abandoned parts do not hold the uploads volume.
const StalePartRetention = 24 * time.Hour

// partSuffix names a staged upload on disk: {Dir}/{id}.part.
const partSuffix = ".part"

// ErrUploadBusy reports that another request is writing (or completing) the same
// staged upload. Parts go in order, one at a time; the API answers 409 and the
// client asks where the upload stands before it sends again.
var ErrUploadBusy = errors.New("submit: another request is writing this upload")

// ErrPartTooLarge reports a part longer than the part cap. The API answers 413.
var ErrPartTooLarge = errors.New("submit: an upload part exceeds the part size limit")

// OffsetMismatchError reports a part that does not start where the staged upload
// ends. Received is where it does end, so the client resumes from there.
type OffsetMismatchError struct {
	Received int64
}

func (e *OffsetMismatchError) Error() string {
	return fmt.Sprintf("submit: the upload holds %d bytes; send the part that starts there", e.Received)
}

// PartStore stages a chunked upload until Manager.CompleteUpload hands the
// assembled bytes to Blobs. Parts are appended in order to {Dir}/{id}.part, and
// the file's length is the resume point: a client that lost its connection asks
// for it and carries on from there. The store serializes the requests for one id
// in process, which is enough for the single felis-api replica (its Deployment is
// Recreate, never two pods at once).
type PartStore struct {
	// Dir holds the staged uploads. cmd/felis puts it on the uploads volume, so a
	// staged upload survives an API restart and the room check sees the same disk.
	Dir string
	// MinFree is the share of Dir's filesystem a part must leave free; 0 uses
	// DefaultUploadsMinFree.
	MinFree float64

	mu   sync.Mutex
	busy map[string]bool
}

// hold claims id for one request; the returned func releases it. A second claim
// while the first is held fails with ErrUploadBusy.
func (s *PartStore) hold(id string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[id] {
		return nil, ErrUploadBusy
	}
	if s.busy == nil {
		s.busy = map[string]bool{}
	}
	s.busy[id] = true
	return func() {
		s.mu.Lock()
		delete(s.busy, id)
		s.mu.Unlock()
	}, nil
}

func (s *PartStore) path(id string) (string, error) {
	if !idRE.MatchString(id) {
		return "", fmt.Errorf("submit: invalid submission id %q", id)
	}
	return filepath.Join(s.Dir, id+partSuffix), nil
}

// CheckRoom refuses a part of up to need bytes that could push Dir's filesystem
// below its free floor.
func (s *PartStore) CheckRoom(need int64) error {
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return fmt.Errorf("submit: mkdir upload parts dir: %w", err)
	}
	return checkRoom(s.Dir, need, s.MinFree)
}

// Size reports how many bytes are staged for id; nothing staged is (0, false, nil).
func (s *PartStore) Size(id string) (int64, bool, error) {
	p, err := s.path(id)
	if err != nil {
		return 0, false, err
	}
	fi, err := os.Stat(p)
	switch {
	case err == nil:
		return fi.Size(), true, nil
	case os.IsNotExist(err):
		return 0, false, nil
	default:
		return 0, false, fmt.Errorf("submit: stat staged upload: %w", err)
	}
}

// Append writes r to id's staged upload at offset and returns the new length.
// Offset 0 starts the upload over; any other offset must equal the staged length,
// or the call fails with *OffsetMismatchError naming it. A part that fails to
// arrive whole is cut back off, so the staged bytes are always a prefix of what
// the client sent.
func (s *PartStore) Append(id string, offset int64, r io.Reader) (int64, error) {
	p, err := s.path(id)
	if err != nil {
		return 0, err
	}
	release, err := s.hold(id)
	if err != nil {
		return 0, err
	}
	defer release()
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return 0, fmt.Errorf("submit: mkdir upload parts dir: %w", err)
	}
	var f *os.File
	if offset == 0 {
		f, err = os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	} else {
		f, err = os.OpenFile(p, os.O_WRONLY, 0)
		if os.IsNotExist(err) {
			return 0, &OffsetMismatchError{Received: 0}
		}
	}
	if err != nil {
		return 0, fmt.Errorf("submit: open staged upload: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return 0, fmt.Errorf("submit: stat staged upload: %w", err)
	}
	if fi.Size() != offset {
		f.Close()
		return 0, &OffsetMismatchError{Received: fi.Size()}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return 0, fmt.Errorf("submit: seek staged upload: %w", err)
	}
	n, err := io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		f.Truncate(offset)
		f.Close()
		return 0, fmt.Errorf("submit: write upload part: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("submit: close staged upload: %w", err)
	}
	return offset + n, nil
}

// open returns id's staged upload for reading, or an ErrInvalid error when
// nothing is staged. The caller must already hold id.
func (s *PartStore) open(id string) (*os.File, error) {
	p, err := s.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, invalidf("no upload in progress for this submission; send its parts first")
	}
	if err != nil {
		return nil, fmt.Errorf("submit: open staged upload: %w", err)
	}
	return f, nil
}

// Delete removes id's staged upload. Nothing staged is success.
func (s *PartStore) Delete(id string) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("submit: remove staged upload: %w", err)
	}
	return nil
}

// Reap deletes every staged upload last written before cutoff, skipping one a
// request holds right now. It returns how many it deleted and carries on past
// one it cannot delete, reporting the first such error.
func (s *PartStore) Reap(cutoff time.Time) (int, error) {
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("submit: list staged uploads: %w", err)
	}
	var reaped int
	var firstErr error
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), partSuffix)
		if !ok || e.IsDir() || !idRE.MatchString(id) {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.ModTime().Before(cutoff) {
			continue
		}
		release, err := s.hold(id)
		if err != nil {
			continue
		}
		err = s.Delete(id)
		release()
		if err == nil {
			reaped++
		} else if firstErr == nil {
			firstErr = err
		}
	}
	return reaped, firstErr
}
