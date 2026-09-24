package offsite

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	s, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParseKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func seal(t *testing.T, plain, key []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Encrypt(&buf, bytes.NewReader(plain), key); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEncryptRoundTrip(t *testing.T) {
	key := testKey(t)
	for _, n := range []int{0, 1, segmentSize - 1, segmentSize, segmentSize + 1, 3*segmentSize + 5} {
		plain := make([]byte, n)
		rand.Read(plain)
		sealed := seal(t, plain, key)
		if int64(len(sealed)) != SealedSize(int64(n)) {
			t.Errorf("n=%d: sealed %d bytes, SealedSize says %d", n, len(sealed), SealedSize(int64(n)))
		}
		var out bytes.Buffer
		if err := Decrypt(&out, bytes.NewReader(sealed), key); err != nil {
			t.Fatalf("n=%d: decrypt: %v", n, err)
		}
		if !bytes.Equal(out.Bytes(), plain) {
			t.Fatalf("n=%d: round trip changed the data", n)
		}
	}
}

// TestDecryptRejectsTampering: a wrong key, a flipped bit, a missing tail at a
// segment boundary and swapped segments all fail instead of yielding data.
func TestDecryptRejectsTampering(t *testing.T) {
	key := testKey(t)
	plain := make([]byte, 3*segmentSize)
	rand.Read(plain)
	sealed := seal(t, plain, key)
	seg := segmentSize + 16

	flipped := bytes.Clone(sealed)
	flipped[headerSize+10] ^= 1
	cut := sealed[:headerSize+2*seg]
	swapped := bytes.Clone(sealed)
	copy(swapped[headerSize:], sealed[headerSize+seg:headerSize+2*seg])
	copy(swapped[headerSize+seg:], sealed[headerSize:headerSize+seg])

	cases := map[string]struct {
		data []byte
		key  []byte
	}{
		"wrong key":   {sealed, testKey(t)},
		"flipped bit": {flipped, key},
		"cut short":   {cut, key},
		"swapped":     {swapped, key},
		"header only": {sealed[:headerSize], key},
		"not ours":    {[]byte("PK\x03\x04 a zip file, not a sealed object"), key},
	}
	for name, c := range cases {
		if err := Decrypt(io.Discard, bytes.NewReader(c.data), c.key); err == nil {
			t.Errorf("%s: decrypted without error", name)
		}
	}
	if err := Decrypt(io.Discard, bytes.NewReader(sealed), testKey(t)); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong key error = %v, want ErrAuth", err)
	}
}

func TestParseKey(t *testing.T) {
	s, _ := NewKey()
	k, err := ParseKey(" " + s + "\n")
	if err != nil || len(k) != KeySize {
		t.Fatalf("ParseKey(NewKey()) = %d bytes, %v", len(k), err)
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", 40)} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
	if KeyID(k) == KeyID(testKey(t)) || len(KeyID(k)) != 16 {
		t.Errorf("KeyID = %q, want 16 hex chars that differ per key", KeyID(k))
	}
}

// ---- fakes ----------------------------------------------------------------

type memBucket struct {
	mu      sync.Mutex
	objs    map[string][]byte
	putErr  map[string]error
	puts    int
	removed []string
}

func newMemBucket() *memBucket {
	return &memBucket{objs: map[string][]byte{}, putErr: map[string]error{}}
}

func (b *memBucket) Put(_ context.Context, key string, r io.Reader, size int64) error {
	if err := b.putErr[key]; err != nil {
		io.Copy(io.Discard, r)
		return err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("put %s: got %d bytes, declared %d", key, len(data), size)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objs[key] = data
	b.puts++
	return nil
}

func (b *memBucket) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objs[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *memBucket) List(_ context.Context, prefix string) ([]Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Object
	for k, v := range b.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Object{Key: k, Size: int64(len(v))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (b *memBucket) Remove(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objs, key)
	b.removed = append(b.removed, key)
	return nil
}

type row struct {
	WorldBackup
	status  string
	expires time.Time
	offsite time.Time
}

type fakeCatalog struct{ rows []*row }

func (c *fakeCatalog) PendingWorlds(context.Context) ([]WorldBackup, error) {
	var out []WorldBackup
	for _, r := range c.rows {
		if r.status == "present" && r.offsite.IsZero() {
			out = append(out, r.WorldBackup)
		}
	}
	return out, nil
}

func (c *fakeCatalog) PresentWorlds(context.Context) ([]WorldBackup, error) {
	var out []WorldBackup
	for _, r := range c.rows {
		if r.status == "present" {
			out = append(out, r.WorldBackup)
		}
	}
	return out, nil
}

func (c *fakeCatalog) MarkOffsite(_ context.Context, id string, at time.Time) error {
	for _, r := range c.rows {
		if r.ID == id {
			r.offsite = at
		}
	}
	return nil
}

func (c *fakeCatalog) ExpiredRefs(_ context.Context, now time.Time) ([]string, error) {
	var out []string
	for _, r := range c.rows {
		if r.status == "deleted" && r.expires.Before(now) && !r.offsite.IsZero() {
			out = append(out, r.Ref)
		}
	}
	return out, nil
}

func writeFile(t *testing.T, dir, name string, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	rand.Read(data)
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

var now = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func newSyncer(t *testing.T, cat *fakeCatalog) (*Syncer, *memBucket) {
	t.Helper()
	b := newMemBucket()
	return &Syncer{
		Bucket: b, Catalog: cat, Key: testKey(t),
		ArchiveDir: t.TempDir(), DBDir: t.TempDir(), DBKeep: 2,
		Now: func() time.Time { return now },
	}, b
}

// TestSyncCopiesWorldsAndRecordsThem: each pending archive is encrypted into
// the bucket and marked; a second run sends nothing again.
func TestSyncCopiesWorldsAndRecordsThem(t *testing.T) {
	cat := &fakeCatalog{rows: []*row{
		{WorldBackup: WorldBackup{ID: "b1", Server: "alpha", Ref: "/var/lib/felis/archives/alpha-1.tar.gz"}, status: "present"},
		{WorldBackup: WorldBackup{ID: "b2", Server: "beta", Ref: "/var/lib/felis/archives/beta-2.tar.gz"}, status: "present"},
	}}
	s, b := newSyncer(t, cat)
	alpha := writeFile(t, s.ArchiveDir, "alpha-1.tar.gz", 3*segmentSize+7)
	writeFile(t, s.ArchiveDir, "beta-2.tar.gz", 10)

	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v (%+v)", err, res)
	}
	if res.WorldsUploaded != 2 || res.WorldsPending != 0 || res.RemoteWorlds != 2 {
		t.Fatalf("result = %+v, want 2 uploaded, 0 pending, 2 remote", res)
	}
	for _, r := range cat.rows {
		if !r.offsite.Equal(now) {
			t.Errorf("row %s offsite_at = %v, want %v", r.ID, r.offsite, now)
		}
	}
	var out bytes.Buffer
	if err := Decrypt(&out, bytes.NewReader(b.objs["worlds/alpha-1.tar.gz.fenc"]), s.Key); err != nil || !bytes.Equal(out.Bytes(), alpha) {
		t.Fatalf("stored alpha does not decrypt to the archive (err %v)", err)
	}

	puts := b.puts
	if res, err := s.Run(context.Background()); err != nil || res.WorldsUploaded != 0 || b.puts != puts {
		t.Fatalf("second run uploaded again: %+v, puts %d→%d, err %v", res, puts, b.puts, err)
	}
}

// TestSyncResumesWithoutResending: an object stored by a run that died before
// recording it is recorded, not uploaded twice; a partial one is replaced.
func TestSyncResumesWithoutResending(t *testing.T) {
	cat := &fakeCatalog{rows: []*row{
		{WorldBackup: WorldBackup{ID: "b1", Server: "alpha", Ref: "/a/alpha-1.tar.gz"}, status: "present"},
		{WorldBackup: WorldBackup{ID: "b2", Server: "beta", Ref: "/a/beta-2.tar.gz"}, status: "present"},
	}}
	s, b := newSyncer(t, cat)
	alpha := writeFile(t, s.ArchiveDir, "alpha-1.tar.gz", 1000)
	writeFile(t, s.ArchiveDir, "beta-2.tar.gz", 1000)
	b.objs["worlds/alpha-1.tar.gz.fenc"] = seal(t, alpha, s.Key)
	b.objs["worlds/beta-2.tar.gz.fenc"] = []byte("partial")

	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.WorldsUploaded != 1 || b.puts != 1 {
		t.Fatalf("uploaded %d (puts %d), want only the partial beta resent", res.WorldsUploaded, b.puts)
	}
	if cat.rows[0].offsite.IsZero() || cat.rows[1].offsite.IsZero() {
		t.Fatal("both rows should be recorded as copied")
	}
}

// TestSyncFailureLeavesRowPending: a failed upload is reported, the row stays
// unmarked (so the reaper keeps the world), and the rest still go.
func TestSyncFailureLeavesRowPending(t *testing.T) {
	cat := &fakeCatalog{rows: []*row{
		{WorldBackup: WorldBackup{ID: "b1", Server: "alpha", Ref: "/a/alpha-1.tar.gz"}, status: "present"},
		{WorldBackup: WorldBackup{ID: "b2", Server: "beta", Ref: "/a/beta-2.tar.gz"}, status: "present"},
		{WorldBackup: WorldBackup{ID: "b3", Server: "gamma", Ref: "/a/gamma-3.tar.gz"}, status: "present"},
	}}
	s, b := newSyncer(t, cat)
	writeFile(t, s.ArchiveDir, "alpha-1.tar.gz", 100)
	writeFile(t, s.ArchiveDir, "beta-2.tar.gz", 100)
	b.putErr["worlds/alpha-1.tar.gz.fenc"] = errors.New("503 slow down")

	res, err := s.Run(context.Background())
	if err == nil {
		t.Fatal("run reported success despite a failed upload")
	}
	if !cat.rows[0].offsite.IsZero() {
		t.Error("the failed archive was recorded as copied")
	}
	if cat.rows[1].offsite.IsZero() {
		t.Error("the archive after the failure was not copied")
	}
	if res.WorldsPending != 1 || len(res.WorldsMissing) != 1 || !strings.Contains(res.WorldsMissing[0], "gamma") {
		t.Fatalf("result = %+v, want alpha pending, gamma missing", res)
	}
}

// TestSyncExpiresOnlyPastRetention: a remote archive goes once its row has
// expired; one evicted early from the local disk stays until then, and an
// object with no row at all is left alone.
func TestSyncExpiresOnlyPastRetention(t *testing.T) {
	cat := &fakeCatalog{rows: []*row{
		{WorldBackup: WorldBackup{ID: "old", Ref: "/a/old.tar.gz"}, status: "deleted", expires: now.Add(-time.Hour), offsite: now.Add(-100 * 24 * time.Hour)},
		{WorldBackup: WorldBackup{ID: "evicted", Ref: "/a/evicted.tar.gz"}, status: "deleted", expires: now.Add(30 * 24 * time.Hour), offsite: now.Add(-24 * time.Hour)},
	}}
	s, b := newSyncer(t, cat)
	for _, k := range []string{"worlds/old.tar.gz.fenc", "worlds/evicted.tar.gz.fenc", "worlds/unknown.tar.gz.fenc"} {
		b.objs[k] = []byte("x")
	}
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.WorldsExpired != 1 || len(b.removed) != 1 || b.removed[0] != "worlds/old.tar.gz.fenc" {
		t.Fatalf("removed %v (expired %d), want only the expired archive", b.removed, res.WorldsExpired)
	}
	if res.RemoteWorlds != 2 {
		t.Fatalf("remote worlds = %d, want 2 left", res.RemoteWorlds)
	}
}

// TestSyncDBKeepsNewest: only the newest DBKeep bundles are sent, and older
// remote bundles are pruned down to DBKeep.
func TestSyncDBKeepsNewest(t *testing.T) {
	s, b := newSyncer(t, &fakeCatalog{})
	for _, name := range []string{
		"felis-db-20260920T030000Z-daily.tar",
		"felis-db-20260922T030000Z-daily.tar",
		"felis-db-20260923T030000Z-manual.tar",
		"felis-db-20260924T030000Z-daily.tar",
	} {
		writeFile(t, s.DBDir, name, 50)
	}
	b.objs["db/felis-db-20260901T030000Z-daily.tar.fenc"] = []byte("old")
	b.objs["db/notes.txt"] = []byte("not ours")

	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.DBUploaded != 2 || res.RemoteDB != 2 || res.NewestDB != "felis-db-20260924T030000Z-daily.tar" {
		t.Fatalf("result = %+v, want the newest 2 uploaded", res)
	}
	var keys []string
	for k := range b.objs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := "db/felis-db-20260923T030000Z-manual.tar.fenc db/felis-db-20260924T030000Z-daily.tar.fenc db/notes.txt"
	if strings.Join(keys, " ") != want {
		t.Fatalf("bucket = %v, want %s", keys, want)
	}
	listed, err := ListDB(context.Background(), b)
	if err != nil || len(listed) != 2 || listed[0].Key != "felis-db-20260924T030000Z-daily.tar" {
		t.Fatalf("ListDB = %+v, %v", listed, err)
	}
}

// TestFetchWorldsRestoresVolume: after a rebuild, every present archive the
// volume lacks comes back byte for byte; ones already there are left alone.
func TestFetchWorldsRestoresVolume(t *testing.T) {
	cat := &fakeCatalog{rows: []*row{
		{WorldBackup: WorldBackup{ID: "b1", Server: "alpha", Ref: "/a/alpha-1.tar.gz"}, status: "present"},
		{WorldBackup: WorldBackup{ID: "b2", Server: "beta", Ref: "/a/beta-2.tar.gz"}, status: "present"},
		{WorldBackup: WorldBackup{ID: "b3", Server: "gamma", Ref: "/a/gamma-3.tar.gz"}, status: "present"},
	}}
	s, b := newSyncer(t, cat)
	alpha := writeFile(t, s.ArchiveDir, "alpha-1.tar.gz", segmentSize+3)
	writeFile(t, s.ArchiveDir, "beta-2.tar.gz", 20)
	if res, err := s.Run(context.Background()); err != nil || len(res.WorldsMissing) != 1 {
		t.Fatalf("run = %+v, %v; want gamma reported missing", res, err)
	}

	fresh := t.TempDir()
	os.WriteFile(filepath.Join(fresh, "beta-2.tar.gz"), []byte("kept"), 0o600)
	res, err := FetchWorlds(context.Background(), b, cat, s.Key, fresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Fetched, ",") != "alpha-1.tar.gz" || len(res.Missing) != 1 {
		t.Fatalf("fetch = %+v, want alpha fetched, gamma missing", res)
	}
	got, _ := os.ReadFile(filepath.Join(fresh, "alpha-1.tar.gz"))
	if !bytes.Equal(got, alpha) {
		t.Fatal("fetched alpha differs from the original")
	}
	if kept, _ := os.ReadFile(filepath.Join(fresh, "beta-2.tar.gz")); string(kept) != "kept" {
		t.Fatal("fetch overwrote an archive already on the volume")
	}

	// A wrong key leaves no file behind, partial or whole.
	other := t.TempDir()
	if err := FetchObject(context.Background(), b, testKey(t), "worlds/alpha-1.tar.gz.fenc", filepath.Join(other, "alpha-1.tar.gz"), 0o600); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong key fetch = %v, want ErrAuth", err)
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Fatalf("wrong key left files: %v", entries)
	}
}

func TestWorldKeyRejectsOddRefs(t *testing.T) {
	if k, ok := WorldKey("/var/lib/felis/archives/alpha-1.tar.gz"); !ok || k != "worlds/alpha-1.tar.gz.fenc" {
		t.Fatalf("WorldKey = %q, %v", k, ok)
	}
	for _, ref := range []string{"", "/", "..", "/a/.."} {
		if _, ok := WorldKey(ref); ok {
			t.Errorf("WorldKey(%q) accepted", ref)
		}
	}
}
