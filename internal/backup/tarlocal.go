package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// TarLocal is the zero-storageClass-requirement backend (spec §19): it mounts
// the source PVC, tars+gzips it, and writes the archive into the backup PVC.
// It runs on any StorageClass, including hostPath-style local-path, where the
// snapshot backends cannot.
type TarLocal struct {
	// BackupRoot is the directory (backup PVC mount) archives are written into.
	BackupRoot string
	// Resolve maps a PVC name to its mounted filesystem path.
	Resolve PVCResolver
	// Now is injectable for deterministic archive names in tests.
	Now func() time.Time
}

func (t *TarLocal) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// partialSuffix marks an archive still being written. Its file is the final
// name hidden behind a leading dot, so a listing of the store shows finished
// archives only and a leftover still says whose it was.
const partialSuffix = ".partial"

// archiveName matches the finished archives Archive names (<server>-<unix
// nanoseconds>.tar.gz); partialName matches their unfinished form. Sweep touches
// nothing else in the store.
var (
	archiveName = regexp.MustCompile(`^[^.].*-[0-9]+\.tar\.gz$`)
	partialName = regexp.MustCompile(`^\..*-[0-9]+\.tar\.gz\.partial$`)
)

// Archive tars+gzips the world on pvc into BackupRoot and returns the archive
// path as the opaque ref, with its size and SHA256.
//
// The archive is written under its hidden .partial name, flushed to disk, read
// back in full, and only then renamed to its final name and the rename flushed
// too. A Job killed at its deadline or a node that loses power mid-write leaves
// a .partial for Sweep, never a truncated file under a name that looks finished,
// and the world is deleted only after an archive that has already been read
// back whole.
func (t *TarLocal) Archive(ctx context.Context, server, pvc string) (Archived, error) {
	srcDir, err := t.Resolve(pvc)
	if err != nil {
		return Archived{}, err
	}
	if err := os.MkdirAll(t.BackupRoot, 0o750); err != nil {
		return Archived{}, fmt.Errorf("backup: mkdir backup root: %w", err)
	}
	name := fmt.Sprintf("%s-%d.tar.gz", server, t.now().UTC().UnixNano())
	dest := filepath.Join(t.BackupRoot, name)
	tmp := filepath.Join(t.BackupRoot, "."+name+partialSuffix)

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return Archived{}, fmt.Errorf("backup: create archive: %w", err)
	}
	h := sha256.New()
	st, err := writeTarGz(ctx, io.MultiWriter(f, h), srcDir)
	if err == nil {
		if err = f.Sync(); err != nil {
			err = fmt.Errorf("backup: sync archive: %w", err)
		}
	}
	if cerr := f.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("backup: close archive: %w", cerr)
	}
	if err != nil {
		os.Remove(tmp)
		return Archived{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))

	// Read back what landed: the gzip CRC, every tar header and entry, and the
	// entry count must all come out as written.
	entries, _, err := verifyArchive(ctx, tmp, sum)
	if err == nil && entries != st.entries {
		err = fmt.Errorf("%w: %s: %d entries read back, %d written", ErrCorrupt, tmp, entries, st.entries)
	}
	if err != nil {
		os.Remove(tmp)
		return Archived{}, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return Archived{}, fmt.Errorf("backup: name archive: %w", err)
	}
	if err := syncDir(t.BackupRoot); err != nil {
		os.Remove(dest)
		return Archived{}, fmt.Errorf("backup: sync backup root: %w", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		return Archived{}, fmt.Errorf("backup: stat archive: %w", err)
	}
	return Archived{Ref: ArchiveRef(dest), Size: info.Size(), SHA256: sum, Skipped: st.skipped}, nil
}

// Verify reads the archive at ref back end to end (see verifyArchive).
func (t *TarLocal) Verify(ctx context.Context, ref ArchiveRef, want string) (string, error) {
	_, sum, err := verifyArchive(ctx, string(ref), want)
	return sum, err
}

// verifyArchive reads the archive at p through gzip and tar to the last byte and
// returns its entry count and SHA256. Anything that does not read back — a
// missing file, a torn gzip stream, a CRC or length mismatch, a bad tar header,
// or a digest other than want (when want is not "") — is ErrCorrupt. Only an
// error opening a file that is there, or the context, is not.
func verifyArchive(ctx context.Context, p, want string) (int, string, error) {
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, "", fmt.Errorf("%w: %s is missing", ErrCorrupt, p)
	}
	if err != nil {
		return 0, "", fmt.Errorf("backup: open archive: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	raw := io.TeeReader(f, h)
	corrupt := func(err error) (int, string, error) {
		if ctx.Err() != nil {
			return 0, "", ctx.Err()
		}
		return 0, "", fmt.Errorf("%w: %s: %v", ErrCorrupt, p, err)
	}

	gz, err := gzip.NewReader(raw)
	if err != nil {
		return corrupt(err)
	}
	tr := tar.NewReader(gz)
	entries := 0
	for {
		if ctx.Err() != nil {
			return 0, "", ctx.Err()
		}
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return corrupt(err)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return corrupt(err)
		}
		entries++
	}
	// The tar end marker is not the end of the gzip member: reading on to EOF is
	// what checks the CRC and length in the gzip trailer.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return corrupt(err)
	}
	if err := gz.Close(); err != nil {
		return corrupt(err)
	}
	if _, err := io.Copy(io.Discard, raw); err != nil {
		return corrupt(err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if want != "" && sum != want {
		return 0, sum, fmt.Errorf("%w: %s: sha256 %s, recorded %s", ErrCorrupt, p, sum, want)
	}
	return entries, sum, nil
}

// Sweep removes the leftovers of archives that never finished (see Sweeper). It
// only looks at the top level of BackupRoot and only at names Archive writes;
// partialBefore must leave room for the longest Archive, and orphanBefore for
// the insert of the record that follows it at the very least.
func (t *TarLocal) Sweep(ctx context.Context, live func(ArchiveRef) bool, partialBefore, orphanBefore time.Time) (Swept, error) {
	var out Swept
	ents, err := os.ReadDir(t.BackupRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("backup: list backup root: %w", err)
	}
	var errs []error
	for _, e := range ents {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		name := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(t.BackupRoot, name)
		cutoff := partialBefore
		switch {
		case partialName.MatchString(name):
		case archiveName.MatchString(name):
			if live(ArchiveRef(p)) {
				continue
			}
			cutoff = orphanBefore
		default:
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(partialBefore) {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			out.Orphans = append(out.Orphans, p)
			out.OrphanBytes += info.Size()
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		out.Removed = append(out.Removed, p)
	}
	return out, errors.Join(errs...)
}

// syncDir flushes a change to a directory's entries (a rename) to disk. A
// filesystem that cannot sync a directory has no stronger promise to give.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

// Restore extracts the archive at ref into the world mount for targetPVC,
// replacing the target's contents so the world equals the archive (spec §466
// "restore PVC": a rollback must not leave stale files the backup lacks — e.g. a
// griefer's chunks). It extracts over the target, then removes any pre-existing
// entry the archive did not contain.
//
// The archive is read back in full before anything is extracted, so a corrupt
// or truncated archive fails with the world untouched. The prune runs only after
// a fully successful extract: an extract that still fails (the volume fills up)
// leaves the target as a recoverable partial overlay rather than a destroyed
// world. The archive is retained on restore, so such a failure is recoverable by
// re-running the Job.
//
// Files and directories get back the permission bits and modification times the
// archive recorded. Ownership is left to the server: every start re-owns the
// world volume to the game uid (felis init-volume).
//
// A top-level lost+found is never a prune target. It is a filesystem artifact
// (root-owned, mode 0700) that the non-root restore Pod cannot delete anyway,
// and writeTarGz includes it in the archive, so it is preserved on both axes.
func (t *TarLocal) Restore(ctx context.Context, ref ArchiveRef, targetPVC string) error {
	dstDir, err := t.Resolve(targetPVC)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return fmt.Errorf("backup: mkdir restore target: %w", err)
	}
	if _, _, err := verifyArchive(ctx, string(ref), ""); err != nil {
		return err
	}
	f, err := os.Open(string(ref))
	if err != nil {
		return fmt.Errorf("backup: open archive: %w", err)
	}
	defer f.Close()

	keep, dirs, err := readTarGz(ctx, f, dstDir)
	if err != nil {
		return err
	}
	if err := pruneToManifest(dstDir, keep); err != nil {
		return err
	}
	return restoreDirMeta(dirs)
}

// pruneToManifest removes every entry under dstDir whose archive-relative path
// is absent from keep, giving Restore replace semantics. keep holds cleaned,
// forward-slash relative paths (no trailing slash) for every archive entry plus
// all of their ancestor directories, so a kept file's parent dirs are never
// removed. A top-level lost+found is always kept. dstDir (the mount root) is
// never removed.
func pruneToManifest(dstDir string, keep map[string]struct{}) error {
	cleanDst := filepath.Clean(dstDir)
	return filepath.WalkDir(cleanDst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == cleanDst {
			return nil // never remove the mount root itself
		}
		relNative, err := filepath.Rel(cleanDst, p)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relNative)
		if rel == "lost+found" {
			if d.IsDir() {
				return filepath.SkipDir // filesystem artifact: keep and don't descend
			}
			return nil
		}
		if _, ok := keep[rel]; ok {
			return nil // the archive contained this path: keep it
		}
		// Stale: present in the target but absent from the archive.
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("backup: prune stale entry %q: %w", rel, err)
		}
		if d.IsDir() {
			return filepath.SkipDir // already removed; don't descend into it
		}
		return nil
	})
}

// Delete removes the tar archive at ref.
func (t *TarLocal) Delete(_ context.Context, ref ArchiveRef) error {
	if err := os.Remove(string(ref)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("backup: delete archive: %w", err)
	}
	return nil
}

// tarStats is what writeTarGz put in the archive and what it left out.
type tarStats struct {
	entries int
	skipped []string
}

// writeTarGz archives srcDir (not the root entry itself) as gzip+tar. Each entry
// keeps its permission bits, without setuid, setgid and sticky, and its
// modification time. Entries other than regular files and directories are left
// out and listed in the stats.
func writeTarGz(ctx context.Context, w io.Writer, srcDir string) (tarStats, error) {
	var st tarStats
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	root := filepath.Clean(srcDir)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // don't archive the root entry itself
		}
		// Normalize to forward slashes so archives are portable.
		name := filepath.ToSlash(rel)

		mode := int64(info.Mode().Perm())
		switch {
		case info.IsDir():
			hdr := &tar.Header{Name: name + "/", Mode: mode, ModTime: info.ModTime(), Typeflag: tar.TypeDir}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			st.entries++
			return nil
		case info.Mode().IsRegular():
			hdr := &tar.Header{Name: name, Mode: mode, ModTime: info.ModTime(), Size: info.Size(), Typeflag: tar.TypeReg}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			src, err := os.Open(path)
			if err != nil {
				return err
			}
			defer src.Close()
			if _, err := io.Copy(tw, src); err != nil {
				return err
			}
			st.entries++
			return nil
		default:
			// A symlink could point anywhere on the node, and a device or socket
			// has no bytes to keep: a world is plain files. What is left out is
			// reported, so a backup never drops something silently.
			st.skipped = append(st.skipped, name)
			return nil
		}
	})
	if err != nil {
		return st, fmt.Errorf("backup: tar walk: %w", err)
	}
	if err := tw.Close(); err != nil {
		return st, fmt.Errorf("backup: close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return st, fmt.Errorf("backup: close gzip: %w", err)
	}
	return st, nil
}

// WriteTarGz archives srcDir into w laid out exactly as Archive lays out a
// backup, so an exported world restores like any other archive, and returns the
// entries it left out. The world export Job streams it straight into its upload.
func WriteTarGz(ctx context.Context, w io.Writer, srcDir string) ([]string, error) {
	st, err := writeTarGz(ctx, w, srcDir)
	return st.skipped, err
}

// dirMeta is a directory's recorded permission bits and modification time,
// applied once nothing more is written into it.
type dirMeta struct {
	path  string
	mode  fs.FileMode
	mtime time.Time
}

// readTarGz extracts the gzip+tar stream into dstDir and returns the keep-set:
// the cleaned, forward-slash relative path of every entry the archive contained
// plus all of their ancestor directories. The caller uses it to prune stale
// target files for replace semantics. On any error the keep-set is incomplete
// and must not be used to prune (a partial manifest would delete live files the
// stream had not yet reached).
//
// Files get their recorded mode and modification time as they are written. A
// directory's are returned instead (restoreDirMeta): extracting and pruning
// inside it would move its mtime again, and a read-only mode would stop the
// extract from writing into it.
func readTarGz(ctx context.Context, r io.Reader, dstDir string) (map[string]struct{}, []dirMeta, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	keep := make(map[string]struct{})
	var dirs []dirMeta
	cleanDst := filepath.Clean(dstDir)
	for {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return keep, dirs, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("backup: read tar: %w", err)
		}

		// Guard against path traversal (zip-slip): the resolved target must stay
		// within dstDir.
		target := filepath.Join(cleanDst, filepath.FromSlash(hdr.Name))
		if target != cleanDst && !strings.HasPrefix(target, cleanDst+string(os.PathSeparator)) {
			return nil, nil, fmt.Errorf("backup: archive entry escapes target: %q", hdr.Name)
		}

		// Record this entry and its ancestors in the keep-set. Names are stored
		// in archive form (forward slash, no trailing slash) to match the
		// relative paths pruneToManifest derives from the on-disk walk.
		rememberKept(keep, hdr.Name)

		mode := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return nil, nil, err
			}
			if target != cleanDst {
				dirs = append(dirs, dirMeta{path: target, mode: mode, mtime: hdr.ModTime})
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return nil, nil, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return nil, nil, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return nil, nil, err
			}
			if err := out.Close(); err != nil {
				return nil, nil, err
			}
			// Chmod rather than the create mode: the umask would narrow it, and
			// a file that already existed keeps its old mode through O_TRUNC.
			if err := os.Chmod(target, mode); err != nil {
				return nil, nil, err
			}
			if recordedTime(hdr.ModTime) {
				if err := os.Chtimes(target, hdr.ModTime, hdr.ModTime); err != nil {
					return nil, nil, err
				}
			}
		default:
			// Ignore entry types tarLocal never writes.
		}
	}
}

// restoreDirMeta applies the directories' recorded modes and times, deepest
// first, so setting a parent's time comes after every change inside it.
func restoreDirMeta(dirs []dirMeta) error {
	sort.SliceStable(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, d := range dirs {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return fmt.Errorf("backup: restore mode of %s: %w", d.path, err)
		}
		if recordedTime(d.mtime) {
			if err := os.Chtimes(d.path, d.mtime, d.mtime); err != nil {
				return fmt.Errorf("backup: restore time of %s: %w", d.path, err)
			}
		}
	}
	return nil
}

// recordedTime reports whether an archive recorded a modification time. Archives
// written before times were kept carry the Unix epoch, which is left alone
// rather than stamped onto every file.
func recordedTime(t time.Time) bool { return t.Unix() > 0 }

// rememberKept adds an archive entry name and every ancestor directory to keep,
// normalized to a cleaned forward-slash path with no trailing slash. Adding
// ancestors guards against archives that list a file without an explicit entry
// for its parent dir: the dir must still survive the prune.
func rememberKept(keep map[string]struct{}, name string) {
	rel := path.Clean(strings.TrimSuffix(name, "/"))
	for rel != "." && rel != "/" && rel != "" {
		keep[rel] = struct{}{}
		rel = path.Dir(rel)
	}
}
