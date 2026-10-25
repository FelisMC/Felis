package fileedit

import (
	"errors"
	"io"
	"math"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const (
	diskUsageMaxEntries  = 1_000_000
	diskUsageMaxDepth    = 128
	diskUsageBatch       = 256
	diskUsagePause       = 5 * time.Millisecond
	diskUsageScanTimeout = 45 * time.Second
	diskUsageJobDeadline = 75 * time.Second
	diskUsageTimeout     = 70 * time.Second
	diskUsageCPU         = "100m"
	diskUsageMemory      = "96Mi"
)

var (
	errDiskUsageLimit    = errors.New("disk usage sampling exceeded its limits")
	errDiskUsageChanged  = errors.New("the world changed during disk usage sampling")
	errDiskUsageBoundary = errors.New("disk usage sampling cannot cross a filesystem boundary")
	errDiskUsageOverflow = errors.New("disk usage exceeds the supported byte count")
)

type diskInode struct{ device, inode uint64 }

type diskUsageScan struct {
	deadline  time.Time
	device    uint64
	remaining int
	seen      map[diskInode]struct{}
	used      int64
}

func diskUsage(r *os.Root) Result {
	used, err := measureDiskUsage(r, diskUsageMaxEntries, time.Now().Add(diskUsageScanTimeout))
	if err != nil {
		code := CodeBadPath
		switch {
		case errors.Is(err, errDiskUsageLimit), errors.Is(err, errDiskUsageOverflow):
			code = CodeTooLarge
		case errors.Is(err, errDiskUsageChanged):
			code = CodeConflict
		}
		return Result{Code: code, Error: "the world could not be fully sampled"}
	}
	return Result{UsedBytes: &used}
}

func measureDiskUsage(r *os.Root, maxEntries int, deadline time.Time) (int64, error) {
	scan := diskUsageScan{deadline: deadline, remaining: maxEntries, seen: make(map[diskInode]struct{})}
	if err := scan.checkDeadline(); err != nil {
		return 0, err
	}
	dir, err := r.Open(".")
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &stat); err != nil {
		return 0, err
	}
	scan.device = uint64(stat.Dev)
	if err := scan.add(stat); err != nil {
		return 0, err
	}
	if err := scan.walk(dir, 0); err != nil {
		return 0, err
	}
	if err := scan.checkDeadline(); err != nil {
		return 0, err
	}
	return scan.used, nil
}

func (s *diskUsageScan) checkDeadline() error {
	if !time.Now().Before(s.deadline) {
		return errDiskUsageLimit
	}
	return nil
}

func (s *diskUsageScan) add(stat unix.Stat_t) error {
	if err := s.checkDeadline(); err != nil {
		return err
	}
	if s.remaining <= 0 {
		return errDiskUsageLimit
	}
	s.remaining--
	if uint64(stat.Dev) != s.device {
		return errDiskUsageBoundary
	}
	if stat.Nlink > 1 || stat.Mode&unix.S_IFMT == unix.S_IFDIR {
		key := diskInode{uint64(stat.Dev), uint64(stat.Ino)}
		if _, ok := s.seen[key]; ok {
			if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
				return errDiskUsageChanged
			}
			return nil
		}
		s.seen[key] = struct{}{}
	}
	// st_blocks counts allocated 512-byte units, including directory and link storage.
	if stat.Blocks < 0 || stat.Blocks > (math.MaxInt64-s.used)/512 {
		return errDiskUsageOverflow
	}
	s.used += stat.Blocks * 512
	return nil
}

func (s *diskUsageScan) walk(dir *os.File, depth int) error {
	if err := s.checkDeadline(); err != nil {
		return err
	}
	before, err := dir.Stat()
	if err != nil {
		return err
	}
	for {
		if err := s.checkDeadline(); err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(diskUsageBatch)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			var stat unix.Stat_t
			if err := unix.Fstatat(int(dir.Fd()), entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
			if err := s.add(stat); err != nil {
				return err
			}
			if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
				if depth >= diskUsageMaxDepth {
					return errDiskUsageLimit
				}
				if err := s.walkChild(dir, entry.Name(), stat, depth+1); err != nil {
					return err
				}
			}
			if s.remaining%diskUsageBatch == 0 {
				time.Sleep(diskUsagePause)
				if err := s.checkDeadline(); err != nil {
					return err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	after, err := dir.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		return errDiskUsageChanged
	}
	return s.checkDeadline()
}

func (s *diskUsageScan) walkChild(parent *os.File, name string, before unix.Stat_t, depth int) error {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), "world directory")
	defer dir.Close()
	var current unix.Stat_t
	if err := unix.Fstat(fd, &current); err != nil {
		return err
	}
	if before.Dev != current.Dev || before.Ino != current.Ino {
		return errDiskUsageChanged
	}
	return s.walk(dir, depth)
}
