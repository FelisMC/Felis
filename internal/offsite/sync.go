// Package offsite maintains encrypted recovery copies of worlds, database bundles, registry images and uploads in S3-compatible storage.
package offsite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"felis.lolicon.best/internal/dbbackup"
)

const (
	worldsDir = "worlds/"
	dbDir     = "db/"
	objExt    = ".fenc"
)

func WorldKey(ref string) (string, bool) {
	name := path.Base(ref)
	if !safeName(name) {
		return "", false
	}
	return worldsDir + name + objExt, true
}

func DBKey(bundle string) string { return dbDir + bundle + objExt }

func safeName(name string) bool {
	return name != "" && name != "." && name != ".." && name != "/" && !strings.ContainsAny(name, `/\`)
}

type WorldBackup struct {
	ID      string
	Server  string
	Ref     string
	Created time.Time
}

type Catalog interface {
	PendingWorlds(ctx context.Context) ([]WorldBackup, error)
	MarkOffsite(ctx context.Context, id string, at time.Time) error
	ExpiredRefs(ctx context.Context, now time.Time) ([]string, error)
	PresentWorlds(ctx context.Context) ([]WorldBackup, error)
	// NewestOffsite returns zero when no archive has been copied.
	NewestOffsite(ctx context.Context) (time.Time, error)
	// KeptRefs retains present archives and nonpresent rows until expires_at.
	KeptRefs(ctx context.Context, now time.Time) ([]string, error)
}

type Syncer struct {
	Bucket  Bucket
	Catalog Catalog
	Key     []byte
	// ArchiveDir empty means no local archive directory is available.
	ArchiveDir string
	DBDir      string
	DBKeep     int // excludes the newest snapshot bundle
	// Snapshot=nil disables fresh dbbackup.LabelOffsite bundles.
	Snapshot func(ctx context.Context) error
	// OrphanAfter=0 preserves unreferenced world objects.
	OrphanAfter time.Duration
	Images      ImageSource
	// ImagePins=nil excludes felis/ and mirror/; otherwise copy only pinned digests from those repositories.
	ImagePins  func(ctx context.Context) (map[string][]string, error)
	UploadsDir string
	Lease      *Lease // nil disables writer checks
	// Upload budget is UploadGrace plus size/MinRate in bytes per second; zero selects defaults.
	UploadGrace time.Duration
	MinRate     int64
	Now         func() time.Time
	Log         io.Writer
}

// Apply deadlines per object so a large archive cannot starve the rest of a sync pass.
const (
	defaultUploadGrace = 10 * time.Minute
	defaultMinRate     = 512 << 10 // bytes per second
)

func (s *Syncer) uploadBudget(size int64) time.Duration {
	grace := s.UploadGrace
	if grace <= 0 {
		grace = defaultUploadGrace
	}
	return grace + time.Duration(float64(SealedSize(size))/float64(s.rate())*float64(time.Second))
}

func (s *Syncer) rate() int64 {
	if s.MinRate > 0 {
		return s.MinRate
	}
	return defaultMinRate
}

type Result struct {
	WorldsUploaded      int      `json:"worlds_uploaded"`
	BytesUploaded       int64    `json:"bytes_uploaded"`
	WorldsPending       int      `json:"worlds_pending"`
	WorldsMissing       []string `json:"worlds_missing,omitempty"`
	WorldsExpired       int      `json:"worlds_expired"`
	RemoteWorlds        int      `json:"remote_worlds"`
	RemoteBytes         int64    `json:"remote_bytes"`
	DBUploaded          int      `json:"db_uploaded"`
	DBPruned            int      `json:"db_pruned"`
	RemoteDB            int      `json:"remote_db"`
	NewestDB            string   `json:"newest_db,omitempty"`
	ImageIndex          string   `json:"image_index,omitempty"`
	ImageRepos          int      `json:"image_repos"`
	Images              int      `json:"images"`
	ImagesUploaded      int      `json:"images_uploaded"`
	ImageBlobsUploaded  int      `json:"image_blobs_uploaded"`
	ImageObjectsPruned  int      `json:"image_objects_pruned"`
	RemoteImageBytes    int64    `json:"remote_image_bytes"`
	ImagesIncomplete    []string `json:"images_incomplete,omitempty"`
	UploadIndex         string   `json:"upload_index,omitempty"`
	Uploads             int      `json:"uploads"`
	UploadsUploaded     int      `json:"uploads_uploaded"`
	UploadObjectsPruned int      `json:"upload_objects_pruned"`
	RemoteUploadBytes   int64    `json:"remote_upload_bytes"`
	Errors              []string `json:"errors,omitempty"`
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Syncer) logf(format string, args ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, "felis offsite: "+format+"\n", args...)
	}
}

// Run sends database bundles before large archives and continues after individual failures, returning an error if any step failed.
func (s *Syncer) Run(ctx context.Context) (Result, error) {
	var res Result
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		res.Errors = append(res.Errors, msg)
		s.logf("%s", msg)
	}

	// Check encryption key and writer ownership before any upload or pruning.
	if err := claim(ctx, s.Bucket, s.Key, s.Lease, s.now()); err != nil {
		return res, err
	}
	remoteWorlds, err := s.listSizes(ctx, worldsDir)
	if err != nil {
		return res, fmt.Errorf("list %s in the bucket: %w", worldsDir, err)
	}
	s.syncDB(ctx, &res, fail)
	s.syncWorlds(ctx, remoteWorlds, &res, fail)
	s.snapshotDB(ctx, &res, fail)
	s.syncImages(ctx, &res, fail)
	s.syncUploads(ctx, &res, fail)
	s.expireWorlds(ctx, remoteWorlds, &res, fail)
	s.sweepWorlds(ctx, remoteWorlds, &res, fail)

	for _, size := range remoteWorlds {
		res.RemoteWorlds++
		res.RemoteBytes += size
	}
	if len(res.Errors) > 0 {
		return res, fmt.Errorf("%d of this run's steps failed; first: %s", len(res.Errors), res.Errors[0])
	}
	return res, nil
}

func (s *Syncer) listSizes(ctx context.Context, prefix string) (map[string]int64, error) {
	objs, err := s.Bucket.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(objs))
	for _, o := range objs {
		out[o.Key] = o.Size
	}
	return out, nil
}

func (s *Syncer) syncWorlds(ctx context.Context, remote map[string]int64, res *Result, fail func(string, ...any)) {
	pending, err := s.Catalog.PendingWorlds(ctx)
	if err != nil {
		fail("list world archives waiting for a copy: %v", err)
		return
	}
	if s.ArchiveDir == "" {
		res.WorldsPending = len(pending)
		if len(pending) > 0 {
			fail("%d world archives wait for a copy, but there is no archive volume to read them from", len(pending))
		}
		return
	}
	for _, w := range pending {
		if ctx.Err() != nil {
			fail("stopped: %v", ctx.Err())
			res.WorldsPending++
			continue
		}
		key, ok := WorldKey(w.Ref)
		if !ok {
			fail("world archive %s of %s has an unusable path %q", w.ID, w.Server, w.Ref)
			res.WorldsPending++
			continue
		}
		local := filepath.Join(s.ArchiveDir, path.Base(w.Ref))
		fi, err := os.Stat(local)
		if errors.Is(err, os.ErrNotExist) {
			res.WorldsMissing = append(res.WorldsMissing, fmt.Sprintf("%s (%s, backup %s)", path.Base(w.Ref), w.Server, w.ID))
			continue
		}
		if err != nil {
			fail("world archive %s: %v", local, err)
			res.WorldsPending++
			continue
		}
		want := SealedSize(fi.Size())
		// If an earlier run uploaded but did not record an object, record it without resending.
		if size, ok := remote[key]; !ok || size != want {
			if err := s.putFile(ctx, key, local, fi.Size()); err != nil {
				fail("upload %s (%s): %v", path.Base(w.Ref), w.Server, err)
				res.WorldsPending++
				continue
			}
			remote[key] = want
			res.WorldsUploaded++
			res.BytesUploaded += fi.Size()
			s.logf("copied world archive %s (%s, %s)", path.Base(w.Ref), w.Server, HumanBytes(fi.Size()))
		}
		if err := s.Catalog.MarkOffsite(ctx, w.ID, s.now()); err != nil {
			fail("record the copy of %s: %v", path.Base(w.Ref), err)
			res.WorldsPending++
		}
	}
}

func (s *Syncer) syncDB(ctx context.Context, res *Result, fail func(string, ...any)) {
	if s.DBDir == "" {
		return
	}
	keep := s.DBKeep
	if keep < 1 {
		keep = 1
	}
	local, err := dbbackup.List(s.DBDir)
	if err != nil {
		fail("list database bundles in %s: %v", s.DBDir, err)
		return
	}
	remote, err := s.listSizes(ctx, dbDir)
	if err != nil {
		fail("list %s in the bucket: %v", dbDir, err)
		return
	}
	// Send only bundles retained across local and remote history so older local bundles are not repeatedly uploaded then pruned.
	names := map[string]bool{}
	for key := range remote {
		name := strings.TrimSuffix(strings.TrimPrefix(key, dbDir), objExt)
		if _, _, ok := dbbackup.ParseBundleName(name); ok && strings.HasSuffix(key, objExt) {
			names[name] = true
		}
	}
	for _, b := range local {
		names[b.Name] = true
	}
	ranked := slices.Sorted(maps.Keys(names))
	slices.Reverse(ranked)
	kept := keptBundles(ranked, keep)
	for _, b := range local {
		if !kept[b.Name] {
			continue
		}
		key := DBKey(b.Name)
		want := SealedSize(b.Size)
		if size, ok := remote[key]; ok && size == want {
			continue
		}
		if err := s.putFile(ctx, key, b.Path, b.Size); err != nil {
			fail("upload database bundle %s: %v", b.Name, err)
			continue
		}
		remote[key] = want
		res.DBUploaded++
		res.BytesUploaded += b.Size
		s.logf("copied database bundle %s (%s)", b.Name, HumanBytes(b.Size))
	}
	for _, name := range ranked {
		key := DBKey(name)
		if _, ok := remote[key]; !ok || kept[name] {
			continue
		}
		if err := s.Bucket.Remove(ctx, key); err != nil {
			fail("prune database bundle %s: %v", name, err)
			continue
		}
		delete(remote, key)
		res.DBPruned++
	}
	res.RemoteDB, res.NewestDB = 0, ""
	for _, name := range ranked {
		if _, ok := remote[DBKey(name)]; ok {
			res.RemoteDB++
			if res.NewestDB == "" {
				res.NewestDB = name
			}
		}
	}
}

// keptBundles keeps daily history independent of frequent snapshots and retains the newest snapshot only while it is newest overall.
func keptBundles(ranked []string, keep int) map[string]bool {
	kept := map[string]bool{}
	n := 0
	for i, name := range ranked {
		if _, label, _ := dbbackup.ParseBundleName(name); label == dbbackup.LabelOffsite {
			if i == 0 {
				kept[name] = true
			}
			continue
		}
		if n++; n > keep {
			break
		}
		kept[name] = true
	}
	return kept
}

// snapshotDB uploads a bundle after newly copied archives so their catalog rows are present in the next recovery database.
func (s *Syncer) snapshotDB(ctx context.Context, res *Result, fail func(string, ...any)) {
	if s.Snapshot == nil || s.DBDir == "" {
		return
	}
	copied, err := s.Catalog.NewestOffsite(ctx)
	if err != nil {
		fail("read when an archive was last copied: %v", err)
		return
	}
	if copied.IsZero() {
		return
	}
	if taken, _, ok := dbbackup.ParseBundleName(res.NewestDB); ok && !copied.Truncate(time.Second).After(taken) {
		return
	}
	if err := s.Snapshot(ctx); err != nil {
		fail("take a database bundle listing the archives just copied: %v", err)
		return
	}
	s.syncDB(ctx, res, fail)
}

func (s *Syncer) expireWorlds(ctx context.Context, remote map[string]int64, res *Result, fail func(string, ...any)) {
	refs, err := s.Catalog.ExpiredRefs(ctx, s.now())
	if err != nil {
		fail("list expired world archives: %v", err)
		return
	}
	for _, ref := range refs {
		key, ok := WorldKey(ref)
		if !ok {
			continue
		}
		if _, ok := remote[key]; !ok {
			continue
		}
		if err := s.Bucket.Remove(ctx, key); err != nil {
			fail("remove expired %s: %v", key, err)
			continue
		}
		delete(remote, key)
		res.WorldsExpired++
	}
}

// sweepWorlds preserves young or age-unknown orphans and sweeps nothing when the catalog is empty, as on a host awaiting database restore.
func (s *Syncer) sweepWorlds(ctx context.Context, remote map[string]int64, res *Result, fail func(string, ...any)) {
	if s.OrphanAfter <= 0 {
		return
	}
	refs, err := s.Catalog.KeptRefs(ctx, s.now())
	if err != nil {
		fail("list the world archives the bucket keeps: %v", err)
		return
	}
	if len(refs) == 0 {
		return
	}
	kept := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if key, ok := WorldKey(ref); ok {
			kept[key] = true
		}
	}
	objs, err := s.Bucket.List(ctx, worldsDir)
	if err != nil {
		fail("list %s in the bucket: %v", worldsDir, err)
		return
	}
	cutoff := s.now().Add(-s.OrphanAfter)
	var young []string
	for _, o := range objs {
		if _, ok := remote[o.Key]; !ok || kept[o.Key] || !strings.HasSuffix(o.Key, objExt) {
			continue
		}
		if o.Modified.IsZero() || o.Modified.After(cutoff) {
			young = append(young, path.Base(o.Key))
			continue
		}
		if err := s.Bucket.Remove(ctx, o.Key); err != nil {
			fail("remove %s, which no backup records: %v", o.Key, err)
			continue
		}
		delete(remote, o.Key)
		res.WorldsExpired++
		s.logf("removed world archive %s: no backup records it, and it was copied %s ago, past the longest retention", path.Base(o.Key), s.now().Sub(o.Modified).Round(time.Hour))
	}
	if len(young) > 0 {
		s.logf("%d world archives in the bucket have no backup record (a database restored from an older bundle?); each goes once it is older than %s: %s",
			len(young), s.OrphanAfter, strings.Join(young[:min(len(young), 5)], ", "))
	}
}

func (s *Syncer) putFile(ctx context.Context, key, p string, size int64) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	// Bound reads to the declared file size; the bucket detects a source that became shorter.
	return s.putStream(ctx, key, io.LimitReader(f, size), size)
}

// putStream uses a known sealed length and per-object deadline, failing on source errors without buffering the whole object.
func (s *Syncer) putStream(ctx context.Context, key string, src io.Reader, size int64) error {
	budget := s.uploadBudget(size)
	putCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(Encrypt(pw, src, s.Key))
	}()
	err := s.Bucket.Put(putCtx, key, pr, SealedSize(size))
	pr.CloseWithError(errors.New("upload finished"))
	if err != nil && ctx.Err() == nil && errors.Is(putCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("not finished within %s (%s at under %s/s); the next run tries again: %w",
			budget.Round(time.Minute), HumanBytes(SealedSize(size)), HumanBytes(s.rate()), err)
	}
	return err
}

type FetchResult struct {
	Fetched  []string
	Present  int
	Missing  []string
	Failures []string
}

func FetchWorlds(ctx context.Context, b Bucket, cat Catalog, key []byte, archiveDir string, log io.Writer) (FetchResult, error) {
	var res FetchResult
	worlds, err := cat.PresentWorlds(ctx)
	if err != nil {
		return res, err
	}
	res.Present = len(worlds)
	for _, w := range worlds {
		objKey, ok := WorldKey(w.Ref)
		if !ok {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: unusable path %q", w.ID, w.Ref))
			continue
		}
		dst := filepath.Join(archiveDir, path.Base(w.Ref))
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		switch err := FetchObject(ctx, b, key, objKey, dst, 0o644); {
		case errors.Is(err, ErrNotFound):
			res.Missing = append(res.Missing, fmt.Sprintf("%s (%s)", path.Base(w.Ref), w.Server))
		case err != nil:
			res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", path.Base(w.Ref), err))
		default:
			alignOwner(dst, archiveDir)
			res.Fetched = append(res.Fetched, path.Base(w.Ref))
			if log != nil {
				fmt.Fprintf(log, "felis offsite: restored %s (%s)\n", path.Base(w.Ref), w.Server)
			}
		}
	}
	if len(res.Failures) > 0 {
		return res, fmt.Errorf("%d archives failed; first: %s", len(res.Failures), res.Failures[0])
	}
	return res, nil
}

// alignOwner changes ownership only as root; otherwise restored archives remain readable under caller ownership.
func alignOwner(file, dir string) {
	if os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = os.Lchown(file, int(st.Uid), int(st.Gid))
	}
}

// FetchObject verifies and decrypts into a hidden temporary sibling before publishing the final name.
func FetchObject(ctx context.Context, b Bucket, key []byte, objKey, dst string, mode os.FileMode) error {
	rc, err := b.Get(ctx, objKey)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".partial")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := Decrypt(f, rc, key); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func ListDB(ctx context.Context, b Bucket) ([]Object, error) {
	objs, err := b.List(ctx, dbDir)
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, o := range objs {
		name := strings.TrimSuffix(strings.TrimPrefix(o.Key, dbDir), objExt)
		if _, _, ok := dbbackup.ParseBundleName(name); !ok || !strings.HasSuffix(o.Key, objExt) {
			continue
		}
		o.Key = name
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key > out[j].Key })
	return out, nil
}

// PeekDB authenticates segments while reading only enough of the encrypted bundle to obtain its manifest.
func PeekDB(ctx context.Context, b Bucket, key []byte, name string) (dbbackup.Manifest, error) {
	rc, err := b.Get(ctx, DBKey(name))
	if err != nil {
		return dbbackup.Manifest{}, err
	}
	defer rc.Close()
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(Decrypt(pw, rc, key))
	}()
	m, err := dbbackup.ReadManifest(pr)
	pr.Close()
	<-done
	return m, err
}

const dbChoices = 3

// ChooseDB refuses a fresh-install newest bundle when older bundles contain more data; legacy bundles without counts are treated conservatively.
func ChooseDB(ctx context.Context, b Bucket, key []byte) (string, dbbackup.Manifest, error) {
	bundles, err := ListDB(ctx, b)
	if err != nil {
		return "", dbbackup.Manifest{}, err
	}
	if len(bundles) == 0 {
		return "", dbbackup.Manifest{}, errors.New("the bucket holds no database bundle")
	}
	newest := bundles[0].Key
	m, err := PeekDB(ctx, b, key, newest)
	if err != nil {
		return "", dbbackup.Manifest{}, fmt.Errorf("%s: %w", newest, err)
	}
	if !m.Counts.Fresh() {
		return newest, m, nil
	}
	var older []string
	for _, o := range bundles[1:] {
		om, err := PeekDB(ctx, b, key, o.Key)
		if err != nil {
			return "", dbbackup.Manifest{}, fmt.Errorf("%s: %w", o.Key, err)
		}
		if c := om.Counts; c == nil || c.Users > m.Counts.Users || c.Servers > m.Counts.Servers {
			older = append(older, fmt.Sprintf("%s (%s)", o.Key, c))
			if len(older) == dbChoices {
				break
			}
		}
	}
	if len(older) == 0 {
		return newest, m, nil
	}
	return "", dbbackup.Manifest{}, fmt.Errorf("the newest bundle %s holds %s, like the database a rebuilt host backs up before its restore; "+
		"fetch an older one by name instead of latest (all of them: felis offsite list): %s", newest, m.Counts, strings.Join(older, ", "))
}

func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
