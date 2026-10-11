package main

import (
	"os"
	"syscall"
)

// ficlone is the FICLONE ioctl: dst shares src's extents until either is written.
const ficlone = 0x40049409

// cloneFile reflinks src into dst where the filesystem supports it (XFS, Btrfs).
func cloneFile(dst, src *os.File) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficlone, src.Fd()); errno != 0 {
		return errno
	}
	return nil
}
