package fileedit

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func putTransferFile(t *testing.T, root, name, body string) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCopyMoveConflictsAndDirectoryMerge(t *testing.T) {
	for _, op := range []string{OpCopy, OpMove} {
		t.Run(op, func(t *testing.T) {
			root := t.TempDir()
			putTransferFile(t, root, "from/plugin/config.yml", "new")
			putTransferFile(t, root, "target/plugin/config.yml", "old")
			putTransferFile(t, root, "target/plugin/retained.yml", "retained")
			req := Request{Op: op, Paths: []string{"from/plugin"}, To: "target"}
			res, err := Execute(root, req)
			if err != nil || res.Code != CodeExists || res.ConflictCount != 1 || len(res.Conflicts) != 1 || res.Conflicts[0] != "target/plugin/config.yml" {
				t.Fatalf("conflict: %+v %v", res, err)
			}
			body, _ := os.ReadFile(filepath.Join(root, "from/plugin/config.yml"))
			if string(body) != "new" {
				t.Fatal("conflict modified the source")
			}
			req.Overwrite = true
			res, err = Execute(root, req)
			if err != nil || res.Code != "" || res.Files != 1 {
				t.Fatalf("transfer: %+v %v", res, err)
			}
			body, _ = os.ReadFile(filepath.Join(root, "target/plugin/config.yml"))
			if string(body) != "new" {
				t.Fatalf("target=%q", body)
			}
			body, _ = os.ReadFile(filepath.Join(root, "target/plugin/retained.yml"))
			if string(body) != "retained" {
				t.Fatal("merge removed unrelated content")
			}
			_, err = os.Stat(filepath.Join(root, "from/plugin"))
			if (op == OpMove) != os.IsNotExist(err) {
				t.Fatalf("source after %s: %v", op, err)
			}
		})
	}
}

func TestTransfersRejectEscapesLinksAndSelf(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, "source/item.txt", "data")
	putTransferFile(t, root, "target/keep", "keep")
	if err := os.Symlink("source", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{OpCopy, OpMove} {
		for _, req := range []Request{
			{Paths: []string{"."}, To: "target"}, {Paths: []string{"../outside"}, To: "target"},
			{Paths: []string{"source"}, To: "source"}, {Paths: []string{"source"}, To: "source/nested"},
			{Paths: []string{"alias"}, To: "target"}, {Paths: []string{"source/item.txt"}, To: "alias"},
			{Paths: []string{"source", "source/item.txt"}, To: "target"},
		} {
			req.Op = op
			res, err := Execute(root, req)
			if err != nil || res.Code == "" {
				t.Fatalf("unsafe transfer %+v: %+v %v", req, res, err)
			}
		}
	}
}

func TestCopyRedactsProtectedAliasesAndMoveRefusesThem(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, "server.properties", "motd=public\nrcon.password=fake-rcon\nforwarding-secrets=fake-forward\n")
	putTransferFile(t, root, "config/paper-global.yml", "fake-key")
	if err := os.Link(filepath.Join(root, "server.properties"), filepath.Join(root, "props-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "config/paper-global.yml"), filepath.Join(root, "key-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "target"), 0755); err != nil {
		t.Fatal(err)
	}
	res, err := Execute(root, Request{Op: OpCopy, Paths: []string{"props-alias"}, To: "target"})
	if err != nil || res.Code != "" {
		t.Fatalf("copy redacted properties: %+v %v", res, err)
	}
	body, _ := os.ReadFile(filepath.Join(root, "target/props-alias"))
	if !bytes.Contains(body, []byte("motd=public")) || bytes.Contains(body, []byte("fake-rcon")) || bytes.Contains(body, []byte("fake-forward")) {
		t.Fatalf("copy leaked credentials: %q", body)
	}
	for _, op := range []string{OpCopy, OpMove} {
		res, err = Execute(root, Request{Op: op, Paths: []string{"key-alias"}, To: "target"})
		if err != nil || res.Code != CodeBadPath {
			t.Fatalf("protected key: %+v %v", res, err)
		}
	}
	res, err = Execute(root, Request{Op: OpMove, Paths: []string{"props-alias"}, To: "target", Overwrite: true})
	if err != nil || res.Code != CodeBadPath {
		t.Fatalf("move properties alias: %+v %v", res, err)
	}
}

func TestMoveFailurePreservesConcurrentlyRecreatedSource(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, "from/item.txt", "original")
	putTransferFile(t, root, "target/keep", "keep")
	oldRename := renameEntry
	defer func() { renameEntry = oldRename }()
	renameEntry = func(r *os.Root, from, to string) error {
		putTransferFile(t, root, "from/item.txt", "newly created")
		return errors.New("injected placement failure")
	}
	res, err := Execute(root, Request{Op: OpMove, Paths: []string{"from/item.txt"}, To: "target"})
	if err != nil || res.Code == "" || !strings.Contains(res.Error, "move-recovery-") {
		t.Fatalf("move failure: %+v %v", res, err)
	}
	body, _ := os.ReadFile(filepath.Join(root, "from/item.txt"))
	if string(body) != "newly created" {
		t.Fatalf("concurrent source overwritten: %q", body)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "target/move-recovery-*/item.txt"))
	if len(matches) != 1 {
		t.Fatalf("preserved source locations=%v", matches)
	}
	body, _ = os.ReadFile(matches[0])
	if string(body) != "original" {
		t.Fatalf("old source lost: %q", body)
	}
	recovered, err := OpenDownload(root, filepath.ToSlash(strings.TrimPrefix(matches[0], root+string(filepath.Separator))), false)
	if err != nil {
		t.Fatalf("recovered source is inaccessible: %v", err)
	}
	recovered.Close()
}

func TestSelectedZipAndServerPackingGuardSecrets(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, "server.properties", "motd=visible\nrcon.password=fake-rcon\n")
	putTransferFile(t, root, "config/paper-global.yml", "fake-key")
	putTransferFile(t, root, "config/normal.yml", "normal")
	putTransferFile(t, root, "config/.felis-edit-111111111111", "staged-fake-key")
	putTransferFile(t, root, "config/.felis-unzip-222222222222/new/key", "staged-fake-key")
	paths := []string{"config", "server.properties"}
	d, err := OpenSelectedDownload(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = d.WriteTo(context.Background(), &out)
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	assertSafeZip(t, out.Bytes())
	res, err := Execute(root, Request{Op: OpZip, Paths: paths, To: "selected.zip"})
	if err != nil || res.Code != "" || res.Bytes == 0 {
		t.Fatalf("pack zip: %+v %v", res, err)
	}
	body, err := os.ReadFile(filepath.Join(root, "selected.zip"))
	if err != nil {
		t.Fatal(err)
	}
	assertSafeZip(t, body)
	res, err = Execute(root, Request{Op: OpZip, Paths: paths, To: "selected.zip"})
	if err != nil || res.Code != CodeExists || res.ConflictCount != 1 {
		t.Fatalf("zip overwrite confirmation: %+v %v", res, err)
	}
	res, err = Execute(root, Request{Op: OpZip, Paths: []string{"config"}, To: "config/self.zip"})
	if err != nil || res.Code != CodeBadPath {
		t.Fatalf("self packing: %+v %v", res, err)
	}
}

func assertSafeZip(t *testing.T, body []byte) {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		r, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		found[entry.Name] = string(b)
		if strings.Contains(string(b), "fake-") || strings.Contains(entry.Name, ".felis-") {
			t.Fatalf("unsafe archive entry %s: %q", entry.Name, b)
		}
	}
	if found["config/normal.yml"] != "normal" || !strings.Contains(found["server.properties"], "motd=visible") {
		t.Fatalf("missing selected files: %v", found)
	}
	if _, ok := found["config/paper-global.yml"]; ok {
		t.Fatal("shared key was archived")
	}
}

func TestMoveMergePreservesNewEntriesInFrozenSource(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, "from/plugin/config.yml", "config")
	putTransferFile(t, root, "target/plugin/retained.yml", "retained")
	oldRename := renameEntry
	defer func() { renameEntry = oldRename }()
	injected := false
	renameEntry = func(r *os.Root, from, to string) error {
		if !injected && strings.Contains(from, "/new/plugin/") {
			injected = true
			putTransferFile(t, root, filepath.ToSlash(filepath.Join(filepath.Dir(from), "late.txt")), "concurrent data")
		}
		return oldRename(r, from, to)
	}
	res, err := Execute(root, Request{Op: OpMove, Paths: []string{"from/plugin"}, To: "target"})
	if err != nil || res.Code != CodeRecovery || len(res.RecoveryPaths) != 1 || !strings.Contains(res.Error, "move-recovery-") {
		t.Fatalf("move with concurrent writer: %+v %v", res, err)
	}
	body, _ := os.ReadFile(filepath.Join(root, "target/plugin/config.yml"))
	if string(body) != "config" {
		t.Fatalf("moved file=%q", body)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "target/move-recovery-*/plugin/late.txt"))
	if len(matches) != 1 {
		t.Fatalf("new source file was discarded: %v", matches)
	}
	body, _ = os.ReadFile(matches[0])
	if string(body) != "concurrent data" {
		t.Fatalf("new file data=%q", body)
	}
}

func TestIncompleteRollbackPreservesRecreatedNamesAndPreviousFiles(t *testing.T) {
	for _, op := range []string{OpCopy, OpMove, OpUnzip} {
		t.Run(op, func(t *testing.T) {
			root := t.TempDir()
			putTransferFile(t, root, "from/plugin/a.txt", "source a")
			putTransferFile(t, root, "from/plugin/b.txt", "source b")
			putTransferFile(t, root, "target/plugin/a.txt", "previous a")
			req := Request{Op: op, Paths: []string{"from/plugin"}, To: "target", Overwrite: true}
			if op == OpUnzip {
				writeZip(t, filepath.Join(root, "target/pack.zip"), file("plugin/a.txt", "source a"), file("plugin/b.txt", "source b"))
				req.Path = "target/pack.zip"
			}
			oldRename := renameEntry
			defer func() { renameEntry = oldRename }()
			injected := false
			renameEntry = func(r *os.Root, from, to string) error {
				if strings.Contains(from, "/new/plugin/b.txt") {
					return errors.New("injected later placement failure")
				}
				err := oldRename(r, from, to)
				if err == nil && strings.Contains(from, "/new/plugin/a.txt") {
					injected = true
					putTransferFile(t, root, from, "concurrent recreated a")
				}
				return err
			}
			res, err := Execute(root, req)
			if err != nil || res.Code != CodeRecovery || len(res.RecoveryPaths) != 1 || !injected || !strings.Contains(res.Error, "recovery-") {
				t.Fatalf("incomplete rollback: %+v %v", res, err)
			}
			body, _ := os.ReadFile(filepath.Join(root, "target/plugin/a.txt"))
			if string(body) != "source a" {
				t.Fatalf("placed source was overwritten during rollback: %q", body)
			}
			previous, _ := filepath.Glob(filepath.Join(root, "target/*-recovery-*/previous/plugin/a.txt"))
			if len(previous) != 1 {
				t.Fatalf("previous file discarded: %v", previous)
			}
			body, _ = os.ReadFile(previous[0])
			if string(body) != "previous a" {
				t.Fatalf("previous file=%q", body)
			}
			location := filepath.Join(root, "from/plugin/a.txt")
			if op != OpMove {
				matches, _ := filepath.Glob(filepath.Join(root, "target/*-recovery-*/plugin/a.txt"))
				if len(matches) != 1 {
					t.Fatalf("concurrent file discarded: %v", matches)
				}
				location = matches[0]
			}
			body, _ = os.ReadFile(location)
			if string(body) != "concurrent recreated a" {
				t.Fatalf("concurrent file overwritten: %q", body)
			}
		})
	}
}

func TestIncompleteRollbackKeepsProtectedBackupPrivate(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, "config/paper-global.yml", "fake-shared-key")
	writeZip(t, filepath.Join(root, "pack.zip"), file("config/paper-global.yml", "replacement"), file("z.txt", "last"))
	oldRename := renameEntry
	defer func() { renameEntry = oldRename }()
	renameEntry = func(r *os.Root, from, to string) error {
		if strings.Contains(from, "/new/z.txt") {
			return errors.New("injected later failure")
		}
		err := oldRename(r, from, to)
		if err == nil && strings.Contains(from, "/new/config/paper-global.yml") {
			putTransferFile(t, root, from, "concurrent config")
		}
		return err
	}
	res, err := Execute(root, Request{Op: OpUnzip, Path: "pack.zip", Overwrite: true})
	if err != nil || res.Code != CodeRecovery || len(res.RecoveryPaths) != 2 || !strings.Contains(res.Error, "administrator recovery") {
		t.Fatalf("protected recovery: %+v %v", res, err)
	}
	kept, _ := filepath.Glob(filepath.Join(root, ".felis-preserved-*/old/config/paper-global.yml"))
	if len(kept) != 1 {
		t.Fatalf("protected backup lost: %v", kept)
	}
	body, _ := os.ReadFile(kept[0])
	if string(body) != "fake-shared-key" {
		t.Fatalf("protected backup changed: %q", body)
	}
	relative := filepath.ToSlash(strings.TrimPrefix(kept[0], root+string(filepath.Separator)))
	read, err := Execute(root, Request{Op: OpRead, Path: relative})
	if err != nil || read.Code != CodeBadPath {
		t.Fatalf("protected backup is readable: %+v %v", read, err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	sweepUnzipTemps(r, ".")
	r.Close()
	if _, err := os.Stat(kept[0]); err != nil {
		t.Fatalf("recovery backup was swept: %v", err)
	}
}

func TestPlacementPreservesDestinationCreatedAfterConflictCheck(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, "from/item.txt", "source")
	putTransferFile(t, root, "target/keep", "keep")
	oldRename := renameEntry
	defer func() { renameEntry = oldRename }()
	renameEntry = func(r *os.Root, from, to string) error {
		if strings.Contains(from, "/new/item.txt") {
			putTransferFile(t, root, to, "concurrent destination")
		}
		return oldRename(r, from, to)
	}
	res, err := Execute(root, Request{Op: OpCopy, Paths: []string{"from/item.txt"}, To: "target"})
	if err != nil || res.Code == "" {
		t.Fatalf("unexpected copy success: %+v %v", res, err)
	}
	body, _ := os.ReadFile(filepath.Join(root, "target/item.txt"))
	if string(body) != "concurrent destination" {
		t.Fatalf("destination overwritten: %q", body)
	}
	body, _ = os.ReadFile(filepath.Join(root, "from/item.txt"))
	if string(body) != "source" {
		t.Fatalf("source changed: %q", body)
	}
}
