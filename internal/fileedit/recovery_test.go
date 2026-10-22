package fileedit

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenameNoReplacePreservesExistingTargets(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "directory"}[directory], func(t *testing.T) {
			root := t.TempDir()
			if directory {
				putTransferFile(t, root, "source/item.txt", "source")
				if err := os.Mkdir(filepath.Join(root, "target"), 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				putTransferFile(t, root, "source", "source")
				putTransferFile(t, root, "target", "target")
			}
			r, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			before := tree(t, root)
			if err := renameNoReplace(r, "source", "target"); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("rename onto an existing target: %v", err)
			}
			assertSameTree(t, before, tree(t, root))
		})
	}
}

func TestCopyFailurePreservesConcurrentlyRecreatedTarget(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, "from/item.txt", "source")
	putTransferFile(t, root, "target/item.txt", "previous")
	oldRename := renameEntry
	defer func() { renameEntry = oldRename }()
	renameEntry = func(r *os.Root, from, to string) error {
		err := oldRename(r, from, to)
		if err == nil && from == "target/item.txt" {
			putTransferFile(t, root, "target/item.txt", "concurrent target")
		}
		return err
	}
	res, err := Execute(root, Request{Op: OpCopy, Paths: []string{"from/item.txt"}, To: "target", Overwrite: true})
	if err != nil || res.Code != CodeRecovery || len(res.RecoveryPaths) != 1 || !strings.Contains(res.Error, "file-recovery-") {
		t.Fatalf("copy with recreated target: %+v %v", res, err)
	}
	body, _ := os.ReadFile(filepath.Join(root, "target/item.txt"))
	if string(body) != "concurrent target" {
		t.Fatalf("concurrent target was overwritten: %q", body)
	}
	backups, _ := filepath.Glob(filepath.Join(root, "target/file-recovery-*/previous/item.txt"))
	if len(backups) != 1 {
		t.Fatalf("previous target was discarded: %v", backups)
	}
	body, _ = os.ReadFile(backups[0])
	if string(body) != "previous" {
		t.Fatalf("previous target changed: %q", body)
	}
	location := filepath.ToSlash(strings.TrimPrefix(backups[0], root+string(filepath.Separator)))
	download, err := OpenDownload(root, location, false)
	if err != nil {
		t.Fatalf("previous target is inaccessible: %v", err)
	}
	download.Close()
}

func TestIncompleteRollbackKeepsReplacedProtectedInodesPrivate(t *testing.T) {
	for _, name := range []string{"config/paper-global.yml", "server.properties"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			putTransferFile(t, root, name, "original fake credential")
			writeZip(t, filepath.Join(root, "pack.zip"), file(name, "replacement"), file("z.txt", "last"))
			oldRename := renameEntry
			defer func() { renameEntry = oldRename }()
			replaced := false
			renameEntry = func(r *os.Root, from, to string) error {
				if strings.HasSuffix(from, "/new/z.txt") {
					return errors.New("injected later placement failure")
				}
				if from == name {
					putTransferFile(t, root, name+".replacement", "new fake credential")
					if err := os.Rename(filepath.Join(root, name+".replacement"), filepath.Join(root, name)); err != nil {
						t.Fatal(err)
					}
					replaced = true
				}
				err := oldRename(r, from, to)
				if err == nil && strings.HasSuffix(from, "/new/"+name) {
					putTransferFile(t, root, from, "concurrent file")
				}
				return err
			}
			res, err := Execute(root, Request{Op: OpUnzip, Path: "pack.zip", Overwrite: true})
			if err != nil || !replaced || res.Code != CodeRecovery || len(res.RecoveryPaths) != 2 || !strings.Contains(res.Error, "administrator recovery") {
				t.Fatalf("protected inode replacement: %+v %v", res, err)
			}
			public, _ := filepath.Glob(filepath.Join(root, "file-recovery-*/previous", name))
			if len(public) != 0 {
				t.Fatalf("protected backup was exposed: %v", public)
			}
			private, _ := filepath.Glob(filepath.Join(root, ".felis-preserved-*/old", name))
			if len(private) != 1 {
				t.Fatalf("protected backup was lost: %v", private)
			}
			body, _ := os.ReadFile(private[0])
			if string(body) != "new fake credential" {
				t.Fatalf("protected backup changed: %q", body)
			}
			location := filepath.ToSlash(strings.TrimPrefix(private[0], root+string(filepath.Separator)))
			read, err := Execute(root, Request{Op: OpRead, Path: location})
			if err != nil || read.Code != CodeBadPath {
				t.Fatalf("protected backup is readable: %+v %v", read, err)
			}
			if download, err := OpenDownload(root, location, false); err == nil {
				download.Close()
				t.Fatal("protected backup is downloadable")
			}
		})
	}
}

func TestSweepPreservesRollbackData(t *testing.T) {
	for _, name := range []string{"old/item.txt", "new/item.txt"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tmp := unzipTempPrefix + "0123456789ab"
			putTransferFile(t, root, filepath.Join(tmp, name), "retained")
			if strings.HasPrefix(name, "new/") {
				putTransferFile(t, root, filepath.Join(tmp, recoveryMarker), "")
			}
			past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			if err := os.Chtimes(filepath.Join(root, tmp), past, past); err != nil {
				t.Fatal(err)
			}
			r, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			sweepUnzipTemps(r, ".")
			body, err := os.ReadFile(filepath.Join(root, tmp, name))
			if err != nil || string(body) != "retained" {
				t.Fatalf("rollback data was swept: %q %v", body, err)
			}
		})
	}
}
