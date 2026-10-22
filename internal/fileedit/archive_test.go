package fileedit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tarFixture(t *testing.T, name, body string, kind byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	h := &tar.Header{Name: name, Typeflag: kind, Mode: 0644}
	if kind == tar.TypeReg {
		h.Size = int64(len(body))
	}
	if kind == tar.TypeSymlink || kind == tar.TypeLink {
		h.Linkname = "/outside"
	}
	if err := w.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if h.Size > 0 {
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestTarExtractionUsesArchiveGuards(t *testing.T) {
	for _, suffix := range []string{".tar", ".tar.gz", ".tgz"} {
		t.Run(suffix, func(t *testing.T) {
			root := t.TempDir()
			body := tarFixture(t, "folder/config.txt", "hello", tar.TypeReg)
			if suffix != ".tar" {
				var out bytes.Buffer
				compressed := gzip.NewWriter(&out)
				if _, err := compressed.Write(body); err != nil {
					t.Fatal(err)
				}
				if err := compressed.Close(); err != nil {
					t.Fatal(err)
				}
				body = out.Bytes()
			}
			if err := os.WriteFile(filepath.Join(root, "pack"+suffix), body, 0644); err != nil {
				t.Fatal(err)
			}
			result, err := Execute(root, Request{Op: OpUnzip, Path: "pack" + suffix})
			if err != nil || result.Code != "" || result.Files != 1 {
				t.Fatalf("extract: %+v %v", result, err)
			}
			got, err := os.ReadFile(filepath.Join(root, "folder/config.txt"))
			if err != nil || string(got) != "hello" {
				t.Fatalf("extracted bytes: %q %v", got, err)
			}
		})
	}
	for _, test := range []struct {
		name string
		kind byte
		code string
	}{
		{"../escape", tar.TypeReg, CodeArchiveUnsafe}, {"linked", tar.TypeSymlink, CodeArchiveSymlink}, {"hardlinked", tar.TypeLink, CodeArchiveSymlink},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "pack.tar"), tarFixture(t, test.name, "bad", test.kind), 0644); err != nil {
				t.Fatal(err)
			}
			result, err := Execute(root, Request{Op: OpUnzip, Path: "pack.tar"})
			if err != nil || result.Code != test.code {
				t.Fatalf("archive guard: %+v %v", result, err)
			}
		})
	}
}

func TestArchiveSweepPreservesActiveStaging(t *testing.T) {
	root := t.TempDir()
	recent, old := ".felis-unzip-111111111111", ".felis-unzip-222222222222"
	for _, name := range []string{recent, old} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, old), past, past); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sweepUnzipTemps(r, ".")
	if _, err := os.Stat(filepath.Join(root, recent)); err != nil {
		t.Fatal("a running operation's staging was removed", err)
	}
	if _, err := os.Stat(filepath.Join(root, old)); !os.IsNotExist(err) {
		t.Fatal("expired staging was retained", err)
	}
}

func TestTarGzipChecksumFailureLeavesOriginalUnchanged(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	compressed := gzip.NewWriter(&out)
	if _, err := compressed.Write(tarFixture(t, "config.txt", "new", tar.TypeReg)); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	body := out.Bytes()
	body[len(body)-8] ^= 0xff
	if err := os.WriteFile(filepath.Join(root, "pack.tgz"), body, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.txt"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := Execute(root, Request{Op: OpUnzip, Path: "pack.tgz", Overwrite: true})
	if err != nil || res.Code != CodeArchiveInvalid {
		t.Fatalf("corrupt gzip: %+v %v", res, err)
	}
	unchanged, err := os.ReadFile(filepath.Join(root, "config.txt"))
	if err != nil || string(unchanged) != "old" {
		t.Fatalf("original changed: %q %v", unchanged, err)
	}
}

func TestArchiveSweepKeepsUnrestoredBackups(t *testing.T) {
	root := t.TempDir()
	name := unzipTempPrefix + "333333333333"
	putTransferFile(t, root, name+"/old/existing.txt", "unrestored")
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, name), past, past); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	sweepUnzipTemps(r, ".")
	r.Close()
	body, err := os.ReadFile(filepath.Join(root, name, "old/existing.txt"))
	if err != nil || string(body) != "unrestored" {
		t.Fatalf("unrestored backup was swept: %q %v", body, err)
	}
}
