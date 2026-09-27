// Package offsite keeps a second copy of what a lost node would take with it:
// every world archive (world_backups), the newest control-plane database
// bundles (internal/dbbackup), the user images in the platform registry
// (images.go) and the submission uploads (uploads.go), encrypted, in an
// S3-compatible bucket off the machine. `felis
// offsite sync` runs it from felis-offsite.timer on the host, which is where
// the archive volume and the bundle directory live and where the registry
// answers on its loopback hostPort.
//
// The database records the copy: world_backups.offsite_at is set once an
// archive's object is in the bucket, and with [offsite] configured the reaper
// deletes an idle world only after that (internal/reaper). Remote world
// objects go when their row has expired, so the bucket keeps each archive for
// the same retention the panel promises, including archives evicted early from
// the local disk to make room; an object whose row a restore did not bring
// back goes once it is older than the longest retention. Remote bundles are
// pruned to the newest DBKeep. A pass that copies archives then sends a fresh
// bundle, whose rows list them, so the newest bundle in the bucket lists every
// archive in it and a restore from it can fetch them all.
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

// Object key layout under the configured prefix.
const (
	worldsDir = "worlds/"
	dbDir     = "db/"
	objExt    = ".fenc"
)

// WorldKey is the object key of the world archive stored at ref (the
// world_backups.backup_ref, an in-pod path whose last element is the file name
// on the archive volume).
func WorldKey(ref string) (string, bool) {
	name := path.Base(ref)
	if !safeName(name) {
		return "", false
	}
	return worldsDir + name + objExt, true
}

// DBKey is the object key of a database bundle.
func DBKey(bundle string) string { return dbDir + bundle + objExt }

func safeName(name string) bool {
	return name != "" && name != "." && name != ".." && name != "/" && !strings.ContainsAny(name, `/\`)
}

// WorldBackup is one world_backups row the sync works on.
type WorldBackup struct {
	ID      string
	Server  string
	Ref     string
	Created time.Time
}

// Catalog is the world_backups view the sync needs; PGCatalog in production.
type Catalog interface {
	// PendingWorlds lists present archives without an off-site copy yet,
	// oldest first.
	PendingWorlds(ctx context.Context) ([]WorldBackup, error)
	// MarkOffsite records that the archive of row id is in the bucket.
	MarkOffsite(ctx context.Context, id string, at time.Time) error
	// ExpiredRefs lists the backup_ref of every row past its retention
	// (deleted and expires_at < now) whose archive was copied off-site.
	ExpiredRefs(ctx context.Context, now time.Time) ([]string, error)
	// PresentWorlds lists every present archive, for a restore of the volume.
	PresentWorlds(ctx context.Context) ([]WorldBackup, error)
	// NewestOffsite is the latest offsite_at of any row: when the bucket last
	// gained an archive. Zero when no archive was ever copied.
	NewestOffsite(ctx context.Context) (time.Time, error)
	// KeptRefs lists the backup_ref of every row whose archive the bucket
	// keeps: present ones, and the rest until their expires_at.
	KeptRefs(ctx context.Context, now time.Time) ([]string, error)
}

// Syncer copies what is missing from the bucket and prunes what has expired.
type Syncer struct {
	Bucket  Bucket
	Catalog Catalog
	Key     []byte
	// ArchiveDir is the host directory of the world archive volume; "" when
	// there is none yet (nothing has been archived on this install).
	ArchiveDir string
	// DBDir holds the database bundles; DBKeep is how many of the newest the
	// bucket keeps, not counting the newest Snapshot bundle.
	DBDir  string
	DBKeep int
	// Snapshot writes a bundle labelled dbbackup.LabelOffsite into DBDir. A
	// pass that leaves the bucket holding an archive newer than its newest
	// bundle takes one and sends it (snapshotDB); nil takes none.
	Snapshot func(ctx context.Context) error
	// OrphanAfter is how old a world object no row keeps (KeptRefs) may get
	// before it goes: the longest retention of any archive. Zero keeps them.
	OrphanAfter time.Duration
	// Images is the platform registry whose user images are copied (images.go);
	// nil copies none.
	Images ImageSource
	// ImagePins lists, per repository, the digests a server's spec or a
	// whitelist entry pins. Under felis/ and mirror/ only those revisions are
	// copied (images.go); nil copies none there.
	ImagePins func(ctx context.Context) (map[string][]string, error)
	// UploadsDir is the host directory of the uploads volume, whose submission
	// contexts are copied (uploads.go); empty copies none.
	UploadsDir string
	// UploadGrace and MinRate bound one object's upload: UploadGrace plus the
	// time the object takes at MinRate bytes a second. Zero takes the defaults.
	// Lease names this host in felis-writer (writer.go); nil checks no writer.
	Lease       *Lease
	UploadGrace time.Duration
	MinRate     int64
	Now         func() time.Time
	Log         io.Writer
}

// Each object gets its own deadline, scaled to its size, so an archive too big
// for the uplink fails alone and the rest of the pass still goes; one deadline
// for the whole pass let a 10 GiB world use all of it, hour after hour, with
// every bundle queued behind it. 512 KiB/s (about 4 Mbit/s) gives a 10 GiB
// archive close to six hours, several times what a 20 Mbit/s uplink needs.
const (
	defaultUploadGrace = 10 * time.Minute
	defaultMinRate     = 512 << 10
)

// uploadBudget is how long an object of size plaintext bytes may take.
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

// Result is what one Run did and found.
type Result struct {
	WorldsUploaded int   `json:"worlds_uploaded"`
	BytesUploaded  int64 `json:"bytes_uploaded"`
	// WorldsPending are present archives still without an off-site copy
	// after this run; the missing ones below are counted there alone.
	WorldsPending int `json:"worlds_pending"`
	// WorldsMissing are present rows whose archive file is not on the volume:
	// nothing to copy, and nothing a restore could use.
	WorldsMissing []string `json:"worlds_missing,omitempty"`
	WorldsExpired int      `json:"worlds_expired"`
	RemoteWorlds  int      `json:"remote_worlds"`
	RemoteBytes   int64    `json:"remote_bytes"`
	DBUploaded    int      `json:"db_uploaded"`
	DBPruned      int      `json:"db_pruned"`
	RemoteDB      int      `json:"remote_db"`
	NewestDB      string   `json:"newest_db,omitempty"`
	// ImageIndex is the newest registry index version in the bucket, which
	// lists ImageRepos repositories holding Images images.
	ImageIndex         string `json:"image_index,omitempty"`
	ImageRepos         int    `json:"image_repos"`
	Images             int    `json:"images"`
	ImagesUploaded     int    `json:"images_uploaded"`
	ImageBlobsUploaded int    `json:"image_blobs_uploaded"`
	ImageObjectsPruned int    `json:"image_objects_pruned"`
	RemoteImageBytes   int64  `json:"remote_image_bytes"`
	// ImagesIncomplete are manifests the registry lists without holding all
	// of them, so there was nothing whole to copy.
	ImagesIncomplete []string `json:"images_incomplete,omitempty"`
	// UploadIndex is the newest uploads index version in the bucket, which
	// names Uploads submission contexts.
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

// Run does one pass: database bundles, world archives, a bundle listing the
// archives just copied, registry images, submission uploads, then expiry. The
// bundles go first: they are small, and every restore starts from one. A
// failure on one item is recorded and the pass carries on; the returned error
// is non-nil when anything failed.
func (s *Syncer) Run(ctx context.Context) (Result, error) {
	var res Result
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		res.Errors = append(res.Errors, msg)
		s.logf("%s", msg)
	}

	// Before anything is written or pruned: objects sealed with another key
	// are copies only that key opens, and DBKeep would prune them; a bucket
	// another host writes is that host's to prune.
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
		// An earlier run may have stored the object and died before recording
		// it; a complete object is only recorded, not sent again.
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
	// The bucket keeps the newest keep bundles of what it holds and what is
	// here together. Local retention is per label, so an old pre-migrate
	// bundle can outlive newer dailies here that the bucket still has:
	// sending it would only see it pruned again at the end of this pass, and
	// sent again on the next.
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
	// Bundle names start with their UTC stamp, so reversed order is newest first.
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

// keptBundles picks the bundles the bucket keeps from ranked, newest first:
// the newest keep that are not snapshots, and the newest snapshot while it is
// the newest of all. A pass takes a snapshot whenever it copies archives, up to
// once an hour, so counting them in keep would push the daily bundles, the
// restore points of the last fortnight, out of the bucket within a day; one
// older than the newest daily lists nothing that daily does not.
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

// snapshotDB takes a bundle and sends it when the bucket holds an archive
// copied after its newest bundle was taken. A restore finds archives through
// the world_backups rows of the bundle it restores: an archive with no row
// there is neither fetched back nor ever expired, and until the next daily
// bundle, up to a day later, every archive copied since the last one was in
// that state. The comparison is by second, the resolution of bundle names; a
// bundle taken in the second of the copy was taken after it.
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

// sweepWorlds removes world objects no row keeps once they are older than
// OrphanAfter, the longest any archive is kept: what a restore from an older
// bundle leaves in the bucket (its archives were copied after that bundle),
// and the copy of a row whose MarkOffsite failed before the row expired.
// Younger ones are kept and reported, as the reaper does on the archive
// volume. A catalog that keeps nothing is a database not restored yet, whose
// rows would keep everything, so nothing is swept on it; an object whose age
// the bucket does not report stays.
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

// putFile encrypts the file at p into key.
func (s *Syncer) putFile(ctx context.Context, key, p string, size int64) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	// A file that changed size under us would not match the declared length;
	// LimitReader keeps the stream to the size we announced and the bucket's
	// length check catches a short one.
	return s.putStream(ctx, key, io.LimitReader(f, size), size)
}

// putStream encrypts size bytes of src into key. The sealed size is known in
// advance, so the upload streams: nothing larger than one part is buffered.
// An error from src fails the upload, and so does running past the object's
// uploadBudget.
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

// FetchResult is what Fetch did.
type FetchResult struct {
	Fetched  []string
	Present  int
	Missing  []string // present rows with no object in the bucket
	Failures []string
}

// FetchWorlds downloads every present archive the volume lacks: the volume
// half of a rebuild, after the database came back from a bundle.
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
		// World-readable like the archives the backup Jobs write: the restore
		// Job reads them as its own non-root user.
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

// alignOwner gives a restored archive the archive volume's owner, which is the
// user the backup Jobs write as, so the volume looks as they left it. Only root
// can; for anyone else the file stays theirs, readable all the same.
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

// FetchObject downloads and decrypts key into dst, created with mode. The file
// appears under its name only once it has decrypted completely; until then it
// is a hidden .partial next to it.
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

// ListDB returns the database bundles in the bucket, newest first.
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

// PeekDB reads the manifest of the database bundle name, downloading and
// decrypting only as far as the manifest. Each segment is authenticated
// before any of its bytes are read, so a manifest read this way is the one
// the bundle was sealed with.
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
	pr.Close() // Decrypt stops at its next write
	<-done
	return m, err
}

// dbChoices is how many older bundles ChooseDB names when it refuses the
// newest; `felis offsite list` shows the rest.
const dbChoices = 3

// ChooseDB picks the bundle `fetch-db latest` means: the newest, unless its
// database looks like a new install's (dbbackup.Counts.Fresh) while an older
// bundle holds more. That newest bundle is what a rebuilt host backs up and
// syncs before its restore, so it is refused with the bundles worth naming
// instead. A bundle from before counts were recorded is taken to hold more.
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

// HumanBytes formats n in binary units.
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
