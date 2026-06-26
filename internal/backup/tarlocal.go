package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
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

// Archive tars+gzips the world on pvc into BackupRoot and returns the archive
// path as the opaque ref plus its on-disk size.
func (t *TarLocal) Archive(ctx context.Context, server, pvc string) (ArchiveRef, int64, error) {
	srcDir, err := t.Resolve(pvc)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(t.BackupRoot, 0o750); err != nil {
		return "", 0, fmt.Errorf("backup: mkdir backup root: %w", err)
	}
	name := fmt.Sprintf("%s-%d.tar.gz", server, t.now().UTC().UnixNano())
	dest := filepath.Join(t.BackupRoot, name)

	f, err := os.Create(dest)
	if err != nil {
		return "", 0, fmt.Errorf("backup: create archive: %w", err)
	}
	if err := writeTarGz(ctx, f, srcDir); err != nil {
		f.Close()
		os.Remove(dest)
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		os.Remove(dest)
		return "", 0, fmt.Errorf("backup: close archive: %w", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		return "", 0, fmt.Errorf("backup: stat archive: %w", err)
	}
	return ArchiveRef(dest), info.Size(), nil
}

// Restore extracts the archive at ref into the world mount for targetPVC,
// replacing the target's contents so the world equals the archive (spec §466
// "restore PVC": a rollback must not leave stale files the backup lacks — e.g. a
// griefer's chunks). It extracts over the target, then removes any pre-existing
// entry the archive did not contain.
//
// The prune runs only after a fully successful extract: a corrupt or truncated
// archive fails before the prune, leaving the target as a (recoverable) partial
// overlay rather than a destroyed world. The archive is retained on restore, so
// such a failure is recoverable by re-running the Job.
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
	f, err := os.Open(string(ref))
	if err != nil {
		return fmt.Errorf("backup: open archive: %w", err)
	}
	defer f.Close()

	keep, err := readTarGz(ctx, f, dstDir)
	if err != nil {
		return err
	}
	return pruneToManifest(dstDir, keep)
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

func writeTarGz(ctx context.Context, w io.Writer, srcDir string) error {
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

		switch {
		case info.IsDir():
			hdr := &tar.Header{Name: name + "/", Mode: 0o750, Typeflag: tar.TypeDir}
			return tw.WriteHeader(hdr)
		case info.Mode().IsRegular():
			hdr := &tar.Header{Name: name, Mode: 0o640, Size: info.Size(), Typeflag: tar.TypeReg}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			src, err := os.Open(path)
			if err != nil {
				return err
			}
			defer src.Close()
			_, err = io.Copy(tw, src)
			return err
		default:
			// Skip symlinks/devices/sockets: a world directory should be plain
			// files, and refusing the rest avoids surprising archive contents.
			return nil
		}
	})
	if err != nil {
		return fmt.Errorf("backup: tar walk: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("backup: close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("backup: close gzip: %w", err)
	}
	return nil
}

// readTarGz extracts the gzip+tar stream into dstDir and returns the keep-set:
// the cleaned, forward-slash relative path of every entry the archive contained
// plus all of their ancestor directories. The caller uses it to prune stale
// target files for replace semantics. On any error the keep-set is incomplete
// and must not be used to prune (a partial manifest would delete live files the
// stream had not yet reached).
func readTarGz(ctx context.Context, r io.Reader, dstDir string) (map[string]struct{}, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("backup: open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	keep := make(map[string]struct{})
	cleanDst := filepath.Clean(dstDir)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return keep, nil
		}
		if err != nil {
			return nil, fmt.Errorf("backup: read tar: %w", err)
		}

		// Guard against path traversal (zip-slip): the resolved target must stay
		// within dstDir.
		target := filepath.Join(cleanDst, filepath.FromSlash(hdr.Name))
		if target != cleanDst && !strings.HasPrefix(target, cleanDst+string(os.PathSeparator)) {
			return nil, fmt.Errorf("backup: archive entry escapes target: %q", hdr.Name)
		}

		// Record this entry and its ancestors in the keep-set. Names are stored
		// in archive form (forward slash, no trailing slash) to match the
		// relative paths pruneToManifest derives from the on-disk walk.
		rememberKept(keep, hdr.Name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return nil, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return nil, err
			}
			if err := out.Close(); err != nil {
				return nil, err
			}
		default:
			// Ignore entry types tarLocal never writes.
		}
	}
}

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
