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

type TarLocal struct {
	BackupRoot string
	Resolve    PVCResolver
	Now        func() time.Time
}

func (t *TarLocal) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

const partialSuffix = ".partial"

var (
	archiveName = regexp.MustCompile(`^[^.].*-[0-9]+\.tar\.gz$`)
	partialName = regexp.MustCompile(`^\..*-[0-9]+\.tar\.gz\.partial$`)
)

// Archive syncs and verifies a hidden partial before final rename, then syncs the directory so incomplete writes never look finished.
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
	st, err := writeTarGz(ctx, io.MultiWriter(f, h), srcDir, nil)
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

func (t *TarLocal) Verify(ctx context.Context, ref ArchiveRef, want string) (string, error) {
	_, sum, err := verifyArchive(ctx, string(ref), want)
	return sum, err
}

// verifyArchive reads through gzip EOF and validates tar entries and optional digest; open or cancellation errors remain distinct from corruption.
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
	// Read past the tar end marker to verify the gzip trailer CRC and length.
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

// Sweep only touches recognized top-level archive names; age thresholds must allow active writes and delayed row inserts to finish.
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

// Restore verifies first, extracts, then prunes stale files only after success; failures retain the archive and may leave a retryable partial overlay.
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

// pruneToManifest preserves the mount root, top-level lost+found and ancestors of every kept archive entry.
func pruneToManifest(dstDir string, keep map[string]struct{}) error {
	cleanDst := filepath.Clean(dstDir)
	return filepath.WalkDir(cleanDst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == cleanDst {
			return nil
		}
		relNative, err := filepath.Rel(cleanDst, p)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relNative)
		if rel == "lost+found" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := keep[rel]; ok {
			return nil
		}
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("backup: prune stale entry %q: %w", rel, err)
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

func (t *TarLocal) Delete(_ context.Context, ref ArchiveRef) error {
	if err := os.Remove(string(ref)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("backup: delete archive: %w", err)
	}
	return nil
}

type tarStats struct {
	entries  int
	skipped  []string
	withheld []string
}

// writeTarGz preserves regular-file and directory modes and times while stripping special bits and reporting skipped unsupported entries.
func writeTarGz(ctx context.Context, w io.Writer, srcDir string, filter Filter) (tarStats, error) {
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
			return nil
		}
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
			var rewrite func([]byte) []byte
			if filter != nil {
				var withhold bool
				if withhold, rewrite = filter(name, info); withhold {
					st.withheld = append(st.withheld, name)
					return nil
				}
			}
			if rewrite != nil {
				content, ok, err := readRewritable(path)
				if err != nil {
					return err
				}
				if !ok {
					st.withheld = append(st.withheld, name)
					return nil
				}
				content = rewrite(content)
				hdr := &tar.Header{Name: name, Mode: mode, ModTime: info.ModTime(), Size: int64(len(content)), Typeflag: tar.TypeReg}
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
				if _, err := tw.Write(content); err != nil {
					return err
				}
				st.entries++
				return nil
			}
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

// Apply directory metadata only after all extraction and pruning writes finish.
type dirMeta struct {
	path  string
	mode  fs.FileMode
	mtime time.Time
}

// readTarGz returns an incomplete keep-set on failure; never prune from it, and apply directory metadata only after extraction.
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

		target := filepath.Join(cleanDst, filepath.FromSlash(hdr.Name))
		if target != cleanDst && !strings.HasPrefix(target, cleanDst+string(os.PathSeparator)) {
			return nil, nil, fmt.Errorf("backup: archive entry escapes target: %q", hdr.Name)
		}

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
			// Chmod explicitly because umask filters creation permissions and truncation retains existing file modes.
			if err := os.Chmod(target, mode); err != nil {
				return nil, nil, err
			}
			if recordedTime(hdr.ModTime) {
				if err := os.Chtimes(target, hdr.ModTime, hdr.ModTime); err != nil {
					return nil, nil, err
				}
			}
		default:
		}
	}
}

// restoreDirMeta visits deepest directories first so parent timestamps survive child updates.
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

// recordedTime ignores epoch timestamps from archives that predate modification-time recording.
func recordedTime(t time.Time) bool { return t.Unix() > 0 }

// rememberKept includes parent directories even when the archive lacks explicit directory entries.
func rememberKept(keep map[string]struct{}, name string) {
	rel := path.Clean(strings.TrimSuffix(name, "/"))
	for rel != "." && rel != "/" && rel != "" {
		keep[rel] = struct{}{}
		rel = path.Dir(rel)
	}
}
