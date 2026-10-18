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

// DefaultPartMaxBytes keeps resumable parts below the edge body limit and bounds retransmission cost.
const DefaultPartMaxBytes = 32 << 20

const StalePartRetention = 24 * time.Hour

const partSuffix = ".part"

var ErrUploadBusy = errors.New("submit: another request is writing this upload")

var ErrPartTooLarge = errors.New("submit: an upload part exceeds the part size limit")

type OffsetMismatchError struct {
	Received int64
}

func (e *OffsetMismatchError) Error() string {
	return fmt.Sprintf("submit: the upload holds %d bytes; send the part that starts there", e.Received)
}

// PartStore serializes each ID in-process; its file length is the durable resume offset across API restarts.
type PartStore struct {
	Dir string
	// MinFree=0 uses DefaultUploadsMinFree.
	MinFree float64

	mu   sync.Mutex
	busy map[string]bool
}

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

func (s *PartStore) CheckRoom(need int64) error {
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return fmt.Errorf("submit: mkdir upload parts dir: %w", err)
	}
	return checkRoom(s.Dir, need, s.MinFree)
}

// Size returns (0,false,nil) when no partial upload is staged.
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

// Append resets at offset 0, otherwise requires the current length and rolls back incomplete parts.
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

// open requires the caller to hold the ID and returns ErrInvalid when no upload is staged.
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

// Reap skips held uploads and continues after removal failures, returning the first error and count removed.
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
