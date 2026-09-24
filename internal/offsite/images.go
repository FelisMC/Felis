package offsite

// The off-site copy of the platform registry's user images. The installer
// pushes everything under felis/ and mirror/ again on any host, so those are
// left out; every other repository holds builds that exist nowhere else: a
// lost registry volume would otherwise leave each server pinned to one of them
// in ImagePullBackOff until someone rebuilds it.
//
// The bucket holds each blob and manifest once, by digest, encrypted like
// everything else, plus an index that says which repository holds which
// manifests under which tags. A new index version is written whenever that
// changes, and every version stays until ImageHistory after a newer one
// replaced it, together with every object it names. So a registry that comes
// back empty, a host being rebuilt whose timer runs before anyone restores
// anything, only adds a new version: the one before it is still there to
// restore from (FetchImages, `felis offsite fetch-images -at`).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"felis.lolicon.best/internal/registrygate"
)

const (
	registryDir       = "registry/"
	imageBlobsDir     = registryDir + "blobs/"
	imageManifestsDir = registryDir + "manifests/"
	imageIndexDir     = registryDir + "index/"
	imageIndexExt     = ".json" + objExt
	imageStampLayout  = "20060102T150405Z"

	maxImageIndexBytes    = 64 << 20
	maxImageManifestBytes = 4 << 20
)

// ImageHistory is how long a registry state stays restorable after a newer one
// replaced it.
const ImageHistory = 14 * 24 * time.Hour

// ErrImageGone is a manifest or blob the registry lists but no longer holds.
var ErrImageGone = errors.New("offsite: not in the registry")

// ImageSource is the registry the sync reads, anonymously.
type ImageSource interface {
	Repositories(ctx context.Context) ([]string, error)
	// Revisions lists every manifest repo holds, tagged or not, and its tags.
	Revisions(ctx context.Context, repo string) (digests []string, tags map[string]string, err error)
	// Manifest returns the manifest stored under digest and its media type.
	// A manifest the registry no longer holds is ErrImageGone.
	Manifest(ctx context.Context, repo, digest string) ([]byte, string, error)
	// Blob opens a blob; one the registry no longer holds is ErrImageGone.
	Blob(ctx context.Context, repo, digest string) (io.ReadCloser, error)
}

// ImageTarget is the registry FetchImages pushes into.
type ImageTarget interface {
	// PutBlob uploads the blob open returns unless repo already holds it.
	PutBlob(ctx context.Context, repo, digest string, size int64, open func() (io.ReadCloser, error)) error
	// PutManifest stores body under reference, a tag or its digest.
	PutManifest(ctx context.Context, repo, reference, mediaType string, body []byte) error
}

// ImageIndex is one state of the registry's user images.
type ImageIndex struct {
	Created      time.Time                `json:"created"`
	Repositories map[string]ImageRepo     `json:"repositories"`
	Manifests    map[string]ImageManifest `json:"manifests"`
}

// ImageRepo is one repository: its manifests, and the tag → digest map.
type ImageRepo struct {
	Manifests []string          `json:"manifests"`
	Tags      map[string]string `json:"tags,omitempty"`
}

// ImageManifest describes a stored manifest: the blobs an image manifest names
// (config and layers), or the manifests an index names.
type ImageManifest struct {
	MediaType string      `json:"media_type"`
	Size      int64       `json:"size"`
	Blobs     []ImageBlob `json:"blobs,omitempty"`
	Children  []string    `json:"children,omitempty"`
}

// ImageBlob is one blob a manifest names.
type ImageBlob struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Images counts the manifests the repositories hold, a manifest in two
// repositories counting twice, like two images.
func (x *ImageIndex) Images() int {
	n := 0
	for _, r := range x.Repositories {
		n += len(r.Manifests)
	}
	return n
}

func newImageIndex(at time.Time) *ImageIndex {
	return &ImageIndex{Created: at, Repositories: map[string]ImageRepo{}, Manifests: map[string]ImageManifest{}}
}

var imageDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func imageBlobKey(digest string) string {
	return imageBlobsDir + strings.TrimPrefix(digest, "sha256:") + objExt
}

func imageManifestKey(digest string) string {
	return imageManifestsDir + strings.TrimPrefix(digest, "sha256:") + objExt
}

func imageIndexKey(stamp string) string { return imageIndexDir + stamp + imageIndexExt }

func reservedRepo(repo string) bool {
	root, _, _ := strings.Cut(repo, "/")
	return slices.Contains(registrygate.ReservedRepoRoots, root)
}

// Manifest media types the copy understands.
const (
	mediaOCIIndex        = "application/vnd.oci.image.index.v1+json"
	mediaDockerList      = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaOCIManifest     = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerManifest2 = "application/vnd.docker.distribution.manifest.v2+json"
)

// describeManifest reads the blobs or child manifests out of a manifest.
func describeManifest(body []byte, mediaType string) (ImageManifest, error) {
	type desc struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	}
	var m struct {
		MediaType string `json:"mediaType"`
		Config    *desc  `json:"config"`
		Layers    []desc `json:"layers"`
		Manifests []desc `json:"manifests"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return ImageManifest{}, fmt.Errorf("manifest: %w", err)
	}
	if mediaType == "" {
		mediaType = m.MediaType
	}
	im := ImageManifest{MediaType: mediaType, Size: int64(len(body))}
	check := func(d desc) error {
		if !imageDigestRE.MatchString(d.Digest) || d.Size < 0 {
			return fmt.Errorf("manifest names %q (size %d), not a sha256 digest", d.Digest, d.Size)
		}
		return nil
	}
	switch mediaType {
	case mediaOCIIndex, mediaDockerList:
		for _, d := range m.Manifests {
			if err := check(d); err != nil {
				return ImageManifest{}, err
			}
			if !slices.Contains(im.Children, d.Digest) {
				im.Children = append(im.Children, d.Digest)
			}
		}
	case mediaOCIManifest, mediaDockerManifest2:
		if m.Config == nil {
			return ImageManifest{}, errors.New("image manifest without a config")
		}
		for _, d := range append([]desc{*m.Config}, m.Layers...) {
			if err := check(d); err != nil {
				return ImageManifest{}, err
			}
			if !slices.ContainsFunc(im.Blobs, func(b ImageBlob) bool { return b.Digest == d.Digest }) {
				im.Blobs = append(im.Blobs, ImageBlob{Digest: d.Digest, Size: d.Size})
			}
		}
	default:
		return ImageManifest{}, fmt.Errorf("unsupported manifest type %q", mediaType)
	}
	return im, nil
}

// imageCopy is one sync pass over the registry.
type imageCopy struct {
	s      *Syncer
	remote map[string]int64
	prev   *ImageIndex
	next   *ImageIndex
	res    *Result
	fail   func(string, ...any)
	// failed is set by anything that leaves the new index short of the
	// registry, which rules out pruning in this pass.
	failed bool
}

// errSkipped marks a manifest left out without failing the pass.
var errSkipped = errors.New("skipped")

func (s *Syncer) syncImages(ctx context.Context, res *Result, fail func(string, ...any)) {
	if s.Images == nil {
		return
	}
	remote, err := s.listSizes(ctx, registryDir)
	if err != nil {
		fail("list %s in the bucket: %v", registryDir, err)
		return
	}
	versions := imageIndexStamps(remote)
	prev := newImageIndex(time.Time{})
	if n := len(versions); n > 0 {
		if prev, err = LoadImageIndex(ctx, s.Bucket, s.Key, versions[n-1]); err != nil {
			fail("read the registry index %s from the bucket: %v", versions[n-1], err)
			return
		}
	}
	c := &imageCopy{s: s, remote: remote, prev: prev, next: newImageIndex(s.now().UTC()), res: res, fail: fail}
	repos, err := s.Images.Repositories(ctx)
	if err != nil {
		fail("list the registry's repositories: %v", err)
		return
	}
	sort.Strings(repos)
	listed := map[string]bool{}
	for _, repo := range repos {
		if reservedRepo(repo) {
			continue
		}
		listed[repo] = true
		if ctx.Err() != nil {
			c.fail("stopped before %s: %v", repo, ctx.Err())
			c.failed = true
			c.carry(repo)
			continue
		}
		c.repo(ctx, repo)
	}

	stamp := ""
	if len(versions) > 0 {
		stamp = versions[len(versions)-1]
	}
	if len(versions) == 0 || !sameImages(prev, c.next) {
		stamp = c.next.Created.Format(imageStampLayout)
		if err := s.putImageIndex(ctx, stamp, c.next); err != nil {
			fail("write the registry index: %v", err)
			return
		}
		if len(versions) == 0 || versions[len(versions)-1] != stamp {
			versions = append(versions, stamp)
		}
		s.logf("recorded registry index %s: %d repositories, %d images", stamp, len(c.next.Repositories), c.next.Images())
	}
	res.ImageIndex = stamp
	res.ImageRepos = len(c.next.Repositories)
	res.Images = c.next.Images()
	if !c.failed {
		s.pruneImages(ctx, versions, c.next, remote, res, fail)
	}
	for key, size := range remote {
		if strings.HasPrefix(key, imageBlobsDir) || strings.HasPrefix(key, imageManifestsDir) {
			res.RemoteImageBytes += size
		}
	}
}

// repo copies one repository into c.next. A repository that cannot be read in
// full keeps the entry the previous index had for it.
func (c *imageCopy) repo(ctx context.Context, repo string) {
	digests, tags, err := c.s.Images.Revisions(ctx, repo)
	if err != nil {
		c.fail("list the images of %s: %v", repo, err)
		c.failed = true
		c.carry(repo)
		return
	}
	entry := ImageRepo{Tags: map[string]string{}}
	short := false
	for _, d := range digests {
		switch err := c.manifest(ctx, repo, d); {
		case errors.Is(err, errSkipped):
		case err != nil:
			c.fail("copy %s@%s: %v", repo, d, err)
			short = true
		default:
			entry.Manifests = append(entry.Manifests, d)
		}
	}
	if short {
		c.failed = true
		if _, ok := c.prev.Repositories[repo]; ok {
			c.carry(repo)
			return
		}
	}
	sort.Strings(entry.Manifests)
	for tag, d := range tags {
		if slices.Contains(entry.Manifests, d) {
			entry.Tags[tag] = d
		}
	}
	if len(entry.Manifests) > 0 {
		c.next.Repositories[repo] = entry
	}
}

// carry keeps the previous index's entry for repo, and what it names.
func (c *imageCopy) carry(repo string) {
	entry, ok := c.prev.Repositories[repo]
	if !ok {
		return
	}
	c.next.Repositories[repo] = entry
	var add func(d string)
	add = func(d string) {
		m, ok := c.prev.Manifests[d]
		if !ok {
			return
		}
		c.next.Manifests[d] = m
		for _, child := range m.Children {
			add(child)
		}
	}
	for _, d := range entry.Manifests {
		add(d)
	}
}

// manifest makes sure digest, everything it names, and then the manifest
// itself are in the bucket, and records it in c.next. A manifest or blob the
// registry lost is errSkipped, unless an earlier pass already copied the
// manifest whole: that copy is then the only complete one and stays.
func (c *imageCopy) manifest(ctx context.Context, repo, digest string) error {
	if _, ok := c.next.Manifests[digest]; ok {
		return nil
	}
	if !imageDigestRE.MatchString(digest) {
		return fmt.Errorf("%q is not a sha256 digest", digest)
	}
	key := imageManifestKey(digest)
	im, stored := c.prev.Manifests[digest]
	stored = stored && c.remote[key] == SealedSize(im.Size)
	var body []byte
	if !stored {
		b, mt, err := c.s.Images.Manifest(ctx, repo, digest)
		if err != nil {
			return c.gone(repo, digest, err)
		}
		if im, err = describeManifest(b, mt); err != nil {
			return err
		}
		body = b
	}
	for _, child := range im.Children {
		if err := c.manifest(ctx, repo, child); err != nil {
			if errors.Is(err, errSkipped) {
				return c.gone(repo, digest, fmt.Errorf("%w: child manifest %s", ErrImageGone, child))
			}
			return fmt.Errorf("child manifest %s: %w", child, err)
		}
	}
	for _, b := range im.Blobs {
		bkey := imageBlobKey(b.Digest)
		if c.remote[bkey] == SealedSize(b.Size) {
			continue
		}
		if err := c.s.putImageBlob(ctx, repo, b); err != nil {
			return c.gone(repo, digest, fmt.Errorf("blob %s: %w", b.Digest, err))
		}
		c.remote[bkey] = SealedSize(b.Size)
		c.res.ImageBlobsUploaded++
		c.res.BytesUploaded += b.Size
	}
	if body != nil {
		if err := c.s.putBytes(ctx, key, body); err != nil {
			return err
		}
		c.remote[key] = SealedSize(int64(len(body)))
		c.res.ImagesUploaded++
		c.s.logf("copied image %s@%s", repo, digest)
	}
	c.next.Manifests[digest] = im
	return nil
}

// gone turns err into errSkipped when it says the registry lost the manifest
// or one of its blobs, keeping the copy an earlier pass made if there is one.
func (c *imageCopy) gone(repo, digest string, err error) error {
	if !errors.Is(err, ErrImageGone) {
		return err
	}
	if _, ok := c.prev.Manifests[digest]; ok && c.prevComplete(digest) {
		c.res.ImagesIncomplete = append(c.res.ImagesIncomplete, fmt.Sprintf("%s@%s: %v; kept the off-site copy", repo, digest, err))
		c.addPrev(digest)
		return nil
	}
	c.res.ImagesIncomplete = append(c.res.ImagesIncomplete, fmt.Sprintf("%s@%s: %v; not copied", repo, digest, err))
	return errSkipped
}

// prevComplete reports whether every object digest needs is in the bucket.
func (c *imageCopy) prevComplete(digest string) bool {
	m, ok := c.prev.Manifests[digest]
	if !ok || c.remote[imageManifestKey(digest)] != SealedSize(m.Size) {
		return false
	}
	for _, b := range m.Blobs {
		if c.remote[imageBlobKey(b.Digest)] != SealedSize(b.Size) {
			return false
		}
	}
	for _, child := range m.Children {
		if !c.prevComplete(child) {
			return false
		}
	}
	return true
}

func (c *imageCopy) addPrev(digest string) {
	m := c.prev.Manifests[digest]
	c.next.Manifests[digest] = m
	for _, child := range m.Children {
		c.addPrev(child)
	}
}

// sameImages reports whether two indexes record the same state.
func sameImages(a, b *ImageIndex) bool {
	ja, err1 := json.Marshal(struct {
		R map[string]ImageRepo
		M map[string]ImageManifest
	}{a.Repositories, a.Manifests})
	jb, err2 := json.Marshal(struct {
		R map[string]ImageRepo
		M map[string]ImageManifest
	}{b.Repositories, b.Manifests})
	return err1 == nil && err2 == nil && bytes.Equal(ja, jb)
}

// pruneImages drops the index versions replaced more than ImageHistory ago,
// then every blob and manifest no remaining version names.
func (s *Syncer) pruneImages(ctx context.Context, versions []string, newest *ImageIndex, remote map[string]int64, res *Result, fail func(string, ...any)) {
	cutoff := s.now().Add(-ImageHistory)
	var keep, drop []string
	for i, v := range versions {
		if i == len(versions)-1 {
			break
		}
		replaced, err := time.Parse(imageStampLayout, versions[i+1])
		if err == nil && replaced.Before(cutoff) {
			drop = append(drop, v)
		} else {
			keep = append(keep, v)
		}
	}
	live := map[string]bool{}
	mark := func(x *ImageIndex) {
		for d, m := range x.Manifests {
			live[imageManifestKey(d)] = true
			for _, b := range m.Blobs {
				live[imageBlobKey(b.Digest)] = true
			}
		}
	}
	mark(newest)
	for _, v := range keep {
		x, err := LoadImageIndex(ctx, s.Bucket, s.Key, v)
		if err != nil {
			fail("read the registry index %s from the bucket: %v; nothing pruned", v, err)
			return
		}
		mark(x)
	}
	for _, v := range drop {
		if err := s.Bucket.Remove(ctx, imageIndexKey(v)); err != nil {
			fail("prune registry index %s: %v", v, err)
			return
		}
		delete(remote, imageIndexKey(v))
		res.ImageObjectsPruned++
	}
	for _, key := range slices.Sorted(maps.Keys(remote)) {
		if !strings.HasPrefix(key, imageBlobsDir) && !strings.HasPrefix(key, imageManifestsDir) {
			continue
		}
		if live[key] {
			continue
		}
		if err := s.Bucket.Remove(ctx, key); err != nil {
			fail("prune %s: %v", key, err)
			continue
		}
		delete(remote, key)
		res.ImageObjectsPruned++
	}
}

// putImageBlob streams one blob from the registry into the bucket, checking it
// against its digest and size on the way: a blob that does not match fails
// the upload instead of storing bytes a restore would push as that digest.
func (s *Syncer) putImageBlob(ctx context.Context, repo string, b ImageBlob) error {
	rc, err := s.Images.Blob(ctx, repo, b.Digest)
	if err != nil {
		return err
	}
	defer rc.Close()
	return s.putStream(ctx, imageBlobKey(b.Digest), newDigestReader(rc, b.Digest, b.Size), b.Size)
}

func (s *Syncer) putBytes(ctx context.Context, key string, plain []byte) error {
	var sealed bytes.Buffer
	if err := Encrypt(&sealed, bytes.NewReader(plain), s.Key); err != nil {
		return err
	}
	return s.Bucket.Put(ctx, key, &sealed, int64(sealed.Len()))
}

func (s *Syncer) putImageIndex(ctx context.Context, stamp string, x *ImageIndex) error {
	raw, err := json.Marshal(x)
	if err != nil {
		return err
	}
	return s.putBytes(ctx, imageIndexKey(stamp), raw)
}

// digestReader passes a blob through and fails at its end when the bytes do not
// hash to digest or are not size long.
type digestReader struct {
	r      io.Reader
	h      hash.Hash
	n      int64
	size   int64
	digest string
}

func newDigestReader(r io.Reader, digest string, size int64) *digestReader {
	return &digestReader{r: r, h: sha256.New(), size: size, digest: digest}
}

func (d *digestReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	d.h.Write(p[:n])
	d.n += int64(n)
	if d.n > d.size {
		return n, fmt.Errorf("blob %s is longer than the %d bytes its manifest records", d.digest, d.size)
	}
	if err == io.EOF {
		if d.n != d.size {
			return n, fmt.Errorf("blob %s ended after %d of %d bytes", d.digest, d.n, d.size)
		}
		if got := "sha256:" + hex.EncodeToString(d.h.Sum(nil)); got != d.digest {
			return n, fmt.Errorf("blob %s hashes to %s", d.digest, got)
		}
	}
	return n, err
}

// imageIndexStamps lists the index versions among keys, oldest first.
func imageIndexStamps(keys map[string]int64) []string {
	var out []string
	for key := range keys {
		stamp, ok := strings.CutPrefix(key, imageIndexDir)
		if !ok {
			continue
		}
		stamp, ok = strings.CutSuffix(stamp, imageIndexExt)
		if _, err := time.Parse(imageStampLayout, stamp); ok && err == nil {
			out = append(out, stamp)
		}
	}
	sort.Strings(out)
	return out
}

// ImageIndexes lists the registry index versions in the bucket, oldest first.
func ImageIndexes(ctx context.Context, b Bucket) ([]string, error) {
	objs, err := b.List(ctx, imageIndexDir)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]int64, len(objs))
	for _, o := range objs {
		keys[o.Key] = o.Size
	}
	return imageIndexStamps(keys), nil
}

// LoadImageIndex reads one index version.
func LoadImageIndex(ctx context.Context, b Bucket, key []byte, stamp string) (*ImageIndex, error) {
	raw, err := getSealed(ctx, b, key, imageIndexKey(stamp), maxImageIndexBytes)
	if err != nil {
		return nil, err
	}
	x := newImageIndex(time.Time{})
	if err := json.Unmarshal(raw, x); err != nil {
		return nil, fmt.Errorf("registry index %s: %w", stamp, err)
	}
	if x.Repositories == nil {
		x.Repositories = map[string]ImageRepo{}
	}
	if x.Manifests == nil {
		x.Manifests = map[string]ImageManifest{}
	}
	return x, nil
}

// ChooseImageIndex picks the version a restore uses: at when given, otherwise
// the newest. A newest version with no images while an older one has some is
// what a rebuilt host's first sync records before the restore, so it is
// refused with the versions worth choosing instead.
func ChooseImageIndex(ctx context.Context, b Bucket, key []byte, at string) (string, *ImageIndex, error) {
	versions, err := ImageIndexes(ctx, b)
	if err != nil {
		return "", nil, err
	}
	if len(versions) == 0 {
		return "", nil, errors.New("the bucket holds no registry index: no sync has copied images yet")
	}
	if at != "" {
		if !slices.Contains(versions, at) {
			return "", nil, fmt.Errorf("the bucket holds no registry index %s; it holds %s", at, strings.Join(versions, ", "))
		}
		x, err := LoadImageIndex(ctx, b, key, at)
		return at, x, err
	}
	newest := versions[len(versions)-1]
	x, err := LoadImageIndex(ctx, b, key, newest)
	if err != nil || len(x.Repositories) > 0 {
		return newest, x, err
	}
	var older []string
	for i := len(versions) - 2; i >= 0; i-- {
		o, err := LoadImageIndex(ctx, b, key, versions[i])
		if err != nil {
			return "", nil, err
		}
		if len(o.Repositories) > 0 {
			older = append(older, fmt.Sprintf("%s (%d repositories, %d images)", versions[i], len(o.Repositories), o.Images()))
		}
	}
	if len(older) == 0 {
		return newest, x, nil
	}
	return "", nil, fmt.Errorf("the newest registry index %s lists no images, which is what a rebuilt host records before its restore; pick the state to restore with -at: %s",
		newest, strings.Join(older, ", "))
}

// getSealed downloads and decrypts a small object.
func getSealed(ctx context.Context, b Bucket, key []byte, objKey string, limit int64) ([]byte, error) {
	rc, err := b.Get(ctx, objKey)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var out bytes.Buffer
	if err := Decrypt(&limitWriter{w: &out, left: limit}, rc, key); err != nil {
		return nil, fmt.Errorf("%s: %w", objKey, err)
	}
	return out.Bytes(), nil
}

type limitWriter struct {
	w    io.Writer
	left int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		return 0, errors.New("object is larger than expected")
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}

// FetchImagesResult is what FetchImages did.
type FetchImagesResult struct {
	Repositories int
	Manifests    int
	Tags         int
	// BlobsPushed and BytesPushed count the blobs the registry lacked.
	BlobsPushed int
	BytesPushed int64
	Failures    []string
}

// FetchImages pushes every image x records back into t, each repository's
// blobs first, then its manifests by digest (an index after the manifests it
// names), then its tags. What t already holds is skipped, so a second run
// finishes what the first could not.
func FetchImages(ctx context.Context, b Bucket, key []byte, x *ImageIndex, t ImageTarget, log io.Writer) (FetchImagesResult, error) {
	var res FetchImagesResult
	bodies := map[string][]byte{}
	body := func(d string) ([]byte, error) {
		if raw, ok := bodies[d]; ok {
			return raw, nil
		}
		raw, err := getSealed(ctx, b, key, imageManifestKey(d), maxImageManifestBytes)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		if got := "sha256:" + hex.EncodeToString(sum[:]); got != d {
			return nil, fmt.Errorf("the stored manifest %s hashes to %s", d, got)
		}
		bodies[d] = raw
		return raw, nil
	}
	pushedBlobs := map[string]bool{}
	for _, repo := range slices.Sorted(maps.Keys(x.Repositories)) {
		entry := x.Repositories[repo]
		done := map[string]bool{}
		var push func(d string) error
		push = func(d string) error {
			if done[d] {
				return nil
			}
			m, ok := x.Manifests[d]
			if !ok {
				return fmt.Errorf("the index names %s but does not describe it", d)
			}
			for _, child := range m.Children {
				if err := push(child); err != nil {
					return fmt.Errorf("child %s: %w", child, err)
				}
			}
			for _, bl := range m.Blobs {
				open := func() (io.ReadCloser, error) {
					if !pushedBlobs[repo+"@"+bl.Digest] {
						pushedBlobs[repo+"@"+bl.Digest] = true
						res.BlobsPushed++
						res.BytesPushed += bl.Size
					}
					return openSealed(ctx, b, key, imageBlobKey(bl.Digest))
				}
				if err := t.PutBlob(ctx, repo, bl.Digest, bl.Size, open); err != nil {
					return fmt.Errorf("blob %s: %w", bl.Digest, err)
				}
			}
			raw, err := body(d)
			if err != nil {
				return err
			}
			if err := t.PutManifest(ctx, repo, d, m.MediaType, raw); err != nil {
				return err
			}
			done[d] = true
			return nil
		}
		ok := true
		for _, d := range entry.Manifests {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if err := push(d); err != nil {
				res.Failures = append(res.Failures, fmt.Sprintf("%s@%s: %v", repo, d, err))
				ok = false
				continue
			}
			res.Manifests++
		}
		for _, tag := range slices.Sorted(maps.Keys(entry.Tags)) {
			d := entry.Tags[tag]
			if !done[d] {
				continue
			}
			if err := t.PutManifest(ctx, repo, tag, x.Manifests[d].MediaType, bodies[d]); err != nil {
				res.Failures = append(res.Failures, fmt.Sprintf("%s:%s: %v", repo, tag, err))
				ok = false
				continue
			}
			res.Tags++
		}
		if ok {
			res.Repositories++
			if log != nil {
				fmt.Fprintf(log, "felis offsite: restored %s (%d images, %d tags)\n", repo, len(entry.Manifests), len(entry.Tags))
			}
		}
	}
	if len(res.Failures) > 0 {
		return res, fmt.Errorf("%d images or tags failed; first: %s", len(res.Failures), res.Failures[0])
	}
	return res, nil
}

// openSealed streams the decrypted content of objKey. Decryption authenticates
// every segment; the registry checks the whole against its digest.
func openSealed(ctx context.Context, b Bucket, key []byte, objKey string) (io.ReadCloser, error) {
	rc, err := b.Get(ctx, objKey)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		err := Decrypt(pw, rc, key)
		rc.Close()
		pw.CloseWithError(err)
	}()
	return pr, nil
}
