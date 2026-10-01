package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/worldexport"
)

// exportReceiver stands in for felis-api's internal upload route: it records
// the PUT it gets (or the error reading it ended on) and answers with reply.
type exportReceiver struct {
	srv     *httptest.Server
	hits    atomic.Int32
	req     *http.Request
	body    []byte
	readErr error
	served  chan struct{} // one send per request, once it is answered
}

func receiveExport(t *testing.T, reply func(w http.ResponseWriter)) *exportReceiver {
	t.Helper()
	rcv := &exportReceiver{served: make(chan struct{}, 1)}
	rcv.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rcv.hits.Add(1)
		rcv.req = r
		rcv.body, rcv.readErr = io.ReadAll(r.Body)
		reply(w)
		rcv.served <- struct{}{}
	}))
	t.Cleanup(rcv.srv.Close)
	return rcv
}

func noContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// sentWhole fails unless the upload rcv got ended with the Content-Digest
// trailer of its own bytes, and declared length as its size (-1: none).
func sentWhole(t *testing.T, rcv *exportReceiver, length int64) {
	t.Helper()
	sum := sha256.Sum256(rcv.body)
	want := "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
	wantLength := ""
	if length >= 0 {
		wantLength = strconv.FormatInt(length, 10)
	}
	r := rcv.req
	if rcv.readErr != nil || r.Trailer.Get(worldexport.DigestTrailer) != want || r.Header.Get(worldexport.LengthHeader) != wantLength ||
		r.ContentLength != -1 || strings.Join(r.TransferEncoding, ",") != "chunked" {
		t.Fatalf("upload read %v, trailer %v, %s %q, length %d, encoding %v; want trailer %q and %s %q, chunked",
			rcv.readErr, r.Trailer, worldexport.LengthHeader, r.Header.Get(worldexport.LengthHeader), r.ContentLength, r.TransferEncoding,
			want, worldexport.LengthHeader, wantLength)
	}
}

func tarEntries(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}

// writeTree writes name → body under root, making the folders on the way.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The two secrets a world holds, and what server.properties reads as once
// redacted.
const (
	secretProps   = "motd=hi\nrcon.password=hunter2\n"
	redactedProps = "motd=hi\nrcon.password=<redacted by felis>\n"
	forwardingKey = "secret: aVeryRealForwardingKey\n"
)

// secretWorld is a world holding both secrets, with a hard link to the
// forwarding secret under a name nothing would guard by.
func secretWorld(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"server.properties":       secretProps,
		"config/paper-global.yml": forwardingKey,
		"world/region/r.0.0.mca":  "chunks",
	})
	if err := os.MkdirAll(filepath.Join(root, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "config/paper-global.yml"), filepath.Join(root, "plugins/copy.yml")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCmdExportWorld(t *testing.T) {
	root := secretWorld(t)
	if err := os.Symlink("server.properties", filepath.Join(root, "props-link")); err != nil {
		t.Fatal(err)
	}
	rcv := receiveExport(t, noContent)
	t.Setenv(worldexport.TokenEnv, "tok")
	var stdout, stderr bytes.Buffer
	code := cmdExport([]string{"--mode", "world", "--server", "survival", "--target-url", rcv.srv.URL + "/api/v1/internal/exports/ab",
		"--worlds-root", root}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	r := rcv.req
	if r.Method != http.MethodPut || r.URL.Path != "/api/v1/internal/exports/ab" || r.Header.Get("Authorization") != "Bearer tok" ||
		r.Header.Get("Content-Type") != "application/gzip" || r.ContentLength != -1 || strings.Join(r.TransferEncoding, ",") != "chunked" {
		t.Fatalf("request = %s %s, headers %v, length %d, encoding %v", r.Method, r.URL.Path, r.Header, r.ContentLength, r.TransferEncoding)
	}
	want := map[string]string{
		"server.properties": redactedProps, "config/": "", "plugins/": "",
		"world/": "", "world/region/": "", "world/region/r.0.0.mca": "chunks",
	}
	if got := tarEntries(t, rcv.body); !reflect.DeepEqual(got, want) {
		t.Fatalf("archive holds %v\nwant %v", got, want)
	}
	sentWhole(t, rcv, -1)
	want2 := "felis export: left out 1 entries a tar cannot hold (symbolic links, devices, sockets)\n" +
		"felis export: left out 2 files that hold platform secrets\n" +
		"felis export: server=survival mode=world downloaded\n"
	if stdout.String() != want2 {
		t.Errorf("stdout = %q, want %q", stdout.String(), want2)
	}
}

// A world root that cannot be opened fails before anything reaches felis-api.
func TestCmdExportWorldUnreadable(t *testing.T) {
	rcv := receiveExport(t, noContent)
	t.Setenv(worldexport.TokenEnv, "tok")
	var stdout, stderr bytes.Buffer
	code := cmdExport([]string{"--mode", "world", "--target-url", rcv.srv.URL, "--worlds-root", filepath.Join(t.TempDir(), "missing")}, &stdout, &stderr)
	if code != 1 || rcv.hits.Load() != 0 {
		t.Fatalf("exit %d with %d requests, want 1 and none", code, rcv.hits.Load())
	}
}

// An export that fails part-way must never reach felis-api as a complete body:
// the chunked upload is cut off, so felis-api aborts the browser's download,
// and the failure itself is what the Job reports.
func TestStreamExportWriteErrorAbortsTheUpload(t *testing.T) {
	broken := errors.New("disk read failed")
	rcv := receiveExport(t, noContent)
	err := streamExport(context.Background(), rcv.srv.URL, "tok", "application/gzip", -1, func(w io.Writer) error {
		if _, err := w.Write(bytes.Repeat([]byte("x"), 100_000)); err != nil {
			return err
		}
		return broken
	})
	if err != broken {
		t.Fatalf("err = %v, want the write's own error, unwrapped", err)
	}
	select {
	case <-rcv.served:
		if rcv.readErr == nil {
			t.Fatalf("felis-api read a complete %d-byte body from a failed export", len(rcv.body))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached felis-api")
	}
}

// When felis-api refuses first, its reason is reported, not the closed pipe
// that then stops the writer.
func TestStreamExportRefusalStopsTheWriter(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer refusing.Close()
	stopped := make(chan error, 1)
	err := streamExport(context.Background(), refusing.URL, "tok", "application/gzip", -1, func(w io.Writer) error {
		for {
			if _, err := w.Write(make([]byte, 32<<10)); err != nil {
				stopped <- err
				return err
			}
		}
	})
	if err == nil || err.Error() != "felis-api answered 404 Not Found" {
		t.Fatalf("err = %v, want felis-api's answer", err)
	}
	if werr := <-stopped; !errors.Is(werr, io.ErrClosedPipe) {
		t.Fatalf("the writer stopped on %v, want the closed pipe", werr)
	}

	// A PUT that never starts leaves no transport to close the body: the writer
	// is still stopped, and the export fails rather than hangs.
	done := make(chan error, 1)
	go func() {
		done <- streamExport(context.Background(), "http://[::1", "tok", "application/gzip", -1, func(w io.Writer) error {
			_, err := w.Write([]byte("x"))
			return err
		})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "missing ']'") {
			t.Fatalf("err = %v, want the bad URL", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an export whose PUT never started hung")
	}
}

// storedBackup writes, at path, a gzip+tar like one the backup store holds:
// the world whole, both secrets included, and a region file that does not
// compress. It returns the archive's sha256.
func storedBackup(t *testing.T, path string) string {
	t.Helper()
	region := make([]byte, 64<<10)
	if _, err := rand.Read(region); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range []struct{ name, body string }{
		{"server.properties", secretProps},
		{"config/paper-global.yml", forwardingKey},
		{"world/level.dat", "level"},
		{"world/region/r.0.0.mca", string(region)},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(e.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

func TestCmdExportBackup(t *testing.T) {
	root := t.TempDir()
	ref := filepath.Join(root, "survival-1.tar.gz")
	sum := storedBackup(t, ref)
	stored, err := os.ReadFile(ref)
	if err != nil {
		t.Fatal(err)
	}
	region := tarEntries(t, stored)["world/region/r.0.0.mca"]
	args := func(url, ref string) []string {
		return []string{"--mode", "backup", "--server", "survival", "--target-url", url, "--ref", ref, "--backup-root", root}
	}

	for name, extra := range map[string][]string{
		"no digest recorded":      nil,
		"recorded digest matches": {"--sha256", sum},
	} {
		t.Run(name+": re-streamed through the guards", func(t *testing.T) {
			rcv := receiveExport(t, noContent)
			t.Setenv(worldexport.TokenEnv, "tok")
			var stdout, stderr bytes.Buffer
			if code := cmdExport(append(args(rcv.srv.URL, ref), extra...), &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr.String())
			}
			r := rcv.req
			if r.ContentLength != -1 || r.Header.Get("Content-Type") != "application/gzip" || r.Header.Get("Authorization") != "Bearer tok" {
				t.Fatalf("length %d, headers %v", r.ContentLength, r.Header)
			}
			want := map[string]string{"server.properties": redactedProps, "world/level.dat": "level", "world/region/r.0.0.mca": region}
			if got := tarEntries(t, rcv.body); !reflect.DeepEqual(got, want) {
				t.Fatalf("archive holds %d entries, want exactly the redacted properties, level.dat and the region file", len(got))
			}
			sentWhole(t, rcv, -1)
			if want := "felis export: left out 1 files that hold platform secrets\nfelis export: server=survival mode=backup downloaded\n"; stdout.String() != want {
				t.Errorf("stdout = %q, want %q", stdout.String(), want)
			}
		})
	}

	// The stored bytes are checked as they stream, and the archive the Job sends
	// is only closed once they are all read: a mismatch cuts the upload off
	// short of its end, so felis-api never passes on a complete-looking copy.
	t.Run("a digest mismatch cuts the upload off before its end", func(t *testing.T) {
		rcv := receiveExport(t, noContent)
		t.Setenv(worldexport.TokenEnv, "tok")
		var stdout, stderr bytes.Buffer
		if code := cmdExport(append(args(rcv.srv.URL, ref), "--sha256", strings.Repeat("ab", 32)), &stdout, &stderr); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if want := "felis export: the backup archive does not match the sha256 recorded when it was written\n"; stderr.String() != want {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
		select {
		case <-rcv.served:
		case <-time.After(5 * time.Second):
			t.Fatal("the upload never reached felis-api")
		}
		if rcv.readErr == nil {
			t.Fatalf("felis-api read a complete %d-byte body", len(rcv.body))
		}
		zr, err := gzip.NewReader(bytes.NewReader(rcv.body))
		if err == nil {
			_, err = io.ReadAll(zr)
		}
		if err == nil {
			t.Fatal("what felis-api got is a complete archive")
		}
	})

	t.Run("a ref outside the backup root is refused before any request", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "secret.tar.gz")
		if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		rcv := receiveExport(t, noContent)
		t.Setenv(worldexport.TokenEnv, "tok")
		var stdout, stderr bytes.Buffer
		if code := cmdExport(args(rcv.srv.URL, outside), &stdout, &stderr); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if n := rcv.hits.Load(); n != 0 {
			t.Fatalf("felis-api got %d requests", n)
		}
	})

	t.Run("a refusal exits 1 with felis-api's reason", func(t *testing.T) {
		rcv := receiveExport(t, func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			io.WriteString(w, `{"error":{"code":"export_expired","message":"nobody opened the download"}}`)
		})
		t.Setenv(worldexport.TokenEnv, "tok")
		var stdout, stderr bytes.Buffer
		if code := cmdExport(args(rcv.srv.URL, ref), &stdout, &stderr); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if got := stderr.String(); got != "felis export: felis-api answered 410 Gone: nobody opened the download\n" {
			t.Fatalf("stderr = %q", got)
		}
	})

	// The request carries the token and the internal face never redirects, so a
	// redirect is refused rather than followed with the token attached. A 302 or
	// 303 is the one net/http would follow on its own (as a GET, and to the same
	// host with the Authorization header still on it).
	for _, status := range []int{http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run("a redirect is not followed: "+strconv.Itoa(status), func(t *testing.T) {
			elsewhere := receiveExport(t, noContent)
			redirecting := httptest.NewServer(http.RedirectHandler(elsewhere.srv.URL, status))
			defer redirecting.Close()
			t.Setenv(worldexport.TokenEnv, "tok")
			var stdout, stderr bytes.Buffer
			if code := cmdExport(args(redirecting.URL, ref), &stdout, &stderr); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			if n := elsewhere.hits.Load(); n != 0 {
				t.Fatalf("the redirect target got %d requests", n)
			}
			if got, want := stderr.String(), "felis export: felis-api answered "+strconv.Itoa(status)+" "+http.StatusText(status)+"\n"; got != want {
				t.Fatalf("stderr = %q, want %q", got, want)
			}
		})
	}
}

func TestCmdExportFiles(t *testing.T) {
	root := secretWorld(t)
	writeTree(t, root, map[string]string{"plugins/Essentials/config.yml": "x: 1"})
	if err := os.Symlink("config.yml", filepath.Join(root, "plugins/Essentials/link.yml")); err != nil {
		t.Fatal(err)
	}
	export := func(t *testing.T, path string, dir bool) (*exportReceiver, int, string, string) {
		t.Helper()
		rcv := receiveExport(t, noContent)
		t.Setenv(worldexport.TokenEnv, "tok")
		args := []string{"--mode", "files", "--server", "survival", "--target-url", rcv.srv.URL, "--worlds-root", root, "--path", path}
		if dir {
			args = append(args, "--dir")
		}
		var stdout, stderr bytes.Buffer
		code := cmdExport(args, &stdout, &stderr)
		return rcv, code, stdout.String(), stderr.String()
	}

	for path, want := range map[string]string{
		"world/region/r.0.0.mca": "chunks",
		"server.properties":      redactedProps,
	} {
		t.Run("a file goes with its exact length: "+path, func(t *testing.T) {
			rcv, code, stdout, stderr := export(t, path, false)
			if code != 0 || stdout != "felis export: server=survival mode=files downloaded\n" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			if string(rcv.body) != want || rcv.req.Header.Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("body %q, type %q; want %q", rcv.body, rcv.req.Header.Get("Content-Type"), want)
			}
			sentWhole(t, rcv, int64(len(want)))
		})
	}

	t.Run("a folder goes as a zip, guarded", func(t *testing.T) {
		rcv, code, stdout, stderr := export(t, "plugins", true)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if rcv.req.ContentLength != -1 || rcv.req.Header.Get("Content-Type") != "application/zip" {
			t.Fatalf("length %d, type %q", rcv.req.ContentLength, rcv.req.Header.Get("Content-Type"))
		}
		zr, err := zip.NewReader(bytes.NewReader(rcv.body), int64(len(rcv.body)))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		if want := []string{"plugins/", "plugins/Essentials/", "plugins/Essentials/config.yml"}; !slices.Equal(names, want) {
			t.Fatalf("zip holds %v, want %v", names, want)
		}
		want := "felis export: left out 1 entries a zip does not carry (symbolic links, devices, sockets)\n" +
			"felis export: left out 1 files that hold platform secrets\n" +
			"felis export: server=survival mode=files downloaded\n"
		if stdout != want {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
	})

	for _, c := range []struct {
		path string
		dir  bool
		want string
	}{
		{"config/paper-global.yml", false, "forwarding secret"},
		{"plugins/copy.yml", false, "forwarding secret"},
		{"plugins", false, "is a folder now"},
		{"server.properties", true, "is not a folder now"},
	} {
		t.Run("refused before any request: "+c.path, func(t *testing.T) {
			rcv, code, _, stderr := export(t, c.path, c.dir)
			if code != 1 || rcv.hits.Load() != 0 || !strings.Contains(stderr, c.want) {
				t.Fatalf("exit %d, %d requests, stderr %q; want 1, none, and %q", code, rcv.hits.Load(), stderr, c.want)
			}
		})
	}
}

func TestCmdExportUsage(t *testing.T) {
	for name, tc := range map[string]struct {
		token string
		args  []string
	}{
		"no token":           {"", []string{"--mode", "world", "--target-url", "http://api/x"}},
		"no target":          {"tok", []string{"--mode", "world"}},
		"unknown mode":       {"tok", []string{"--mode", "both", "--target-url", "http://api/x"}},
		"backup without ref": {"tok", []string{"--mode", "backup", "--target-url", "http://api/x"}},
		"files without path": {"tok", []string{"--mode", "files", "--target-url", "http://api/x"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(worldexport.TokenEnv, tc.token)
			var stdout, stderr bytes.Buffer
			if code := cmdExport(tc.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit %d, want 2; stderr %q", code, stderr.String())
			}
		})
	}
}

// TestCmdExportWiring: the Job's `felis export` reaches cmdExport, and
// felis-api's executor mounts the backup store at the path the archives were
// written under, since a backup's ref is an absolute path there.
func TestCmdExportWiring(t *testing.T) {
	t.Setenv(worldexport.TokenEnv, "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"export"}, &stdout, &stderr); code != 2 ||
		stderr.String() != "felis export: --target-url and "+worldexport.TokenEnv+" are required\n" {
		t.Fatalf("felis export = %d, stderr %q", code, stderr.String())
	}
	cfg := &config.Config{}
	cfg.K8s.Namespace, cfg.Archive.LocalPath = "games", "/srv/felis-backups"
	want := worldexport.Config{Namespace: "games", Image: "felis:1", BackupPVC: "felis-backups", BackupRoot: "/srv/felis-backups"}
	if got := exportConfig(cfg, "felis:1", "felis-backups"); !reflect.DeepEqual(got, want) {
		t.Fatalf("exportConfig = %+v, want %+v", got, want)
	}
}
