package submit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

const contextBlobName = "context.tar.gz"

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

// LocalContextStore.Base must match Manager.ContextStore; build Jobs fetch its bytes through the internal API rather than mounting it.
type LocalContextStore struct {
	Base string
	// MinFree=0 uses DefaultUploadsMinFree.
	MinFree float64

	sync func(*os.File) error
}

// DefaultUploadsMinFree preserves node headroom because local-path uploads share disk with worlds and Postgres.
const DefaultUploadsMinFree = 0.10

func (s *LocalContextStore) CheckRoom(need int64) error {
	return checkRoom(s.Base, need, s.MinFree)
}

// checkRoom wraps ErrUploadsFull when the proposed upload would breach the configured free-space share.
func checkRoom(dir string, need int64, minFree float64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("submit: measure the uploads store %s: %w", dir, err)
	}
	bsize := uint64(st.Bsize)
	total, avail := uint64(st.Blocks)*bsize, uint64(st.Bavail)*bsize
	if total == 0 {
		return nil
	}
	if minFree <= 0 {
		minFree = DefaultUploadsMinFree
	}
	floor := uint64(float64(total) * minFree)
	if n := uint64(max(need, 0)); avail < n || avail-n < floor {
		return fmt.Errorf("%w: %d MiB free of %d MiB, and an upload of up to %d MiB would leave less than %.0f%% free",
			ErrUploadsFull, avail>>20, total>>20, n>>20, minFree*100)
	}
	return nil
}

func (s *LocalContextStore) dir(id string) (string, error) {
	if !idRE.MatchString(id) {
		return "", fmt.Errorf("submit: invalid submission id %q", id)
	}
	return filepath.Join(s.Base, id), nil
}

// Put syncs a temporary sibling before rename and parent directories after it so incomplete uploads never replace a good context.
func (s *LocalContextStore) Put(_ context.Context, id string, r io.Reader) (int64, error) {
	dir, err := s.dir(id)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 0, fmt.Errorf("submit: mkdir context dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, contextBlobName+".*.tmp")
	if err != nil {
		return 0, fmt.Errorf("submit: create temp context: %w", err)
	}
	tmpName := tmp.Name()
	n, err := io.Copy(tmp, r)
	if err == nil {
		err = s.flush(tmp)
	}
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return 0, fmt.Errorf("submit: write context blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return 0, fmt.Errorf("submit: close context blob: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, contextBlobName)); err != nil {
		os.Remove(tmpName)
		return 0, fmt.Errorf("submit: commit context blob: %w", err)
	}
	for _, d := range []string{dir, s.Base} {
		if err := s.syncDir(d); err != nil {
			return 0, fmt.Errorf("submit: sync context dir: %w", err)
		}
	}
	return n, nil
}

func (s *LocalContextStore) flush(f *os.File) error {
	if s.sync != nil {
		return s.sync(f)
	}
	return f.Sync()
}

func (s *LocalContextStore) syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := s.flush(d); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

func (s *LocalContextStore) Exists(_ context.Context, id string) (bool, error) {
	dir, err := s.dir(id)
	if err != nil {
		return false, err
	}
	switch _, err := os.Stat(filepath.Join(dir, contextBlobName)); {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("submit: stat context blob: %w", err)
	}
}

// Open wraps ErrBlobNotFound for absent blobs so the API can return 404.
func (s *LocalContextStore) Open(_ context.Context, id string) (io.ReadCloser, error) {
	dir, err := s.dir(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, contextBlobName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %v", ErrBlobNotFound, err)
		}
		return nil, fmt.Errorf("submit: open context blob: %w", err)
	}
	return f, nil
}

// Size returns (0,false,nil) for absent blobs.
func (s *LocalContextStore) Size(_ context.Context, id string) (int64, bool, error) {
	dir, err := s.dir(id)
	if err != nil {
		return 0, false, err
	}
	fi, err := os.Stat(filepath.Join(dir, contextBlobName))
	switch {
	case err == nil:
		return fi.Size(), true, nil
	case os.IsNotExist(err):
		return 0, false, nil
	default:
		return 0, false, fmt.Errorf("submit: stat context blob: %w", err)
	}
}

// Delete removes the blob directory after row deletion and treats absence as success.
func (s *LocalContextStore) Delete(_ context.Context, id string) error {
	dir, err := s.dir(id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("submit: remove context blob: %w", err)
	}
	return nil
}

var _ Blobs = (*LocalContextStore)(nil)
