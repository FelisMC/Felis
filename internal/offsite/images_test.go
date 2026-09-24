package offsite

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// ---- fake registry ---------------------------------------------------------

type fakeRepo struct {
	digests []string
	tags    map[string]string
}

type fakeManifest struct {
	body      []byte
	mediaType string
}

// fakeRegistry is the registry as the sync reads it and as a restore writes
// it: one blob and manifest store shared by every repository, as
// distribution keeps it, and per-repository links.
type fakeRegistry struct {
	repos     map[string]*fakeRepo
	blobs     map[string][]byte
	manifests map[string]fakeManifest
	// linked records which blobs each repository holds, for a restore.
	linked   map[string]map[string]bool
	revErr   map[string]error
	gone     map[string]bool
	corrupt  map[string]bool
	blobGets int
	// log is what a restore did, in order.
	log []string
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		repos: map[string]*fakeRepo{}, blobs: map[string][]byte{}, manifests: map[string]fakeManifest{},
		linked: map[string]map[string]bool{}, revErr: map[string]error{}, gone: map[string]bool{}, corrupt: map[string]bool{},
	}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *fakeRegistry) blob(data []byte) map[string]any {
	d := digestOf(data)
	r.blobs[d] = data
	return map[string]any{"mediaType": "application/octet-stream", "digest": d, "size": len(data)}
}

func (r *fakeRegistry) link(repo, digest string, tags ...string) {
	rp := r.repos[repo]
	if rp == nil {
		rp = &fakeRepo{tags: map[string]string{}}
		r.repos[repo] = rp
	}
	if !slices.Contains(rp.digests, digest) {
		rp.digests = append(rp.digests, digest)
	}
	for _, t := range tags {
		rp.tags[t] = digest
	}
}

func (r *fakeRegistry) putManifest(mediaType string, m map[string]any) string {
	m["schemaVersion"] = 2
	m["mediaType"] = mediaType
	body, _ := json.Marshal(m)
	d := digestOf(body)
	r.manifests[d] = fakeManifest{body: body, mediaType: mediaType}
	return d
}

// image stores an image manifest of the given layers in repo and returns its digest.
func (r *fakeRegistry) image(repo, tag string, layers ...[]byte) string {
	var ls []any
	for _, l := range layers {
		ls = append(ls, r.blob(l))
	}
	cfg := r.blob([]byte(fmt.Sprintf(`{"architecture":"arm64","repo":%q,"tag":%q}`, repo, tag)))
	d := r.putManifest(mediaDockerManifest2, map[string]any{"config": cfg, "layers": ls})
	r.link(repo, d, tag)
	return d
}

// index stores an image index over children in repo.
func (r *fakeRegistry) index(repo, tag string, children ...string) string {
	var ms []any
	for _, c := range children {
		ms = append(ms, map[string]any{"mediaType": mediaOCIManifest, "digest": c, "size": len(r.manifests[c].body)})
	}
	d := r.putManifest(mediaOCIIndex, map[string]any{"manifests": ms})
	r.link(repo, d, tag)
	return d
}

func (r *fakeRegistry) Repositories(context.Context) ([]string, error) {
	var out []string
	for name := range r.repos {
		out = append(out, name)
	}
	return out, nil
}

func (r *fakeRegistry) Revisions(_ context.Context, repo string) ([]string, map[string]string, error) {
	if err := r.revErr[repo]; err != nil {
		return nil, nil, err
	}
	rp := r.repos[repo]
	return slices.Clone(rp.digests), rp.tags, nil
}

func (r *fakeRegistry) Manifest(_ context.Context, repo, digest string) ([]byte, string, error) {
	m, ok := r.manifests[digest]
	if !ok || r.gone[digest] {
		return nil, "", fmt.Errorf("%w: manifest %s", ErrImageGone, digest)
	}
	return m.body, m.mediaType, nil
}

func (r *fakeRegistry) Blob(_ context.Context, repo, digest string) (io.ReadCloser, error) {
	r.blobGets++
	b, ok := r.blobs[digest]
	if !ok || r.gone[digest] {
		return nil, fmt.Errorf("%w: blob %s", ErrImageGone, digest)
	}
	if r.corrupt[digest] {
		b = append(bytes.Clone(b), 'x')
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (r *fakeRegistry) PutBlob(_ context.Context, repo, digest string, size int64, open func() (io.ReadCloser, error)) error {
	if r.linked[repo][digest] {
		return nil
	}
	rc, err := open()
	if err != nil {
		return err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	if int64(len(data)) != size || digestOf(data) != digest {
		return fmt.Errorf("blob upload does not match %s", digest)
	}
	r.blobs[digest] = data
	if r.linked[repo] == nil {
		r.linked[repo] = map[string]bool{}
	}
	r.linked[repo][digest] = true
	r.log = append(r.log, "blob "+repo+" "+digest)
	return nil
}

func (r *fakeRegistry) PutManifest(_ context.Context, repo, reference, mediaType string, body []byte) error {
	d := digestOf(body)
	if strings.HasPrefix(reference, "sha256:") && reference != d {
		return fmt.Errorf("manifest %s hashes to %s", reference, d)
	}
	// Like distribution: everything a manifest names must be in the repository.
	im, err := describeManifest(body, mediaType)
	if err != nil {
		return err
	}
	for _, b := range im.Blobs {
		if !r.linked[repo][b.Digest] {
			return fmt.Errorf("blob unknown: %s", b.Digest)
		}
	}
	for _, c := range im.Children {
		if r.repos[repo] == nil || !slices.Contains(r.repos[repo].digests, c) {
			return fmt.Errorf("manifest unknown: %s", c)
		}
	}
	r.manifests[d] = fakeManifest{body: body, mediaType: mediaType}
	if strings.HasPrefix(reference, "sha256:") {
		r.link(repo, d)
	} else {
		r.link(repo, d, reference)
	}
	r.log = append(r.log, "manifest "+repo+" "+reference)
	return nil
}

func layer(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func newImageSyncer(t *testing.T, reg *fakeRegistry) (*Syncer, *memBucket, *time.Time) {
	t.Helper()
	b := newMemBucket()
	clock := now
	return &Syncer{
		Bucket: b, Catalog: &fakeCatalog{}, Key: testKey(t), Images: reg,
		Now: func() time.Time { return clock },
	}, b, &clock
}

func keysUnder(b *memBucket, prefix string) []string {
	objs, _ := b.List(context.Background(), prefix)
	var out []string
	for _, o := range objs {
		out = append(out, o.Key)
	}
	return out
}

// ---- tests -----------------------------------------------------------------

// TestSyncImagesCopiesUserImages: user repositories are copied blob by blob,
// each blob once however many images share it, and the platform's reserved
// repositories are left alone. A second run with nothing new sends nothing.
func TestSyncImagesCopiesUserImages(t *testing.T) {
	reg := newFakeRegistry()
	shared := layer(3000)
	a := reg.image("user-uploads/sub-a", "latest", shared, layer(70000))
	child := reg.image("e2e/multi", "arm64", shared)
	idx := reg.index("e2e/multi", "latest", child)
	reg.image("felis/felis", "v1", layer(5000))
	reg.image("mirror/trivy-db", "2", layer(5000))

	s, b, _ := newImageSyncer(t, reg)
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ImageRepos != 2 || res.Images != 3 || res.ImagesUploaded != 3 {
		t.Fatalf("result = %+v, want 2 repositories, 3 images, 3 uploaded", res)
	}
	// shared layer once, one unique layer, two configs
	if got := keysUnder(b, imageBlobsDir); len(got) != 4 || res.ImageBlobsUploaded != 4 {
		t.Fatalf("blobs in bucket = %v (uploaded %d), want 4", got, res.ImageBlobsUploaded)
	}
	for _, d := range []string{a, child, idx} {
		if _, ok := b.objs[imageManifestKey(d)]; !ok {
			t.Errorf("manifest %s not in the bucket", d)
		}
	}
	x, err := LoadImageIndex(context.Background(), b, s.Key, res.ImageIndex)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := x.Repositories["felis/felis"]; ok {
		t.Fatal("reserved repository felis/felis was copied")
	}
	if got := x.Repositories["e2e/multi"]; got.Tags["latest"] != idx || got.Tags["arm64"] != child {
		t.Fatalf("e2e/multi tags = %v", got.Tags)
	}

	puts, gets := b.puts, reg.blobGets
	res, err = s.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if b.puts != puts || reg.blobGets != gets {
		t.Fatalf("second run sent %d objects and read %d blobs, want none", b.puts-puts, reg.blobGets-gets)
	}
	if len(keysUnder(b, imageIndexDir)) != 1 {
		t.Fatalf("an unchanged registry wrote another index: %v", keysUnder(b, imageIndexDir))
	}
}

// TestSyncImagesCopiesPinnedPlatformRevisions: of felis/ and mirror/ only the
// revisions a pin names are copied, without tags; a restore puts them back by
// digest and leaves the tags the installer pushed on the new host alone. When
// the pins cannot be read, the previous copy of those revisions is kept and
// the run fails.
func TestSyncImagesCopiesPinnedPlatformRevisions(t *testing.T) {
	reg := newFakeRegistry()
	old := reg.image("felis/paper", "demo", layer(4000))
	current := reg.image("felis/paper", "demo", layer(4000))
	reg.image("mirror/trivy", "1", layer(2000))
	user := reg.image("user/a", "v1", layer(1000))

	s, b, clock := newImageSyncer(t, reg)
	s.ImagePins = func(context.Context) (map[string][]string, error) {
		return map[string][]string{"felis/paper": {old}, "user/a": {user}}, nil
	}
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	x, err := LoadImageIndex(context.Background(), b, s.Key, res.ImageIndex)
	if err != nil {
		t.Fatal(err)
	}
	paper, ok := x.Repositories["felis/paper"]
	if !ok || !slices.Equal(paper.Manifests, []string{old}) || len(paper.Tags) != 0 {
		t.Fatalf("felis/paper = %+v, want only the pinned %s and no tags", paper, old)
	}
	if _, ok := x.Repositories["mirror/trivy"]; ok {
		t.Fatal("unpinned mirror/trivy was copied")
	}
	if got := x.Repositories["user/a"]; got.Tags["v1"] != user {
		t.Fatalf("user/a = %+v", got)
	}

	fresh := newFakeRegistry()
	rebuilt := fresh.image("felis/paper", "demo", layer(4000))
	if _, err := FetchImages(context.Background(), b, s.Key, x, fresh, nil); err != nil {
		t.Fatalf("FetchImages: %v", err)
	}
	if got := fresh.repos["felis/paper"]; got.tags["demo"] != rebuilt || !slices.Contains(got.digests, old) {
		t.Fatalf("restored felis/paper: tags %v digests %v, want demo=%s and %s present", got.tags, got.digests, rebuilt, old)
	}
	if slices.Contains(fresh.repos["felis/paper"].digests, current) {
		t.Fatal("the unpinned revision was restored")
	}

	*clock = clock.Add(time.Hour)
	s.ImagePins = func(context.Context) (map[string][]string, error) { return nil, errors.New("cluster down") }
	res, err = s.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded without the pins")
	}
	x, err = LoadImageIndex(context.Background(), b, s.Key, res.ImageIndex)
	if err != nil {
		t.Fatal(err)
	}
	if got := x.Repositories["felis/paper"]; !slices.Equal(got.Manifests, []string{old}) {
		t.Fatalf("without the pins felis/paper = %+v, want the previous copy kept", got)
	}
}

// TestSyncImagesRejectsCorruptBlob: bytes that do not hash to the digest never
// become that digest's object, the image stays out of the index, and the run
// fails so the next one tries again.
func TestSyncImagesRejectsCorruptBlob(t *testing.T) {
	reg := newFakeRegistry()
	bad := layer(100)
	reg.image("user/a", "v1", bad)
	reg.corrupt[digestOf(bad)] = true

	s, b, _ := newImageSyncer(t, reg)
	res, err := s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("Run = %v, want the corrupt blob to fail it", err)
	}
	if _, ok := b.objs[imageBlobKey(digestOf(bad))]; ok {
		t.Fatal("the corrupt blob was stored")
	}
	if len(keysUnder(b, imageManifestsDir)) != 0 || res.Images != 0 {
		t.Fatalf("manifest stored without its blob: %v, images=%d", keysUnder(b, imageManifestsDir), res.Images)
	}
}

// TestSyncImagesSkipsWhatTheRegistryLost: a manifest whose blob the registry no
// longer has is reported, not copied, and does not fail the run; one copied
// earlier keeps its off-site copy.
func TestSyncImagesSkipsWhatTheRegistryLost(t *testing.T) {
	reg := newFakeRegistry()
	keptLayer := layer(100)
	kept := reg.image("user/a", "v1", keptLayer)
	s, b, _ := newImageSyncer(t, reg)
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	lostLayer := layer(100)
	lost := reg.image("user/a", "v2", lostLayer)
	reg.gone[digestOf(lostLayer)] = true
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run = %v, want a lost blob to be reported only", err)
	}
	if len(res.ImagesIncomplete) != 1 || !strings.Contains(res.ImagesIncomplete[0], lost) {
		t.Fatalf("incomplete = %v, want %s", res.ImagesIncomplete, lost)
	}
	x, _ := LoadImageIndex(context.Background(), b, s.Key, res.ImageIndex)
	if got := x.Repositories["user/a"]; !slices.Equal(got.Manifests, []string{kept}) || got.Tags["v2"] != "" {
		t.Fatalf("user/a = %+v, want only the complete image", got)
	}
}

// TestSyncImagesKeepsReplacedStates: a registry that comes back empty (a host
// being rebuilt) writes a new index version but prunes nothing; the objects of
// the old state go only ImageHistory after it was replaced.
func TestSyncImagesKeepsReplacedStates(t *testing.T) {
	reg := newFakeRegistry()
	reg.image("user/a", "v1", layer(100))
	s, b, clock := newImageSyncer(t, reg)
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	objects := len(keysUnder(b, imageBlobsDir)) + len(keysUnder(b, imageManifestsDir))

	reg.repos = map[string]*fakeRepo{}
	*clock = clock.Add(time.Hour)
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.ImageRepos != 0 || len(keysUnder(b, imageIndexDir)) != 2 || res.ImageObjectsPruned != 0 {
		t.Fatalf("after emptying: repos=%d versions=%v pruned=%d", res.ImageRepos, keysUnder(b, imageIndexDir), res.ImageObjectsPruned)
	}
	if got := len(keysUnder(b, imageBlobsDir)) + len(keysUnder(b, imageManifestsDir)); got != objects {
		t.Fatalf("objects = %d, want the old state's %d kept", got, objects)
	}
	// A restore now refuses the empty newest state and names the old one.
	if _, _, err := ChooseImageIndex(context.Background(), b, s.Key, ""); err == nil || !strings.Contains(err.Error(), "-at") {
		t.Fatalf("ChooseImageIndex = %v, want it to point at -at", err)
	}

	*clock = clock.Add(ImageHistory - 2*time.Hour)
	if res, _ := s.Run(context.Background()); res.ImageObjectsPruned != 0 {
		t.Fatalf("pruned %d objects inside the history window", res.ImageObjectsPruned)
	}
	*clock = clock.Add(3 * time.Hour)
	res, err = s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.ImageObjectsPruned != objects+1 || len(keysUnder(b, imageBlobsDir)) != 0 || len(keysUnder(b, imageIndexDir)) != 1 {
		t.Fatalf("pruned %d, left blobs %v and versions %v", res.ImageObjectsPruned, keysUnder(b, imageBlobsDir), keysUnder(b, imageIndexDir))
	}
}

// TestSyncImagesUnreadableRepoKeepsItsEntry: a repository the registry fails to
// list keeps its last recorded state, and the failed run prunes nothing.
func TestSyncImagesUnreadableRepoKeepsItsEntry(t *testing.T) {
	reg := newFakeRegistry()
	a := reg.image("user/a", "v1", layer(100))
	s, b, clock := newImageSyncer(t, reg)
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg.revErr["user/a"] = errors.New("gate restarting")
	reg.image("user/b", "v1", layer(100))
	*clock = clock.Add(ImageHistory * 2)
	res, err := s.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with an unreadable repository")
	}
	x, _ := LoadImageIndex(context.Background(), b, s.Key, res.ImageIndex)
	if got := x.Repositories["user/a"]; !slices.Equal(got.Manifests, []string{a}) {
		t.Fatalf("user/a = %+v, want its last recorded state", got)
	}
	if _, ok := x.Repositories["user/b"]; !ok || res.ImageObjectsPruned != 0 {
		t.Fatalf("user/b missing or pruned %d", res.ImageObjectsPruned)
	}
}

// TestFetchImagesRestoresRegistry: a fresh registry gets back every user image
// with the same digests and tags, blobs before the manifests that name them and
// an index after its children; a second run pushes nothing.
func TestFetchImagesRestoresRegistry(t *testing.T) {
	reg := newFakeRegistry()
	shared := layer(3000)
	a := reg.image("user/a", "v1", shared, layer(9000))
	child := reg.image("user/multi", "arm64", shared)
	idx := reg.index("user/multi", "latest", child)
	s, b, _ := newImageSyncer(t, reg)
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	stamp, x, err := ChooseImageIndex(context.Background(), b, s.Key, "")
	if err != nil || stamp == "" {
		t.Fatalf("ChooseImageIndex = %q, %v", stamp, err)
	}
	fresh := newFakeRegistry()
	res, err := FetchImages(context.Background(), b, s.Key, x, fresh, nil)
	if err != nil {
		t.Fatalf("FetchImages: %v", err)
	}
	if res.Repositories != 2 || res.Manifests != 3 || res.Tags != 3 || res.BlobsPushed != 5 {
		t.Fatalf("result = %+v", res)
	}
	for repo, want := range map[string]map[string]string{
		"user/a":     {"v1": a},
		"user/multi": {"arm64": child, "latest": idx},
	} {
		for tag, d := range want {
			if got := fresh.repos[repo].tags[tag]; got != d {
				t.Errorf("%s:%s = %s, want %s", repo, tag, got, d)
			}
		}
	}
	if !bytes.Equal(fresh.manifests[idx].body, reg.manifests[idx].body) {
		t.Fatal("restored index differs from the original")
	}
	pos := func(entry string) int { return slices.Index(fresh.log, entry) }
	if pos("manifest user/multi "+child) > pos("manifest user/multi "+idx) {
		t.Fatalf("index pushed before its child: %v", fresh.log)
	}

	n := len(fresh.log)
	if _, err := FetchImages(context.Background(), b, s.Key, x, fresh, nil); err != nil {
		t.Fatal(err)
	}
	for _, e := range fresh.log[n:] {
		if strings.HasPrefix(e, "blob ") {
			t.Fatalf("second restore uploaded a blob again: %s", e)
		}
	}
}
