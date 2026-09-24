package offsite

// Submission build contexts: the modpacks users uploaded, kept on the uploads
// volume as <id>/context.tar.gz (internal/submit.LocalContextStore). A reviewer
// downloads one to inspect it, and building the submission again needs it; the
// image already built from it is in the registry copy (images.go).
//
// The copy is content-addressed like the images: uploads/blobs/<sha256>.fenc
// holds each context once, and uploads/index/<stamp>.json.fenc versions which
// submission holds which. A context deleted here (a withdrawn submission, a
// rejected one reaped) leaves the newest version but stays in the versions
// before it for UploadHistory. A volume that comes back empty, as it does on a
// rebuilt host before its restore, therefore takes nothing out of the bucket
// for two weeks.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	uploadsDir          = "uploads/"
	uploadBlobsDir      = uploadsDir + "blobs/"
	uploadIndexDir      = uploadsDir + "index/"
	maxUploadIndexBytes = 16 << 20

	// UploadContextFile is the one file of a submission's directory on the
	// uploads volume (internal/submit's contextBlobName).
	UploadContextFile = "context.tar.gz"
)

// UploadHistory is how long an uploads index version is kept after a newer one
// replaced it, with the contexts only it names.
const UploadHistory = ImageHistory

// uploadIDRE is internal/submit's idRE: a submission id can hold no path
// separator and no "..".
var uploadIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

// UploadIndex is one version of the uploads copy: which submission holds which
// context.
type UploadIndex struct {
	Created  time.Time                `json:"created"`
	Contexts map[string]UploadContext `json:"contexts"`
}

// UploadContext is one submission's context as the sync last saw it. Size and
// ModTime let the next pass skip hashing a file that has not changed.
type UploadContext struct {
	Digest  string    `json:"digest"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// Bytes is the total size of the contexts x names.
func (x *UploadIndex) Bytes() int64 {
	var n int64
	for _, c := range x.Contexts {
		n += c.Size
	}
	return n
}

func newUploadIndex(at time.Time) *UploadIndex {
	return &UploadIndex{Created: at, Contexts: map[string]UploadContext{}}
}

func uploadBlobKey(digest string) string {
	return uploadBlobsDir + strings.TrimPrefix(digest, "sha256:") + objExt
}

func uploadIndexKey(stamp string) string { return uploadIndexDir + stamp + imageIndexExt }

func (s *Syncer) syncUploads(ctx context.Context, res *Result, fail func(string, ...any)) {
	if s.UploadsDir == "" {
		return
	}
	remote, err := s.listSizes(ctx, uploadsDir)
	if err != nil {
		fail("list %s in the bucket: %v", uploadsDir, err)
		return
	}
	versions := indexStamps(remote, uploadIndexDir)
	prev := newUploadIndex(time.Time{})
	if n := len(versions); n > 0 {
		if prev, err = LoadUploadIndex(ctx, s.Bucket, s.Key, versions[n-1]); err != nil {
			fail("read the uploads index %s from the bucket: %v", versions[n-1], err)
			return
		}
	}
	entries, err := os.ReadDir(s.UploadsDir)
	if err != nil {
		fail("read the uploads volume %s: %v", s.UploadsDir, err)
		return
	}
	next := newUploadIndex(s.now().UTC())
	// failed is set by anything that leaves next short of the volume, which
	// rules out pruning in this pass.
	failed := false
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || !uploadIDRE.MatchString(id) {
			continue
		}
		old, had := prev.Contexts[id]
		if ctx.Err() != nil {
			fail("stopped before the upload of %s: %v", id, ctx.Err())
			failed = true
			if had {
				next.Contexts[id] = old
			}
			continue
		}
		c, err := s.copyUpload(ctx, id, old, had, remote, res)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// A directory without its context: an upload still being written,
			// or one reaped between the listing and here.
		case err != nil:
			fail("copy the upload of %s: %v", id, err)
			failed = true
			if had {
				next.Contexts[id] = old
			}
		default:
			next.Contexts[id] = c
		}
	}

	stamp := ""
	if len(versions) > 0 {
		stamp = versions[len(versions)-1]
	}
	if len(versions) == 0 || !maps.EqualFunc(prev.Contexts, next.Contexts, sameContext) {
		stamp = next.Created.Format(imageStampLayout)
		raw, err := json.Marshal(next)
		if err == nil {
			err = s.putBytes(ctx, uploadIndexKey(stamp), raw)
		}
		if err != nil {
			fail("write the uploads index: %v", err)
			return
		}
		if len(versions) == 0 || versions[len(versions)-1] != stamp {
			versions = append(versions, stamp)
		}
		s.logf("recorded uploads index %s: %d contexts (%s)", stamp, len(next.Contexts), HumanBytes(next.Bytes()))
	}
	res.UploadIndex = stamp
	res.Uploads = len(next.Contexts)
	if !failed {
		s.pruneUploads(ctx, versions, next, remote, res, fail)
	}
	for key, size := range remote {
		if strings.HasPrefix(key, uploadBlobsDir) {
			res.RemoteUploadBytes += size
		}
	}
}

// copyUpload makes sure the bucket holds the context of submission id and
// returns what the index records for it. A file whose size and modification
// time match the previous index is not read again.
func (s *Syncer) copyUpload(ctx context.Context, id string, old UploadContext, had bool, remote map[string]int64, res *Result) (UploadContext, error) {
	p := filepath.Join(s.UploadsDir, id, UploadContextFile)
	fi, err := os.Lstat(p)
	if err != nil {
		return UploadContext{}, err
	}
	if !fi.Mode().IsRegular() {
		return UploadContext{}, fmt.Errorf("%s is not a regular file", p)
	}
	c := UploadContext{Size: fi.Size(), ModTime: fi.ModTime().UTC()}
	if had && old.Size == c.Size && old.ModTime.Equal(c.ModTime) && remote[uploadBlobKey(old.Digest)] == SealedSize(old.Size) {
		return old, nil
	}
	if c.Digest, err = hashFile(p); err != nil {
		return UploadContext{}, err
	}
	key := uploadBlobKey(c.Digest)
	if remote[key] == SealedSize(c.Size) {
		return c, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return UploadContext{}, err
	}
	defer f.Close()
	// Checked again on the way up: a context replaced between the hash and
	// the upload fails instead of being stored under the old digest.
	if err := s.putStream(ctx, key, newDigestReader(f, c.Digest, c.Size), c.Size); err != nil {
		return UploadContext{}, err
	}
	remote[key] = SealedSize(c.Size)
	res.UploadsUploaded++
	res.BytesUploaded += c.Size
	s.logf("copied the upload of %s (%s)", id, HumanBytes(c.Size))
	return c, nil
}

func sameContext(a, b UploadContext) bool {
	return a.Digest == b.Digest && a.Size == b.Size && a.ModTime.Equal(b.ModTime)
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// pruneUploads drops the index versions replaced more than UploadHistory ago,
// then every context no remaining version names.
func (s *Syncer) pruneUploads(ctx context.Context, versions []string, newest *UploadIndex, remote map[string]int64, res *Result, fail func(string, ...any)) {
	keep, drop := retire(versions, s.now().Add(-UploadHistory))
	live := map[string]bool{}
	mark := func(x *UploadIndex) {
		for _, c := range x.Contexts {
			live[uploadBlobKey(c.Digest)] = true
		}
	}
	mark(newest)
	for _, v := range keep {
		x, err := LoadUploadIndex(ctx, s.Bucket, s.Key, v)
		if err != nil {
			fail("read the uploads index %s from the bucket: %v; nothing pruned", v, err)
			return
		}
		mark(x)
	}
	for _, v := range drop {
		if err := s.Bucket.Remove(ctx, uploadIndexKey(v)); err != nil {
			fail("prune uploads index %s: %v", v, err)
			return
		}
		delete(remote, uploadIndexKey(v))
		res.UploadObjectsPruned++
	}
	for _, key := range slices.Sorted(maps.Keys(remote)) {
		if !strings.HasPrefix(key, uploadBlobsDir) || live[key] {
			continue
		}
		if err := s.Bucket.Remove(ctx, key); err != nil {
			fail("prune %s: %v", key, err)
			continue
		}
		delete(remote, key)
		res.UploadObjectsPruned++
	}
}

// UploadIndexes lists the uploads index versions in the bucket, oldest first.
func UploadIndexes(ctx context.Context, b Bucket) ([]string, error) {
	objs, err := b.List(ctx, uploadIndexDir)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]int64, len(objs))
	for _, o := range objs {
		keys[o.Key] = o.Size
	}
	return indexStamps(keys, uploadIndexDir), nil
}

// LoadUploadIndex reads one uploads index version.
func LoadUploadIndex(ctx context.Context, b Bucket, key []byte, stamp string) (*UploadIndex, error) {
	raw, err := getSealed(ctx, b, key, uploadIndexKey(stamp), maxUploadIndexBytes)
	if err != nil {
		return nil, err
	}
	x := newUploadIndex(time.Time{})
	if err := json.Unmarshal(raw, x); err != nil {
		return nil, fmt.Errorf("uploads index %s: %w", stamp, err)
	}
	if x.Contexts == nil {
		x.Contexts = map[string]UploadContext{}
	}
	return x, nil
}

// ChooseUploadIndex picks the version a restore uses: at when given, otherwise
// the newest. An empty newest version while an older one names contexts is
// what a rebuilt host's first sync records, so it is refused with the versions
// worth choosing instead.
func ChooseUploadIndex(ctx context.Context, b Bucket, key []byte, at string) (string, *UploadIndex, error) {
	versions, err := UploadIndexes(ctx, b)
	if err != nil {
		return "", nil, err
	}
	if len(versions) == 0 {
		return "", nil, errors.New("the bucket holds no uploads index: no sync has copied the uploads volume yet")
	}
	if at != "" {
		if !slices.Contains(versions, at) {
			return "", nil, fmt.Errorf("the bucket holds no uploads index %s; it holds %s", at, strings.Join(versions, ", "))
		}
		x, err := LoadUploadIndex(ctx, b, key, at)
		return at, x, err
	}
	newest := versions[len(versions)-1]
	x, err := LoadUploadIndex(ctx, b, key, newest)
	if err != nil || len(x.Contexts) > 0 {
		return newest, x, err
	}
	var older []string
	for i := len(versions) - 2; i >= 0; i-- {
		o, err := LoadUploadIndex(ctx, b, key, versions[i])
		if err != nil {
			return "", nil, err
		}
		if len(o.Contexts) > 0 {
			older = append(older, fmt.Sprintf("%s (%d contexts)", versions[i], len(o.Contexts)))
		}
	}
	if len(older) == 0 {
		return newest, x, nil
	}
	return "", nil, fmt.Errorf("the newest uploads index %s lists no contexts, which is what a rebuilt host records before its restore; pick the state to restore with -at: %s",
		newest, strings.Join(older, ", "))
}

// FetchUploadsResult counts what FetchUploads wrote.
type FetchUploadsResult struct {
	Written  int
	Present  int
	Bytes    int64
	Failures []string
}

// FetchUploads writes every context x names into dir as <id>/context.tar.gz,
// skipping one already there with the same content. Each is written to a
// temporary file beside its place, checked against its digest, and renamed
// into place, so a failure leaves no partial context. uid and gid own what it
// creates (-1 leaves the caller's).
func FetchUploads(ctx context.Context, b Bucket, key []byte, x *UploadIndex, dir string, uid, gid int, log io.Writer) (FetchUploadsResult, error) {
	var res FetchUploadsResult
	for _, id := range slices.Sorted(maps.Keys(x.Contexts)) {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		c := x.Contexts[id]
		if !uploadIDRE.MatchString(id) {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: not a submission id", id))
			continue
		}
		wrote, err := fetchUpload(ctx, b, key, id, c, dir, uid, gid)
		switch {
		case err != nil:
			res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", id, err))
		case wrote:
			res.Written++
			res.Bytes += c.Size
			if log != nil {
				fmt.Fprintf(log, "felis offsite: restored the upload of %s (%s)\n", id, HumanBytes(c.Size))
			}
		default:
			res.Present++
		}
	}
	if len(res.Failures) > 0 {
		return res, fmt.Errorf("%d contexts failed; first: %s", len(res.Failures), res.Failures[0])
	}
	return res, nil
}

func fetchUpload(ctx context.Context, b Bucket, key []byte, id string, c UploadContext, dir string, uid, gid int) (bool, error) {
	sub := filepath.Join(dir, id)
	final := filepath.Join(sub, UploadContextFile)
	if fi, err := os.Lstat(final); err == nil && fi.Mode().IsRegular() && fi.Size() == c.Size {
		if d, err := hashFile(final); err == nil && d == c.Digest {
			return false, nil
		}
	}
	if err := os.MkdirAll(sub, 0o770); err != nil {
		return false, err
	}
	if err := chown(sub, uid, gid); err != nil {
		return false, err
	}
	rc, err := openSealed(ctx, b, key, uploadBlobKey(c.Digest))
	if err != nil {
		return false, err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(sub, UploadContextFile+".*.restore")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, newDigestReader(rc, c.Digest, c.Size))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o660)
	}
	if err == nil {
		err = chown(tmp.Name(), uid, gid)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), final)
	}
	return err == nil, err
}

func chown(p string, uid, gid int) error {
	if uid < 0 && gid < 0 {
		return nil
	}
	return os.Lchown(p, uid, gid)
}
