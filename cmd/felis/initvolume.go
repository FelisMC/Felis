package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"

	"felis.lolicon.best/internal/naming"
)

// cmdInitVolume is the felis-image `prepare-data` initContainer entrypoint: it
// hands every entry of a server's world volume to the game uid/gid before the
// server container starts. The operator runs the server itself as naming.GameUID,
// so a world written by an earlier release (whose server ran as root), a restore
// Job (which extracts as root), or a storage provisioner that creates the volume
// root-owned would otherwise leave files the server cannot write — a world that
// boots and then fails every save.
//
// fsGroup covers only part of this: kubelet applies it to volume types that
// support ownership management, and a k3s local-path PV is a hostPath underneath,
// which it skips. A walk from inside the pod works for every volume type.
//
// Only mismatched entries are touched, so a volume already owned by the game uid
// costs one lstat per entry and no writes. The walk runs inside an os.Root at the
// data dir and uses lchown, so a symlink a plugin planted is re-owned as a link
// and never followed out of the volume.
//
// A single entry that cannot be chowned is reported and skipped: failing the pod
// over one odd file would keep the whole server down, while the server itself
// reports the one file it cannot write. Only an unreadable data dir fails.
func cmdInitVolume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init-volume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", defaultForwardingDataDir, "world volume mount to hand to the game uid")
	uid := fs.Int64("uid", naming.GameUID, "owner uid for every entry")
	gid := fs.Int64("gid", naming.GameGID, "owner gid for every entry")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, err := os.OpenRoot(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "felis init-volume: open %s: %v\n", *dataDir, err)
		return 1
	}
	defer root.Close()
	res, err := chownTree(root, int(*uid), int(*gid), root.Lchown)
	if err != nil {
		fmt.Fprintf(stderr, "felis init-volume: %v\n", err)
		return 1
	}
	for _, f := range res.failures {
		fmt.Fprintf(stderr, "felis init-volume: %s\n", f)
	}
	fmt.Fprintf(stdout, "felis init-volume: %d entries checked, %d handed to %d:%d, %d failed\n",
		res.checked, res.changed, *uid, *gid, len(res.failures))
	return 0
}

// chownResult tallies one walk; failures is capped so a volume of thousands of
// unownable files cannot flood the pod log.
type chownResult struct {
	checked  int
	changed  int
	failures []string
}

const maxReportedChownFailures = 20

// chownTree walks root and calls chown on every entry (the root dir included)
// whose owner is not uid:gid. It returns an error only when the root itself
// cannot be read; per-entry failures are collected in the result.
func chownTree(root *os.Root, uid, gid int, chown func(name string, uid, gid int) error) (chownResult, error) {
	var res chownResult
	fail := func(name string, err error) {
		if len(res.failures) < maxReportedChownFailures {
			res.failures = append(res.failures, fmt.Sprintf("%s: %v", name, err))
		} else if len(res.failures) == maxReportedChownFailures {
			res.failures = append(res.failures, "further failures not listed")
		}
	}
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if name == "." {
				return walkErr
			}
			fail(name, walkErr)
			// A directory that cannot be listed is skipped as a whole; a file
			// error has nothing below it to skip.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		res.checked++
		info, err := d.Info()
		if err != nil {
			fail(name, err)
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid && int(st.Gid) == gid {
			return nil
		}
		if err := chown(name, uid, gid); err != nil {
			if !errors.Is(err, fs.ErrNotExist) { // gone mid-walk: nothing left to own
				fail(name, err)
			}
			return nil
		}
		res.changed++
		return nil
	})
	return res, err
}
