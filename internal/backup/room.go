package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// MinFreeAfter is the share of the archive filesystem an on-demand backup must
// leave free. The archive store sits on the node's disk beside the worlds and
// the database; below about a tenth free the kubelet starts evicting pods
// (docs/troubleshooting.md §13b), so a backup that would cross it is refused.
const MinFreeAfter = 0.10

// ErrNoRoom is returned by CheckRoom when the archive would leave too little
// free space.
var ErrNoRoom = errors.New("backup: not enough free disk for the archive")

// CheckRoom refuses an archive of srcDir into archiveDir that could push the
// archive filesystem below minFree free. The world's uncompressed size stands
// in for the archive's, which gzip only makes smaller. archiveDir need not
// exist yet; its nearest existing parent is measured.
func CheckRoom(archiveDir, srcDir string, minFree float64) error {
	dir := archiveDir
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fmt.Errorf("backup: no existing directory above %s", archiveDir)
		}
		dir = parent
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("backup: measure %s: %w", dir, err)
	}
	bsize := uint64(st.Bsize) // uint32 on darwin
	total := uint64(st.Blocks) * bsize
	avail := uint64(st.Bavail) * bsize
	if total == 0 {
		return nil
	}
	need, err := treeBytes(srcDir)
	if err != nil {
		return err
	}
	floor := uint64(float64(total) * minFree)
	if avail < need || avail-need < floor {
		return fmt.Errorf("%w: the world is %s and %s is free of %s, which would leave less than %.0f%% free",
			ErrNoRoom, byteSize(need), byteSize(avail), byteSize(total), minFree*100)
	}
	return nil
}

// treeBytes sums the sizes of the regular files under dir.
func treeBytes(dir string) (uint64, error) {
	var n uint64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			n += uint64(info.Size())
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("backup: measure the world: %w", err)
	}
	return n, nil
}

func byteSize(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
