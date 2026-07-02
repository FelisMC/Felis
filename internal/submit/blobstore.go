package submit

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
// Base MUST equal the Manager's ContextStore so the blob lands exactly where
// deriveContextRef points Kaniko's --context; cmd/felis wires both from the one
// config field (registry.user_uploads_context).
//
// INTEGRATION-ONLY seam (out of scope of the upload transport): persisting the
// blob is end-to-end only once the same uploads PVC is mounted into the Kaniko
// build Pod and Kaniko is told to read a local context (build/jobspec.go passes
// the ref straight into --context). The transport here makes the file durable at
// the derived location; wiring that path into the sandboxed build Job is a
// separate deployment integration, exactly like the restore executor's PVC mount.
type LocalContextStore struct {
	// Base is the directory (uploads PVC mount) submission contexts are written
	// under. Each submission gets its own {Base}/{id}/ subdirectory.
	Base string
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

// Compile-time proof that the filesystem store satisfies the Blobs transport.
var _ Blobs = (*LocalContextStore)(nil)
