package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"felis.lolicon.best/internal/fileedit"
)

// filesResult is the Result a `felis files` run printed on its marked line,
// the last it prints.
func filesResult(t *testing.T, stdout string) fileedit.Result {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	line, ok := strings.CutPrefix(lines[len(lines)-1], fileedit.ResultPrefix)
	if !ok {
		t.Fatalf("stdout has no result line: %q", stdout)
	}
	var res fileedit.Result
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		t.Fatalf("result line %q: %v", line, err)
	}
	return res
}

// stagedUpload serves body to a request carrying Bearer token, and 404 to any
// other, the way felis-api's internal face does.
func stagedUpload(t *testing.T, token string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no such upload", http.StatusNotFound)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func uploadArgs(root, sourceURL string, body []byte) []string {
	sum := sha256.Sum256(body)
	return []string{
		"--op", "upload", "--path", "plugins/a.jar", "--worlds-root", root,
		"--source-url", sourceURL, "--size", "4", "--sha256", hex.EncodeToString(sum[:]),
	}
}

// uploadRoot is a world with the plugins folder an upload lands in.
func uploadRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCmdFilesUpload(t *testing.T) {
	body := []byte("PK\x03\x04")

	t.Run("fetches the staged bytes with its token and lands them", func(t *testing.T) {
		root := uploadRoot(t)
		srv := stagedUpload(t, "tok", body)
		t.Setenv(fileedit.UploadTokenEnv, "tok")
		var stdout, stderr bytes.Buffer
		if code := cmdFiles(uploadArgs(root, srv.URL+"/u", body), &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), fileedit.ProgressPrefix+`{"done":4,"total":4}`+"\n") {
			t.Fatalf("stdout %q does not start with the progress to the last byte", stdout.String())
		}
		if res := filesResult(t, stdout.String()); res.Code != "" {
			t.Fatalf("result = %+v", res)
		}
		got, err := os.ReadFile(filepath.Join(root, "plugins", "a.jar"))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("landed %q, %v", got, err)
		}
	})

	// A refused fetch is the Job failing, never a Result: the API answers it with a
	// 500 the caller retries whole.
	t.Run("a refused fetch exits 1 and lands nothing", func(t *testing.T) {
		root := uploadRoot(t)
		srv := stagedUpload(t, "tok", body)
		t.Setenv(fileedit.UploadTokenEnv, "wrong")
		var stdout, stderr bytes.Buffer
		if code := cmdFiles(uploadArgs(root, srv.URL+"/u", body), &stdout, &stderr); code != 1 {
			t.Fatalf("exit %d, want 1; stdout %q", code, stdout.String())
		}
		if !strings.Contains(stderr.String(), "404") {
			t.Fatalf("stderr %q does not name the status", stderr.String())
		}
		if _, err := os.Lstat(filepath.Join(root, "plugins", "a.jar")); !os.IsNotExist(err) {
			t.Fatalf("a refused fetch left a file: %v", err)
		}
	})

	// The request carries the token, and the internal face never redirects, so a
	// redirect is refused rather than followed with the token attached.
	t.Run("a redirect is not followed", func(t *testing.T) {
		root := uploadRoot(t)
		var hits atomic.Int32
		elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Write(body)
		}))
		defer elsewhere.Close()
		redirecting := httptest.NewServer(http.RedirectHandler(elsewhere.URL+"/u", http.StatusFound))
		defer redirecting.Close()
		t.Setenv(fileedit.UploadTokenEnv, "tok")
		var stdout, stderr bytes.Buffer
		if code := cmdFiles(uploadArgs(root, redirecting.URL+"/u", body), &stdout, &stderr); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if n := hits.Load(); n != 0 {
			t.Fatalf("the redirect target was fetched %d times", n)
		}
	})

	for name, tc := range map[string]struct {
		token string
		drop  string
	}{
		"no token":      {"", ""},
		"no source URL": {"tok", "--source-url"},
	} {
		t.Run(name+" exits 2", func(t *testing.T) {
			srv := stagedUpload(t, "tok", body)
			t.Setenv(fileedit.UploadTokenEnv, tc.token)
			args := uploadArgs(uploadRoot(t), srv.URL+"/u", body)
			if tc.drop != "" {
				for i, a := range args {
					if a == tc.drop {
						args = append(args[:i:i], args[i+2:]...)
						break
					}
				}
			}
			var stdout, stderr bytes.Buffer
			if code := cmdFiles(args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
		})
	}
}

func TestCmdFilesWrite(t *testing.T) {
	root := t.TempDir()
	args := []string{"--op", "write", "--path", "ops.json", "--worlds-root", root}

	t.Run("reassembles the content parts", func(t *testing.T) {
		content := []byte("[]\r\n")
		t.Setenv(fileedit.ContentPartsEnv, "1")
		t.Setenv(fileedit.ContentEnv+"_0", base64.StdEncoding.EncodeToString(content))
		var stdout, stderr bytes.Buffer
		if code := cmdFiles(args, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr.String())
		}
		if res := filesResult(t, stdout.String()); res.Code != "" {
			t.Fatalf("result = %+v", res)
		}
		if got, err := os.ReadFile(filepath.Join(root, "ops.json")); err != nil || !bytes.Equal(got, content) {
			t.Fatalf("wrote %q, %v", got, err)
		}
	})

	// Writing what did arrive of an incomplete spec would truncate the file.
	t.Run("an incomplete content spec exits 2 and writes nothing", func(t *testing.T) {
		t.Setenv(fileedit.ContentPartsEnv, "2")
		t.Setenv(fileedit.ContentEnv+"_0", base64.StdEncoding.EncodeToString([]byte("x")))
		var stdout, stderr bytes.Buffer
		if code := cmdFiles([]string{"--op", "write", "--path", "new.txt", "--worlds-root", root}, &stdout, &stderr); code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
		if _, err := os.Lstat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
			t.Fatalf("an incomplete spec wrote a file: %v", err)
		}
	})
}

// A caller-fault outcome is a successful run carrying a code, so felis-api can
// answer the precise 4xx instead of a 500.
func TestCmdFilesCallerFaultIsAResult(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := cmdFiles([]string{"--op", "mkdir", "--path", "../out", "--worlds-root", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if res := filesResult(t, stdout.String()); res.Code != fileedit.CodeBadPath {
		t.Fatalf("result = %+v, want code %s", res, fileedit.CodeBadPath)
	}
	stdout.Reset()
	if code := cmdFiles([]string{"--worlds-root", root}, &stdout, &stderr); code != 2 {
		t.Fatalf("no --op: exit %d, want 2", code)
	}
}

// TestCmdFilesUnzip checks an unzip extracts next to the archive and reports its
// progress before its result, the same way an upload does.
func TestCmdFilesUnzip(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "maps"), 0o755); err != nil {
		t.Fatal(err)
	}
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, body := range map[string]string{"world/level.dat": "level", "world/region/r.0.0.mca": "region!"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "maps", "a.zip"), zb.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdFiles([]string{"--op", "unzip", "--path", "maps/a.zip", "--worlds-root", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if res := filesResult(t, stdout.String()); res.Code != "" || res.Files != 2 || res.Bytes != 12 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.HasPrefix(stdout.String(), fileedit.ProgressPrefix) ||
		!strings.Contains(stdout.String(), fileedit.ProgressPrefix+`{"done":12,"total":12}`+"\n") {
		t.Fatalf("stdout %q does not report the progress to the last byte", stdout.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "maps", "world", "region", "r.0.0.mca"))
	if err != nil || string(got) != "region!" {
		t.Fatalf("extracted %q, %v", got, err)
	}
}
