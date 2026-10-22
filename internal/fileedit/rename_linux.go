//go:build linux

package fileedit

import (
	"os"
	"path"

	"golang.org/x/sys/unix"
)

func renameNoReplace(r *os.Root, from, to string) error {
	source, err := r.Open(path.Dir(from))
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := r.Open(path.Dir(to))
	if err != nil {
		return err
	}
	defer target.Close()
	if err := unix.Renameat2(int(source.Fd()), path.Base(from), int(target.Fd()), path.Base(to), unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
