// Package offsite keeps a second copy of what a lost node would take with it:
// every world archive (world_backups) and the newest control-plane database
// bundles (internal/dbbackup), encrypted, in an S3-compatible bucket off the
// machine. `felis offsite sync` runs it from felis-offsite.timer on the host,
// which is where both the archive volume and the bundle directory live.
//
// The database records the copy: world_backups.offsite_at is set once an
// archive's object is in the bucket, and with [offsite] configured the reaper
// deletes an idle world only after that (internal/reaper). Remote world
// objects go when their row has expired, so the bucket keeps each archive for
// the same retention the panel promises, including archives evicted early from
// the local disk to make room. Remote bundles are pruned to the newest DBKeep.
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
	// bucket keeps.
	DBDir  string
	DBKeep int
	Now    func() time.Time
	Log    io.Writer
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
	Errors        []string `json:"errors,omitempty"`
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

// Run does one pass: world archives, then database bundles, then expiry. A
// failure on one item is recorded and the pass carries on; the returned error
// is non-nil when anything failed.
func (s *Syncer) Run(ctx context.Context) (Result, error) {
	var res Result
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		res.Errors = append(res.Errors, msg)
		s.logf("%s", msg)
	}

	remoteWorlds, err := s.listSizes(ctx, worldsDir)
	if err != nil {
		return res, fmt.Errorf("list %s in the bucket: %w", worldsDir, err)
	}
	s.syncWorlds(ctx, remoteWorlds, &res, fail)
	s.syncDB(ctx, &res, fail)
	s.expireWorlds(ctx, remoteWorlds, &res, fail)

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
	kept := map[string]bool{}
	for _, name := range ranked[:min(keep, len(ranked))] {
		kept[name] = true
	}
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
	for _, name := range ranked {
		if _, ok := remote[DBKey(name)]; ok {
			res.RemoteDB++
			if res.NewestDB == "" {
				res.NewestDB = name
			}
		}
	}
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

// putFile encrypts the file at p into key. The sealed size is known in
// advance, so the upload streams: nothing larger than one part is buffered.
func (s *Syncer) putFile(ctx context.Context, key, p string, size int64) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	go func() {
		// A file that changed size under us would not match the declared
		// length; LimitReader keeps the stream to the size we announced and
		// the length check below catches a short one.
		err := Encrypt(pw, io.LimitReader(f, size), s.Key)
		pw.CloseWithError(err)
	}()
	err = s.Bucket.Put(ctx, key, pr, SealedSize(size))
	pr.CloseWithError(errors.New("upload finished"))
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
