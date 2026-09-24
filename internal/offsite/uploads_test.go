package offsite

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newUploadsSyncer(t *testing.T) (*Syncer, *memBucket, *time.Time, string) {
	t.Helper()
	dir := t.TempDir()
	b := newMemBucket()
	clock := now
	return &Syncer{
		Bucket: b, Catalog: &fakeCatalog{}, Key: testKey(t), UploadsDir: dir,
		Now: func() time.Time { return clock },
	}, b, &clock, dir
}

func putContext(t *testing.T, dir, id string, data []byte, mtime time.Time) {
	t.Helper()
	sub := filepath.Join(dir, id)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, UploadContextFile)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func runSync(t *testing.T, s *Syncer) Result {
	t.Helper()
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (%v)", err, res.Errors)
	}
	return res
}

// TestSyncUploadsCopiesContexts: every submission's context is copied, two
// identical ones once; a directory without its context and a name that is not
// a submission id are left out. A second run with nothing new sends nothing.
func TestSyncUploadsCopiesContexts(t *testing.T) {
	s, b, _, dir := newUploadsSyncer(t)
	pack := bytes.Repeat([]byte("modpack"), 5000)
	putContext(t, dir, "sub-aaa", pack, now.Add(-time.Hour))
	putContext(t, dir, "sub-bbb", pack, now.Add(-time.Hour))
	putContext(t, dir, "sub-ccc", []byte("another pack"), now.Add(-time.Hour))
	if err := os.MkdirAll(filepath.Join(dir, "sub-writing"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}

	res := runSync(t, s)
	if res.Uploads != 3 || res.UploadsUploaded != 2 {
		t.Fatalf("uploads=%d uploaded=%d, want 3 contexts in 2 objects", res.Uploads, res.UploadsUploaded)
	}
	if got := keysUnder(b, uploadBlobsDir); len(got) != 2 {
		t.Fatalf("blobs %v, want 2", got)
	}
	if got := keysUnder(b, uploadIndexDir); len(got) != 1 {
		t.Fatalf("index versions %v, want 1", got)
	}
	x, err := LoadUploadIndex(context.Background(), b, s.Key, res.UploadIndex)
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Contexts) != 3 || x.Contexts["sub-aaa"].Digest != x.Contexts["sub-bbb"].Digest {
		t.Fatalf("index %+v", x.Contexts)
	}

	puts := b.puts
	res = runSync(t, s)
	if b.puts != puts || res.UploadsUploaded != 0 {
		t.Fatalf("second run put %d objects, uploaded %d; want nothing", b.puts-puts, res.UploadsUploaded)
	}
}

// TestSyncUploadsEmptyVolumeKeepsCopies: a volume that comes back empty (a
// rebuilt host before its restore) records an empty version, keeps every
// context for UploadHistory, and a restore refuses the empty version until
// told which one to use. Past the history both go.
func TestSyncUploadsEmptyVolumeKeepsCopies(t *testing.T) {
	s, b, clock, dir := newUploadsSyncer(t)
	putContext(t, dir, "sub-aaa", []byte("pack a"), now.Add(-time.Hour))
	putContext(t, dir, "sub-bbb", []byte("pack b"), now.Add(-time.Hour))
	first := runSync(t, s).UploadIndex

	for _, id := range []string{"sub-aaa", "sub-bbb"} {
		if err := os.RemoveAll(filepath.Join(dir, id)); err != nil {
			t.Fatal(err)
		}
	}
	*clock = clock.Add(time.Hour)
	res := runSync(t, s)
	if res.Uploads != 0 || res.UploadObjectsPruned != 0 {
		t.Fatalf("empty volume: uploads=%d pruned=%d", res.Uploads, res.UploadObjectsPruned)
	}
	if got := keysUnder(b, uploadBlobsDir); len(got) != 2 {
		t.Fatalf("blobs %v, want both kept", got)
	}
	ctx := context.Background()
	if _, _, err := ChooseUploadIndex(ctx, b, s.Key, ""); err == nil || !strings.Contains(err.Error(), first) {
		t.Fatalf("choosing the empty newest version: %v, want a refusal naming %s", err, first)
	}
	stamp, x, err := ChooseUploadIndex(ctx, b, s.Key, first)
	if err != nil || stamp != first || len(x.Contexts) != 2 {
		t.Fatalf("choose %s: %s %v %v", first, stamp, x, err)
	}

	*clock = clock.Add(UploadHistory + time.Hour)
	res = runSync(t, s)
	if got := keysUnder(b, uploadBlobsDir); len(got) != 0 {
		t.Fatalf("blobs %v after the history, want none", got)
	}
	if got := keysUnder(b, uploadIndexDir); len(got) != 1 {
		t.Fatalf("index versions %v after the history, want the newest only", got)
	}
	if res.UploadObjectsPruned != 3 {
		t.Fatalf("pruned %d, want the old version and its 2 contexts", res.UploadObjectsPruned)
	}
}

// TestSyncUploadsReplacedContext: a context rewritten in place is copied again;
// the old bytes stay while the version that names them is kept.
func TestSyncUploadsReplacedContext(t *testing.T) {
	s, b, clock, dir := newUploadsSyncer(t)
	putContext(t, dir, "sub-aaa", []byte("version one"), now.Add(-time.Hour))
	runSync(t, s)

	putContext(t, dir, "sub-aaa", []byte("version two"), now)
	*clock = clock.Add(time.Hour)
	res := runSync(t, s)
	if res.UploadsUploaded != 1 {
		t.Fatalf("uploaded %d, want the new version", res.UploadsUploaded)
	}
	if got := keysUnder(b, uploadBlobsDir); len(got) != 2 {
		t.Fatalf("blobs %v, want old and new", got)
	}

	*clock = clock.Add(UploadHistory + time.Hour)
	runSync(t, s)
	if got := keysUnder(b, uploadBlobsDir); len(got) != 1 {
		t.Fatalf("blobs %v after the history, want the new one only", got)
	}
}

// TestFetchUploadsRestores: a restore writes every context back in place,
// leaves one that is already right alone, and rewrites one whose bytes differ.
func TestFetchUploadsRestores(t *testing.T) {
	s, b, _, dir := newUploadsSyncer(t)
	a, c := bytes.Repeat([]byte("a"), 70000), []byte("pack c")
	putContext(t, dir, "sub-aaa", a, now.Add(-time.Hour))
	putContext(t, dir, "sub-ccc", c, now.Add(-time.Hour))
	stamp := runSync(t, s).UploadIndex

	ctx := context.Background()
	x, err := LoadUploadIndex(ctx, b, s.Key, stamp)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	res, err := FetchUploads(ctx, b, s.Key, x, target, -1, -1, nil)
	if err != nil || res.Written != 2 || res.Bytes != int64(len(a)+len(c)) {
		t.Fatalf("fetch: %+v %v", res, err)
	}
	for id, want := range map[string][]byte{"sub-aaa": a, "sub-ccc": c} {
		got, err := os.ReadFile(filepath.Join(target, id, UploadContextFile))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s restored as %d bytes (%v)", id, len(got), err)
		}
	}

	if err := os.WriteFile(filepath.Join(target, "sub-ccc", UploadContextFile), []byte("pack X"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = FetchUploads(ctx, b, s.Key, x, target, -1, -1, nil)
	if err != nil || res.Written != 1 || res.Present != 1 {
		t.Fatalf("second fetch: %+v %v, want the damaged one rewritten", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "sub-ccc", UploadContextFile)); !bytes.Equal(got, c) {
		t.Fatalf("sub-ccc is %q after the second fetch", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(target, "*", "*.restore"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}
