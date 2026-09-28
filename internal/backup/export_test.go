package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testFilter withholds secret.yml and rewrites props.txt, by name.
func testFilter(name string, _ fs.FileInfo) (bool, func([]byte) []byte) {
	switch filepath.Base(name) {
	case "secret.yml":
		return true, nil
	case "props.txt":
		return false, summarize
	}
	return false, nil
}

// summarize is a rewrite that changes the length, so a header left with the
// old size shows: the first bytes upper-cased, then how many there were.
func summarize(b []byte) []byte {
	return fmt.Appendf(nil, "%s (%d bytes)", bytes.ToUpper(b[:min(len(b), 8)]), len(b))
}

// untar reads a gzip+tar stream into name → content ("<dir>" for folders,
// "-> target" for symbolic links).
func untar(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			got[hdr.Name] = "<dir>"
			continue
		case tar.TypeSymlink:
			got[hdr.Name] = "-> " + hdr.Linkname
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(body)) != hdr.Size {
			t.Fatalf("%s: header says %d bytes, body has %d", hdr.Name, hdr.Size, len(body))
		}
		got[hdr.Name] = string(body)
	}
}

func TestWriteTarGzFilter(t *testing.T) {
	src := t.TempDir()
	for name, body := range map[string]string{
		"keep.txt":           "kept",
		"props.txt":          "rcon=x",
		"conf/secret.yml":    "key",
		"conf/big/props.txt": strings.Repeat("a", maxRewrite+1),
		"edge/props.txt":     strings.Repeat("a", maxRewrite),
	} {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("keep.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	skipped, withheld, err := WriteTarGz(context.Background(), &out, src, testFilter)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"keep.txt": "kept", "props.txt": "RCON=X (6 bytes)",
		"conf/": "<dir>", "conf/big/": "<dir>",
		// Exactly maxRewrite bytes still fits.
		"edge/": "<dir>", "edge/props.txt": "AAAAAAAA (1048576 bytes)",
	}
	if got := untar(t, out.Bytes()); !reflect.DeepEqual(got, want) {
		t.Fatalf("archive = %v\nwant %v", got, want)
	}
	if !reflect.DeepEqual(skipped, []string{"link"}) {
		t.Errorf("skipped = %v, want [link]", skipped)
	}
	// An oversized file the filter would rewrite goes out withheld, never as is.
	if !reflect.DeepEqual(withheld, []string{"conf/big/props.txt", "conf/secret.yml"}) {
		t.Errorf("withheld = %v", withheld)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := WriteTarGz(ctx, io.Discard, src, testFilter); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: err = %v, want context.Canceled", err)
	}

	// No filter keeps everything: the backup path.
	out.Reset()
	if _, withheld, err := WriteTarGz(context.Background(), &out, src, nil); err != nil || withheld != nil {
		t.Fatalf("unfiltered: withheld %v, err %v", withheld, err)
	}
	if got := untar(t, out.Bytes()); got["props.txt"] != "rcon=x" || got["conf/secret.yml"] != "key" {
		t.Fatalf("unfiltered archive changed files: %v", got)
	}
}

// storedArchive is a gzip+tar like one Archive writes.
func storedArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(hdr *tar.Header, body string) {
		hdr.Size = int64(len(body))
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add(&tar.Header{Name: "conf/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: "conf/secret.yml", Typeflag: tar.TypeReg, Mode: 0o600}, "key")
	add(&tar.Header{Name: "props.txt", Typeflag: tar.TypeReg, Mode: 0o644}, "rcon=x")
	add(&tar.Header{Name: "world/level.dat", Typeflag: tar.TypeReg, Mode: 0o600}, "level")
	add(&tar.Header{Name: "big/props.txt", Typeflag: tar.TypeReg, Mode: 0o644}, strings.Repeat("a", maxRewrite+1))
	add(&tar.Header{Name: "edge/props.txt", Typeflag: tar.TypeReg, Mode: 0o644}, strings.Repeat("a", maxRewrite))
	// A link holds no bytes, so it passes whatever its name.
	add(&tar.Header{Name: "old/secret.yml", Typeflag: tar.TypeSymlink, Linkname: "../world/level.dat", Mode: 0o777}, "")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// failAtEOF passes r through and turns its EOF into err, as the export Job's
// digest check does on a mismatch.
type failAtEOF struct {
	r    io.Reader
	err  error
	read int
}

func (f *failAtEOF) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	f.read += n
	if err == io.EOF {
		return n, f.err
	}
	return n, err
}

func TestFilterTarGz(t *testing.T) {
	stored := storedArchive(t)

	t.Run("withholds and rewrites by name, keeps the rest", func(t *testing.T) {
		var out bytes.Buffer
		withheld, err := FilterTarGz(context.Background(), &out, bytes.NewReader(stored), testFilter)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"conf/": "<dir>", "props.txt": "RCON=X (6 bytes)", "world/level.dat": "level",
			"edge/props.txt": "AAAAAAAA (1048576 bytes)", "old/secret.yml": "-> ../world/level.dat",
		}
		if got := untar(t, out.Bytes()); !reflect.DeepEqual(got, want) {
			t.Fatalf("archive = %v\nwant %v", got, want)
		}
		if !reflect.DeepEqual(withheld, []string{"conf/secret.yml", "big/props.txt"}) {
			t.Errorf("withheld = %v", withheld)
		}
	})

	t.Run("an error at the end of the input leaves the output unfinished", func(t *testing.T) {
		bad := errors.New("digest mismatch")
		src := &failAtEOF{r: bytes.NewReader(stored), err: bad}
		var out bytes.Buffer
		if _, err := FilterTarGz(context.Background(), &out, src, testFilter); !errors.Is(err, bad) {
			t.Fatalf("err = %v, want the end-of-input error", err)
		}
		if src.read != len(stored) {
			t.Fatalf("read %d of %d input bytes", src.read, len(stored))
		}
		zr, err := gzip.NewReader(bytes.NewReader(out.Bytes()))
		if err == nil {
			_, err = io.ReadAll(zr)
		}
		if err == nil {
			t.Fatal("the output is a complete gzip stream; it must lack its end")
		}
	})

	t.Run("a cancelled copy stops", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := FilterTarGz(ctx, io.Discard, bytes.NewReader(stored), testFilter); err != context.Canceled {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("not an archive", func(t *testing.T) {
		var out bytes.Buffer
		if _, err := FilterTarGz(context.Background(), &out, strings.NewReader("plain text"), testFilter); err == nil {
			t.Fatal("a non-gzip input was accepted")
		}
	})
}
