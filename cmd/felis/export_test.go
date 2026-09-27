package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestCmdExportWorld(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "world", "region"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"server.properties": "motd=hi\n", "world/region/r.0.0.mca": "chunks"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
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
	got := tarEntries(t, rcv.body)
	want := map[string]string{"server.properties": "motd=hi\n", "world/": "", "world/region/": "", "world/region/r.0.0.mca": "chunks"}
	if len(got) != len(want) {
		t.Fatalf("archive holds %v, want %v", got, want)
	}
	for name, body := range want {
		if b, ok := got[name]; !ok || b != body {
			t.Errorf("%s = %q (present %v), want %q", name, b, ok, body)
		}
	}
	if !strings.Contains(stdout.String(), "server=survival mode=world downloaded") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// A world that cannot be read must never reach felis-api as a complete body:
// the chunked upload is cut off, so felis-api aborts the browser's download.
func TestCmdExportWorldReadErrorAbortsTheUpload(t *testing.T) {
	rcv := receiveExport(t, noContent)
	t.Setenv(worldexport.TokenEnv, "tok")
	var stdout, stderr bytes.Buffer
	code := cmdExport([]string{"--mode", "world", "--target-url", rcv.srv.URL, "--worlds-root", filepath.Join(t.TempDir(), "missing")}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	select {
	case <-rcv.served:
		if rcv.readErr == nil {
			t.Fatalf("felis-api read a complete %d-byte body from an unreadable world", len(rcv.body))
		}
	case <-time.After(2 * time.Second): // the request never reached the handler
	}
}

func TestCmdExportBackup(t *testing.T) {
	root := t.TempDir()
	archive := bytes.Repeat([]byte("felis"), 10_000)
	ref := filepath.Join(root, "survival-1.tar.gz")
	if err := os.WriteFile(ref, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	args := func(url, ref string) []string {
		return []string{"--mode", "backup", "--server", "survival", "--target-url", url, "--ref", ref, "--backup-root", root}
	}

	t.Run("hands the archive over with its length", func(t *testing.T) {
		rcv := receiveExport(t, noContent)
		t.Setenv(worldexport.TokenEnv, "tok")
		var stdout, stderr bytes.Buffer
		if code := cmdExport(args(rcv.srv.URL, ref), &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr.String())
		}
		if rcv.req.ContentLength != int64(len(archive)) || !bytes.Equal(rcv.body, archive) || rcv.req.Header.Get("Authorization") != "Bearer tok" {
			t.Fatalf("got %d bytes (length %d, auth %q), want the %d archive bytes",
				len(rcv.body), rcv.req.ContentLength, rcv.req.Header.Get("Authorization"), len(archive))
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

func TestCmdExportUsage(t *testing.T) {
	for name, tc := range map[string]struct {
		token string
		args  []string
	}{
		"no token":           {"", []string{"--mode", "world", "--target-url", "http://api/x"}},
		"no target":          {"tok", []string{"--mode", "world"}},
		"unknown mode":       {"tok", []string{"--mode", "both", "--target-url", "http://api/x"}},
		"backup without ref": {"tok", []string{"--mode", "backup", "--target-url", "http://api/x"}},
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
	if got := exportConfig(cfg, "felis:1", "felis-backups"); got != want {
		t.Fatalf("exportConfig = %+v, want %+v", got, want)
	}
}
