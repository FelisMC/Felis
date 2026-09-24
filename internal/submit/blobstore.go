package submit

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

// contextBlobName is the fixed object name of a submission's build context under
// its id-namespaced prefix. It is the single source of truth for both the
// derived context ref (deriveContextRef) and the on-disk write target
// (LocalContextStore), so the blob always lands exactly where Kaniko's
// --context points (build/jobspec.go).
const contextBlobName = "context.tar.gz"

// idRE re-validates a submission id at the storage boundary. The Manager only
// ever passes ids it loaded from the Store (already the crypto-hex ids newID
// mints), but LocalContextStore is a standalone component that treats the id as
// untrusted path input: a lowercase-alphanumeric-with-dashes id can contain no
// path separator and no "..", so it can never escape Base. This is the same
// defense-in-depth stance as internal/backup's zip-slip guard.
var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

// LocalContextStore is the filesystem-backed build-context blob store: it writes
// each submission's uploaded modpack to {Base}/{id}/context.tar.gz on a mounted
// PVC. It is one of two implemented Blobs backends — cmd/felis selects it when
// user_uploads_context is a local path and S3ContextStore when it is an s3:// base;
// a base that is neither (or an s3:// base with no credentials configured) leaves
// Manager.Blobs nil so the upload endpoint returns 503 rather than pretending to
// accept a file it cannot persist.
//
// Base MUST equal the Manager's ContextStore; cmd/felis wires both from the one
// config field (registry.user_uploads_context).
//
// The build Pod never mounts this PVC. With Manager.ContextBaseURL set (every
// installed API) the derived context ref is the internal face's
// /api/v1/internal/submissions/{id}/context route, which streams the blob out
// of this store to the build Job's `felis fetch-context` step.
type LocalContextStore struct {
	// Base is the directory (uploads PVC mount) submission contexts are written
	// under. Each submission gets its own {Base}/{id}/ subdirectory.
	Base string
	// MinFree is the share of Base's filesystem an upload must leave free; 0 uses
	// DefaultUploadsMinFree.
	MinFree float64
}

// DefaultUploadsMinFree is the share of the uploads filesystem an upload must
// leave free. On k3s local-path the uploads PVC is a directory on the node's
// disk, beside the worlds and the database, and below about a tenth free the
// kubelet starts evicting pods (the same floor backup.MinFreeAfter keeps).
const DefaultUploadsMinFree = 0.10

// CheckRoom refuses an upload of up to need bytes that could push Base's
// filesystem below its free floor.
func (s *LocalContextStore) CheckRoom(need int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.Base, &st); err != nil {
		return fmt.Errorf("submit: measure the uploads store %s: %w", s.Base, err)
	}
	bsize := uint64(st.Bsize) // uint32 on darwin
	total, avail := uint64(st.Blocks)*bsize, uint64(st.Bavail)*bsize
	if total == 0 {
		return nil
	}
	minFree := s.MinFree
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

// dir returns the per-submission directory, rejecting an id that could escape
// Base. Every path the store touches is rooted here.
func (s *LocalContextStore) dir(id string) (string, error) {
	if !idRE.MatchString(id) {
		return "", fmt.Errorf("submit: invalid submission id %q", id)
	}
	return filepath.Join(s.Base, id), nil
}

// Put writes r to {Base}/{id}/context.tar.gz atomically: it streams into a temp
// file in the same directory and renames it over any previous upload only on a
// fully successful copy. So a failed, truncated, or oversize upload never
// replaces a good context and never leaves a half-written blob for Kaniko to
// read; a re-upload while the submission is still pending simply supersedes the
// previous one. It returns the number of bytes stored.
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
	return n, nil
}

// Exists reports whether a context blob has been stored for id. Approve consults
// it so a submission whose context was never uploaded is refused BEFORE the CAS,
// instead of being approved into a build Kaniko cannot pull.
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

// Open returns the stored context blob for id — the read side of the transport the
// build Pod's fetch initContainer uses. A missing blob is ErrBlobNotFound (404 on
// the route), never a bare os error, so the API keeps its status mapping.
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

// Size reports the stored blob's size — the accounting read behind the per-user
// storage budget. A missing blob is (0, false, nil): absence is not an error
// here, it is simply no bytes to count (the same distinction Exists draws for
// Approve).
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

// Delete removes everything stored for id — the blob plus its id-namespaced
// directory (a stray temp file from an interrupted upload goes with it) — after
// the submission row is gone. Idempotent: nothing stored is success.
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

// Compile-time proof that the filesystem store satisfies the Blobs transport.
var _ Blobs = (*LocalContextStore)(nil)
