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
	// stall holds keys whose upload never finishes: Put waits out its context.
	stall map[string]bool
	// started lists every Put in the order it began.
	started []string
	// modified is what List reports as each key's modification time.
	modified map[string]time.Time
	// getErr fails Get for a key the way an unreachable bucket would.
	getErr map[string]error
}

func newMemBucket() *memBucket {
	return &memBucket{objs: map[string][]byte{}, putErr: map[string]error{}}
}

func (b *memBucket) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	b.mu.Lock()
	b.started = append(b.started, key)
	b.mu.Unlock()
	if b.stall[key] {
		<-ctx.Done()
		return ctx.Err()
	}
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
	if err := b.getErr[key]; err != nil {
		return nil, err
	}
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
			out = append(out, Object{Key: k, Size: int64(len(v)), Modified: b.modified[k]})
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

func (c *fakeCatalog) NewestOffsite(context.Context) (time.Time, error) {
	var at time.Time
	for _, r := range c.rows {
		if r.offsite.After(at) {
			at = r.offsite
		}
	}
	return at, nil
}

func (c *fakeCatalog) KeptRefs(_ context.Context, now time.Time) ([]string, error) {
	var out []string
	for _, r := range c.rows {
		if r.status == "present" || !r.expires.Before(now) {
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

// newSyncer syncs into a bucket that already records its key, as every bucket
// does after its first run.
func newSyncer(t *testing.T, cat *fakeCatalog) (*Syncer, *memBucket) {
	t.Helper()
	b := newMemBucket()
	key := testKey(t)
	b.objs[keyMark] = []byte(KeyID(key) + "\n")
	return &Syncer{
		Bucket: b, Catalog: cat, Key: key,
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
	want := "db/felis-db-20260923T030000Z-manual.tar.fenc db/felis-db-20260924T030000Z-daily.tar.fenc db/notes.txt " + keyMark
	if strings.Join(keys, " ") != want {
		t.Fatalf("bucket = %v, want %s", keys, want)
	}
	listed, err := ListDB(context.Background(), b)
	if err != nil || len(listed) != 2 || listed[0].Key != "felis-db-20260924T030000Z-daily.tar" {
		t.Fatalf("ListDB = %+v, %v", listed, err)
	}
}

// TestSyncDBRanksWithTheBucket: an old local bundle (kept here by its label's
// own retention) that newer bundles in the bucket outrank is not sent, so a
// pass does not upload what it then prunes, and the next pass the same again.
func TestSyncDBRanksWithTheBucket(t *testing.T) {
	s, b := newSyncer(t, &fakeCatalog{})
	writeFile(t, s.DBDir, "felis-db-20260910T030000Z-pre-migrate.tar", 50)
	writeFile(t, s.DBDir, "felis-db-20260924T030000Z-daily.tar", 50)
	b.objs["db/felis-db-20260923T030000Z-daily.tar.fenc"] = []byte("gone here, kept there")

	for pass := 1; pass <= 2; pass++ {
		res, err := s.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if pass == 1 {
			want = 1
		}
		if res.DBUploaded != want || res.DBPruned != 0 || res.RemoteDB != 2 {
			t.Fatalf("pass %d: result = %+v, want %d uploaded, none pruned, 2 held", pass, res, want)
		}
	}
	if _, ok := b.objs["db/felis-db-20260910T030000Z-pre-migrate.tar.fenc"]; ok {
		t.Fatal("the outranked bundle was sent")
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

// TestSyncBigArchiveDoesNotStallThePass: an archive the uplink cannot send in
// time fails at its own deadline and stays pending, while the database bundle
// (sent before any world) and the archives after it still reach the bucket.
// One deadline for the whole pass used to go to the big archive, every hour,
// with the bundles queued behind it.
func TestSyncBigArchiveDoesNotStallThePass(t *testing.T) {
	cat := &fakeCatalog{rows: []*row{
		{WorldBackup: WorldBackup{ID: "b1", Server: "alpha", Ref: "/a/alpha-1.tar.gz"}, status: "present"},
		{WorldBackup: WorldBackup{ID: "b2", Server: "beta", Ref: "/a/beta-2.tar.gz"}, status: "present"},
	}}
	s, b := newSyncer(t, cat)
	s.UploadGrace = 50 * time.Millisecond
	writeFile(t, s.ArchiveDir, "alpha-1.tar.gz", 100)
	writeFile(t, s.ArchiveDir, "beta-2.tar.gz", 100)
	writeFile(t, s.DBDir, "felis-db-20260924T030000Z-daily.tar", 50)
	b.stall = map[string]bool{"worlds/alpha-1.tar.gz.fenc": true}

	type ran struct {
		res Result
		err error
	}
	done := make(chan ran, 1)
	go func() {
		res, err := s.Run(context.Background())
		done <- ran{res, err}
	}()
	var r ran
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the pass is still waiting on the archive that cannot be sent")
	}

	if r.err == nil || !strings.Contains(r.err.Error(), "alpha-1.tar.gz") || !strings.Contains(r.err.Error(), "the next run tries again") {
		t.Fatalf("run error = %v, want the stalled archive named as retried next run", r.err)
	}
	if r.res.DBUploaded != 1 || r.res.WorldsUploaded != 1 || r.res.WorldsPending != 1 {
		t.Fatalf("result = %+v, want the bundle and beta copied, alpha pending", r.res)
	}
	if !cat.rows[0].offsite.IsZero() || cat.rows[1].offsite.IsZero() {
		t.Fatalf("offsite_at alpha=%v beta=%v, want only beta recorded", cat.rows[0].offsite, cat.rows[1].offsite)
	}
	if len(b.started) == 0 || !strings.HasPrefix(b.started[0], dbDir) {
		t.Fatalf("uploads began in the order %v, want the database bundle first", b.started)
	}
}

// TestUploadBudgetFitsTheUplink: by default a 10 GiB archive may take well over
// what a 20 Mbit/s uplink needs to send it, and the smallest object still gets
// the grace.
func TestUploadBudgetFitsTheUplink(t *testing.T) {
	s := &Syncer{}
	const big = 10 << 30
	need := time.Duration(float64(big) / (20e6 / 8) * float64(time.Second))
	if got := s.uploadBudget(big); got < 3*need {
		t.Errorf("budget for 10 GiB = %s, want at least %s (3× a 20 Mbit/s uplink)", got, 3*need)
	}
	if got := s.uploadBudget(0); got < defaultUploadGrace {
		t.Errorf("budget for an empty object = %s, want at least %s", got, defaultUploadGrace)
	}
}
