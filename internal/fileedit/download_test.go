package fileedit

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// downloadWorld is worldRoot plus the guarded files with real secrets in them.
func downloadWorld(t *testing.T) string {
	t.Helper()
	root, _ := worldRoot(t)
	for name, body := range map[string]string{
		"server.properties":       "motd=hi\nrcon.password=hunter2\n",
		"config/paper-global.yml": "secret: aVeryRealForwardingKey\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// download runs a whole download into memory.
func download(t *testing.T, root, name string, dir bool) (*Download, []byte) {
	t.Helper()
	d, err := OpenDownload(root, name, dir)
	if err != nil {
		t.Fatalf("OpenDownload(%s): %v", name, err)
	}
	defer d.Close()
	var out bytes.Buffer
	if err := d.WriteTo(context.Background(), &out); err != nil {
		t.Fatalf("WriteTo(%s): %v", name, err)
	}
	return d, out.Bytes()
}

// unzipped reads a zip into name → content ("<dir> <mode>" for folders).
func unzipped(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			// The Unix mode, S_IFDIR included, which is what unzip tools
			// restore a folder's permissions from.
			got[f.Name] = fmt.Sprintf("<dir %o>", f.ExternalAttrs>>16)
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		got[f.Name] = f.Mode().Perm().String() + " " + string(body)
	}
	return got
}

func TestDownload(t *testing.T) {
	const redacted = "motd=hi\nrcon.password=" + redactedValue + "\n"

	t.Run("a file goes out as its exact bytes", func(t *testing.T) {
		root := downloadWorld(t)
		d, got := download(t, root, "config/paper.yml", false)
		if string(got) != "verbose: false\n" || d.Size != int64(len(got)) || d.ContentType != DownloadFileType {
			t.Fatalf("download = %q, size %d, type %s", got, d.Size, d.ContentType)
		}
	})

	t.Run("server.properties, under any name, goes out redacted", func(t *testing.T) {
		root := downloadWorld(t)
		if err := os.Link(filepath.Join(root, "server.properties"), filepath.Join(root, "copy.txt")); err != nil {
			t.Fatal(err)
		}
		symlink(t, "server.properties", filepath.Join(root, "sym.txt"))
		for _, name := range []string{"server.properties", "./server.properties", "copy.txt", "sym.txt"} {
			d, got := download(t, root, name, false)
			if string(got) != redacted || d.Size != int64(len(redacted)) {
				t.Errorf("%s: download = %q, size %d; want %q", name, got, d.Size, redacted)
			}
		}
	})

	t.Run("the forwarding secret, under any name, is refused", func(t *testing.T) {
		root := downloadWorld(t)
		if err := os.Link(filepath.Join(root, "config/paper-global.yml"), filepath.Join(root, "hard.yml")); err != nil {
			t.Fatal(err)
		}
		symlink(t, "config", filepath.Join(root, "cfg"))
		for _, name := range []string{"config/paper-global.yml", "hard.yml", "cfg/paper-global.yml"} {
			d, err := OpenDownload(root, name, false)
			if err == nil {
				d.Close()
				t.Errorf("%s: opened for download", name)
			} else if !strings.Contains(err.Error(), "forwarding secret") {
				t.Errorf("%s: err = %v", name, err)
			}
		}
	})

	// A guarded name that is itself a link guards what it points at: that file
	// is what the server reads, under whatever name it is reached.
	t.Run("a guarded name that is a link guards its target", func(t *testing.T) {
		root, _ := worldRoot(t)
		for name, body := range map[string]string{
			"config/real.yml": "secret: aVeryRealForwardingKey\n",
			"real.properties": "motd=hi\nrcon.password=hunter2\n",
		} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(filepath.Join(root, "server.properties")); err != nil {
			t.Fatal(err)
		}
		symlink(t, "real.yml", filepath.Join(root, "config/paper-global.yml"))
		symlink(t, "real.properties", filepath.Join(root, "server.properties"))
		if d, err := OpenDownload(root, "config/real.yml", false); err == nil {
			d.Close()
			t.Error("the forwarding secret opened under its link target's name")
		}
		if _, got := download(t, root, "real.properties", false); string(got) != redacted {
			t.Errorf("real.properties = %q, want %q", got, redacted)
		}
	})

	t.Run("what is not there as the listing said is refused", func(t *testing.T) {
		root := downloadWorld(t)
		// Opening a FIFO for reading would wait for a writer that never comes.
		if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name string
			dir  bool
			want string
		}{
			{"config", false, "is a folder now"},
			{"server.properties", true, "is not a folder now"},
			{"missing.txt", false, "no such file"},
			{"pipe", false, "not a regular file"},
			{".", true, "not a file or folder inside"},
			{"", true, "not a file or folder inside"},
			{"../outside", true, "not a file or folder inside"},
			{"/etc", true, "not a file or folder inside"},
		} {
			d, err := OpenDownload(root, c.name, c.dir)
			if err == nil {
				d.Close()
				t.Errorf("%q: opened", c.name)
			} else if !strings.Contains(err.Error(), c.want) {
				t.Errorf("%q: err = %v, want %q", c.name, err, c.want)
			}
		}
	})

	t.Run("a folder goes out as a zip under its own name, guarded", func(t *testing.T) {
		root := downloadWorld(t)
		plugins := filepath.Join(root, "plugins")
		for _, d := range []string{"plugins/Essentials/empty", "plugins/Essentials/data"} {
			if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for name, body := range map[string]string{
			"plugins/a.jar":                             "jar",
			"plugins/Essentials/config.yml":             "x: 1",
			"plugins/Essentials/data/server.properties": "rcon.password=notthereal\n",
		} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for name, mode := range map[string]os.FileMode{"a.jar": 0o755, "Essentials/empty": 0o700} {
			if err := os.Chmod(filepath.Join(plugins, name), mode); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Link(filepath.Join(root, "config/paper-global.yml"), filepath.Join(plugins, "stolen.yml")); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(root, "server.properties"), filepath.Join(plugins, "props.txt")); err != nil {
			t.Fatal(err)
		}
		symlink(t, "../config/paper-global.yml", filepath.Join(plugins, "sym.yml"))

		d, b := download(t, root, "plugins", true)
		if d.Size != -1 || d.ContentType != DownloadZipType {
			t.Fatalf("size %d, type %s", d.Size, d.ContentType)
		}
		want := map[string]string{
			"plugins/":                      "<dir 40755>",
			"plugins/Essentials/":           "<dir 40755>",
			"plugins/Essentials/empty/":     "<dir 40700>",
			"plugins/Essentials/data/":      "<dir 40755>",
			"plugins/a.jar":                 "-rwxr-xr-x jar",
			"plugins/Essentials/config.yml": "-rw-r--r-- x: 1",
			// Only the world root's server.properties is the server's.
			"plugins/Essentials/data/server.properties": "-rw-r--r-- rcon.password=notthereal\n",
			"plugins/props.txt":                         "-rw-r--r-- " + redacted,
		}
		if got := unzipped(t, b); !reflect.DeepEqual(got, want) {
			t.Fatalf("zip = %v\nwant %v", got, want)
		}
		if d.Skipped != 1 || d.Withheld != 1 {
			t.Errorf("skipped %d, withheld %d; want 1 and 1", d.Skipped, d.Withheld)
		}
		if bytes.Contains(b, []byte("aVeryReal")) || bytes.Contains(b, []byte("hunter2")) {
			t.Fatal("a secret is in the zip")
		}

		// A nested folder unpacks as itself, not under its parents.
		_, b = download(t, root, "plugins/Essentials/data", true)
		want = map[string]string{
			"data/":                  "<dir 40755>",
			"data/server.properties": "-rw-r--r-- rcon.password=notthereal\n",
		}
		if got := unzipped(t, b); !reflect.DeepEqual(got, want) {
			t.Fatalf("nested zip = %v\nwant %v", got, want)
		}
	})

	// Redaction reads the file whole; one too big for that is refused, or left
	// out of a folder, never sent as it is.
	t.Run("a server.properties too big to redact is refused", func(t *testing.T) {
		root := downloadWorld(t)
		big := append([]byte("rcon.password=hunter2\n"), bytes.Repeat([]byte("#"), MaxReadBytes)...)
		if err := os.WriteFile(filepath.Join(root, "server.properties"), big, 0o644); err != nil {
			t.Fatal(err)
		}
		d, err := OpenDownload(root, "server.properties", false)
		if err == nil {
			d.Close()
			t.Fatal("an oversized server.properties opened for download")
		}
		if !strings.Contains(err.Error(), "cannot be redacted") {
			t.Fatalf("err = %v", err)
		}

		if err := os.Link(filepath.Join(root, "server.properties"), filepath.Join(root, "config/props.txt")); err != nil {
			t.Fatal(err)
		}
		d, b := download(t, root, "config", true)
		want := map[string]string{"config/": "<dir 40755>", "config/paper.yml": "-rw-r--r-- verbose: false\n"}
		if got := unzipped(t, b); !reflect.DeepEqual(got, want) || d.Withheld != 2 {
			t.Fatalf("zip = %v, withheld %d; want %v and 2", got, d.Withheld, want)
		}
	})

	t.Run("a file that shrinks mid-download fails it", func(t *testing.T) {
		root := downloadWorld(t)
		d, err := OpenDownload(root, "config/paper.yml", false)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err := os.Truncate(filepath.Join(root, "config/paper.yml"), 3); err != nil {
			t.Fatal(err)
		}
		if err := d.WriteTo(context.Background(), io.Discard); err == nil || !strings.Contains(err.Error(), "shrank") {
			t.Fatalf("err = %v, want the shrank error", err)
		}
	})

	t.Run("a cancelled download stops", func(t *testing.T) {
		root := downloadWorld(t)
		// Folders only: no file copy is there to notice the cancel.
		if err := os.MkdirAll(filepath.Join(root, "empty/a/b"), 0o755); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for _, c := range []struct {
			name string
			dir  bool
		}{{"config/paper.yml", false}, {"config", true}, {"empty", true}} {
			d, err := OpenDownload(root, c.name, c.dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.WriteTo(ctx, io.Discard); err != context.Canceled {
				t.Errorf("%s: err = %v, want context.Canceled", c.name, err)
			}
			d.Close()
		}
	})
}
