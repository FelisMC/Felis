package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// cmdStageWorld is the first step of a live restore point (internal/api livebackup.go).
// While felis-api holds the running server's saving off, the backup Job's init container
// copies the world volume, mounted read-only at --from, into a scratch directory at --to;
// the moment it exits felis-api turns saving back on, and the Job's main container
// archives the copy at leisure. The game pauses its writes only for the copy, never for
// the compression, the checksum or the read-back.
//
// It copies each file to the end it reaches, so a log the server keeps appending to
// cannot fail the copy the way it fails a tar of the live volume. A file is cloned
// instead of copied (reflink, FICLONE) where the filesystem allows it, which on XFS or
// Btrfs makes the copy near-instant whatever the world's size.
func cmdStageWorld(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stage-world", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "/live", "the world volume, mounted read-only")
	to := fs.String("to", "/world", "the scratch directory the archive step reads")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	started := time.Now()
	st, err := stageWorld(*from, *to)
	if err != nil {
		fmt.Fprintf(stderr, "felis stage-world: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "felis stage-world: copied %d files (%d bytes, %d cloned) in %s\n", st.files, st.bytes, st.cloned, time.Since(started).Round(time.Millisecond))
	return 0
}

type stageStats struct {
	files, cloned int
	bytes         int64
}

// stageWorld copies the tree at from into to, which must exist and be empty: regular
// files, directories and symlinks, with their modes and modification times. Sockets,
// devices and pipes have no place in a world and are skipped. It refuses up front when
// to has less free space than from holds.
func stageWorld(from, to string) (stageStats, error) {
	return copyTree(from, to, copyOptions{})
}

// copyOptions widen copyTree for moving a whole volume (felis ha copy-volume): owner
// keeps each entry's owner and exact mode, which needs root; replace empties to first,
// so a copy that stopped half-way can be done again.
type copyOptions struct {
	owner, replace bool
}

func copyTree(from, to string, opt copyOptions) (stageStats, error) {
	var st stageStats
	entries, err := os.ReadDir(to)
	if err != nil {
		return st, err
	}
	if len(entries) > 0 && opt.replace {
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(to, e.Name())); err != nil {
				return st, err
			}
		}
		entries = nil
	}
	if len(entries) > 0 {
		return st, fmt.Errorf("%s is not empty", to)
	}
	need, err := treeSize(from)
	if err != nil {
		return st, err
	}
	if free, err := stageFreeBytes(to); err == nil && free < need {
		return st, fmt.Errorf("the scratch space has %d bytes free and the world holds %d", free, need)
	}
	type dirTime struct {
		path string
		mod  time.Time
		perm fs.FileMode
	}
	var dirs []dirTime
	err = filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file deleted between the listing and the copy is not part of the
			// frozen world: the server only deletes while saving is on.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(to, rel)
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			if rel != "." {
				if err := os.Mkdir(dest, mode.Perm()|0o700); err != nil {
					return err
				}
			}
			if opt.owner {
				if err := keepOwner(dest, info); err != nil {
					return err
				}
			}
			dirs = append(dirs, dirTime{dest, info.ModTime(), mode.Perm()})
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, dest); err != nil || !opt.owner {
				return err
			}
			return keepOwner(dest, info)
		case mode.IsRegular():
			cloned, n, err := copyFile(path, dest, mode.Perm())
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			st.files++
			st.bytes += n
			if cloned {
				st.cloned++
			}
			if opt.owner {
				if err := keepOwner(dest, info); err != nil {
					return err
				}
			}
			return os.Chtimes(dest, info.ModTime(), info.ModTime())
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	// Directory modes and times last: creating their children needed them writable, and
	// moved their times.
	for i := len(dirs) - 1; i >= 0; i-- {
		if opt.owner {
			if err := os.Chmod(dirs[i].path, dirs[i].perm); err != nil {
				return st, err
			}
		}
		if err := os.Chtimes(dirs[i].path, dirs[i].mod, dirs[i].mod); err != nil {
			return st, err
		}
	}
	return st, nil
}

// keepOwner gives dest the owner of the entry info describes (not following a link).
func keepOwner(dest string, info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return os.Lchown(dest, int(st.Uid), int(st.Gid))
}

// copyFile copies src to a new dest, cloning it when the filesystem can.
func copyFile(src, dest string, perm fs.FileMode) (cloned bool, n int64, err error) {
	in, err := os.Open(src)
	if err != nil {
		return false, 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm|0o600)
	if err != nil {
		return false, 0, err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	if cloneFile(out, in) == nil {
		info, err := out.Stat()
		if err != nil {
			return true, 0, err
		}
		return true, info.Size(), nil
	}
	n, err = io.Copy(out, in)
	return false, n, err
}

// treeSize is the bytes of every regular file under root.
func treeSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

// stageFreeBytes measures the scratch space; tests replace it.
var stageFreeBytes = freeBytes

// freeBytes is the space an unprivileged writer may still use under dir.
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
