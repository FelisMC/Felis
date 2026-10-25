package main

import (
	"archive/zip"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"felis.lolicon.best/internal/fileedit"
)

func TestCmdFilesSamplesWorldDiskUsage(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "server.properties")
	if err := os.WriteFile(name, []byte("rcon.password=fake-private-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var want int64
	for _, path := range []string{root, name} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatal("allocated blocks unavailable")
		}
		want += stat.Blocks * 512
	}
	var stdout, stderr bytes.Buffer
	if code := cmdFiles([]string{"--op", "disk-usage", "--worlds-root", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("disk usage exit=%d stderr=%q", code, stderr.String())
	}
	res := filesResult(t, stdout.String())
	if res.Code != "" || res.UsedBytes == nil || *res.UsedBytes != want || strings.Contains(stdout.String()+stderr.String(), "fake-private") {
		t.Fatalf("CLI disk usage result=%+v want=%d", res, want)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdFiles([]string{"--op", "disk-usage", "--path", "server.properties", "--worlds-root", root}, &stdout, &stderr); code != 0 || filesResult(t, stdout.String()).Code != fileedit.CodeBadPath || filesResult(t, stdout.String()).UsedBytes != nil {
		t.Fatal("disk usage CLI accepted a path")
	}
}

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

func TestCmdFilesListsInstalledExtensions(t *testing.T) {
	root := uploadRoot(t)
	var body bytes.Buffer
	zw := zip.NewWriter(&body)
	w, err := zw.Create("plugin.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "name: CLIPlugin\nversion: '1.2.3'\nauthor: Test\n"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugins/cli.jar.disabled"), body.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := cmdFiles([]string{"--op", "extensions", "--worlds-root", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, stderr.String())
	}
	res := filesResult(t, stdout.String())
	if res.Code != "" || len(res.Extensions) != 1 || res.Extensions[0].Enabled || res.Extensions[0].MetadataStatus != "ok" || res.Extensions[0].Metadata[0].Name != "CLIPlugin" {
		t.Fatalf("extensions result: %+v", res)
	}
}

func TestCmdFilesPaperGlobalUsesFilteredInlinePatches(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config/paper-global.yml")
	original := "# retained\nproxies:\n  velocity:\n    secret: fake-cli-private\nconsole:\n  enable-brigadier-completions: false # retained switch\nmessages:\n  no-permission: Denied\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := cmdFiles([]string{"--op", "paper-global-read", "--worlds-root", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("filtered read exit=%d stderr=%q", code, stderr.String())
	}
	first := filesResult(t, stdout.String())
	if first.Code != "" || first.PaperGlobal == nil || len(first.PaperGlobal.Values) != 2 || first.PaperGlobal.Values["console.enable-brigadier-completions"] != false || strings.Contains(stdout.String()+stderr.String(), "fake-cli-private") {
		t.Fatal("CLI read was not a filtered configuration view")
	}
	content := []byte(`{"console.enable-brigadier-completions":true}`)
	sum := sha256.Sum256(content)
	t.Setenv(fileedit.ContentPartsEnv, "1")
	t.Setenv(fileedit.ContentEnv+"_0", base64.StdEncoding.EncodeToString(content))
	args := []string{"--op", "paper-global-patch", "--worlds-root", root, "--sha256", hex.EncodeToString(sum[:]), "--expect-sha256", first.PaperGlobal.SHA256}
	stdout.Reset()
	stderr.Reset()
	if code := cmdFiles(args, &stdout, &stderr); code != 0 {
		t.Fatalf("filtered patch exit=%d stderr=%q", code, stderr.String())
	}
	patched := filesResult(t, stdout.String())
	stored, err := os.ReadFile(path)
	storedSum := sha256.Sum256(stored)
	if err != nil || patched.Code != "" || patched.PaperGlobal == nil || patched.PaperGlobal.SHA256 != hex.EncodeToString(storedSum[:]) || patched.PaperGlobal.Values["console.enable-brigadier-completions"] != true || strings.Contains(stdout.String()+stderr.String(), "fake-cli-private") {
		t.Fatal("CLI patch failed or exposed private data")
	}
	for _, retained := range []string{"secret: fake-cli-private", "# retained switch", "no-permission: Denied"} {
		if !bytes.Contains(stored, []byte(retained)) {
			t.Fatalf("CLI patch lost retained content %q", retained)
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdFiles(args, &stdout, &stderr); code != 0 || filesResult(t, stdout.String()).Code != fileedit.CodeConflict {
		t.Fatalf("stale patch exit=%d stderr=%q", code, stderr.String())
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, stored) {
		t.Fatal("stale CLI patch modified the file")
	}
	if err := os.WriteFile(path, []byte("private: *fake-cli-private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdFiles([]string{"--op", "paper-global-read", "--worlds-root", root}, &stdout, &stderr); code != 0 || filesResult(t, stdout.String()).Code != fileedit.CodeBadPath || strings.Contains(stdout.String()+stderr.String(), "fake-cli-private") {
		t.Fatalf("unsafe YAML diagnostic exit=%d", code)
	}
}

func TestCmdFilesReadsOlderLogPages(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "logs/latest.log"), []byte("oldest\r\n"+strings.Repeat("latest\r\n", 200)), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := cmdFiles([]string{"--op", "log-tail", "--path", "other.log", "--worlds-root", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, stderr.String())
	}
	first := filesResult(t, stdout.String())
	if first.Code != "" || first.LogTail == nil || !first.LogTail.HasMore || string(first.LogTail.Content) != strings.Repeat("latest\r\n", 200) {
		t.Fatalf("latest log page: %+v", first)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdFiles([]string{"--op", "log-tail", "--cursor", first.LogTail.NextCursor, "--worlds-root", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, stderr.String())
	}
	older := filesResult(t, stdout.String())
	sum := sha256.Sum256([]byte("oldest\r\n"))
	if older.Code != "" || older.LogTail == nil || older.LogTail.HasMore || older.LogTail.NextCursor != "" || older.LogTail.ByteLimited || string(older.LogTail.Content) != "oldest\r\n" || older.LogTail.ContentSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("older log page: %+v", older)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdFiles([]string{"--op", "log-tail", "--cursor", "invalid?", "--worlds-root", root}, &stdout, &stderr); code != 0 || filesResult(t, stdout.String()).Code != fileedit.CodeBadPath {
		t.Fatalf("invalid log cursor: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

// The fixture controls upload GET and landing DELETE responses and records DELETE counts.
type stagedSource struct {
	*httptest.Server
	reports, strays atomic.Int32
	landedCode      int
	landedTo        string
}

func stagedUpload(t *testing.T, token string, body []byte) *stagedSource {
	t.Helper()
	s := &stagedSource{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.URL.Path != "/u" {
			s.strays.Add(1)
			http.Error(w, "no such upload", http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			w.Write(body)
		case http.MethodDelete:
			s.reports.Add(1)
			if s.landedTo != "" {
				w.Header().Set("Location", s.landedTo)
			}
			w.WriteHeader(cmp.Or(s.landedCode, http.StatusNoContent))
		default:
			s.strays.Add(1)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(s.Close)
	return s
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
		if n, strays := srv.reports.Load(), srv.strays.Load(); n != 1 || strays != 0 || stderr.Len() != 0 {
			t.Fatalf("reported landed %d times, %d stray requests, stderr %q; want once", n, strays, stderr.String())
		}
	})

	t.Run("a file already there is a result, and nothing is reported landed", func(t *testing.T) {
		root := uploadRoot(t)
		if err := os.WriteFile(filepath.Join(root, "plugins", "a.jar"), []byte("old!"), 0o644); err != nil {
			t.Fatal(err)
		}
		srv := stagedUpload(t, "tok", body)
		t.Setenv(fileedit.UploadTokenEnv, "tok")
		var stdout, stderr bytes.Buffer
		if code := cmdFiles(uploadArgs(root, srv.URL+"/u", body), &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr.String())
		}
		if res := filesResult(t, stdout.String()); res.Code != fileedit.CodeExists || srv.reports.Load() != 0 {
			t.Fatalf("result = %+v, reported landed %d times", res, srv.reports.Load())
		}
	})

	// The file is in place whatever felis-api answers, so the Job still succeeds
	// and says why the staged copy may linger. A redirect is not followed, since
	// the request carries the token.
	for name, tc := range map[string]struct {
		code   int
		stderr string
	}{
		"refused":    {http.StatusNotFound, "felis files: tell felis-api the upload landed: DELETE returned 404 Not Found\n"},
		"redirected": {http.StatusFound, "felis files: tell felis-api the upload landed: DELETE returned 302 Found\n"},
	} {
		t.Run("a landed report "+name+" still lands the file", func(t *testing.T) {
			var elsewhere atomic.Int32
			away := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
			defer away.Close()
			root := uploadRoot(t)
			srv := stagedUpload(t, "tok", body)
			srv.landedCode, srv.landedTo = tc.code, away.URL+"/u"
			t.Setenv(fileedit.UploadTokenEnv, "tok")
			var stdout, stderr bytes.Buffer
			if code := cmdFiles(uploadArgs(root, srv.URL+"/u", body), &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr.String())
			}
			if res := filesResult(t, stdout.String()); res.Code != "" || stderr.String() != tc.stderr || elsewhere.Load() != 0 {
				t.Fatalf("result = %+v, stderr %q, redirect followed %d times", res, stderr.String(), elsewhere.Load())
			}
			if got, err := os.ReadFile(filepath.Join(root, "plugins", "a.jar")); err != nil || !bytes.Equal(got, body) {
				t.Fatalf("landed %q, %v", got, err)
			}
		})
	}

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
		if srv.reports.Load() != 0 {
			t.Fatal("a refused fetch was reported landed")
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
	content := []byte("[]\r\n")
	sum := sha256.Sum256(content)
	args := []string{"--op", "write", "--path", "ops.json", "--worlds-root", root, "--sha256", hex.EncodeToString(sum[:])}

	t.Run("reassembles the content parts", func(t *testing.T) {
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
		if code := cmdFiles([]string{"--op", "write", "--path", "new.txt", "--worlds-root", root, "--sha256", hex.EncodeToString(sum[:])}, &stdout, &stderr); code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
		if _, err := os.Lstat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
			t.Fatalf("an incomplete spec wrote a file: %v", err)
		}
	})

	// Without the content's SHA-256 the Job could not tell bytes changed on the
	// way from the bytes felis-api sent.
	t.Run("a write without its SHA-256 exits 2 and writes nothing", func(t *testing.T) {
		t.Setenv(fileedit.ContentPartsEnv, "1")
		t.Setenv(fileedit.ContentEnv+"_0", base64.StdEncoding.EncodeToString(content))
		var stdout, stderr bytes.Buffer
		if code := cmdFiles([]string{"--op", "write", "--path", "new.txt", "--worlds-root", root}, &stdout, &stderr); code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
		if _, err := os.Lstat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
			t.Fatalf("a write without its SHA-256 wrote a file: %v", err)
		}
	})

	t.Run("content that changed on the way is a result and writes nothing", func(t *testing.T) {
		t.Setenv(fileedit.ContentPartsEnv, "1")
		t.Setenv(fileedit.ContentEnv+"_0", base64.StdEncoding.EncodeToString([]byte("[]\n")))
		var stdout, stderr bytes.Buffer
		if code := cmdFiles([]string{"--op", "write", "--path", "new.txt", "--worlds-root", root, "--sha256", hex.EncodeToString(sum[:])}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr.String())
		}
		if res := filesResult(t, stdout.String()); res.Code != fileedit.CodeDigestMismatch {
			t.Fatalf("result = %+v, want %s", res, fileedit.CodeDigestMismatch)
		}
		if _, err := os.Lstat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
			t.Fatalf("changed content wrote a file: %v", err)
		}
	})
}

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

func TestFilesStagedWriteRetainsConditionalWriteChecks(t *testing.T) {
	root := t.TempDir()
	body := bytes.Repeat([]byte("x"), 5<<20)
	source := stagedUpload(t, "fake-write-token", body)
	t.Setenv(fileedit.UploadTokenEnv, "fake-write-token")
	sum := sha256.Sum256(body)
	args := []string{"--op", "write", "--path", "large.txt", "--worlds-root", root, "--source-url", source.URL + "/u", "--size", strconv.Itoa(len(body)), "--sha256", hex.EncodeToString(sum[:]), "--create-only"}
	var stdout, stderr bytes.Buffer
	if code := cmdFiles(args, &stdout, &stderr); code != 0 {
		t.Fatalf("staged write exit=%d stderr=%s", code, stderr.String())
	}
	res := filesResult(t, stdout.String())
	if res.Code != "" {
		t.Fatalf("write result=%+v", res)
	}
	got, err := os.ReadFile(filepath.Join(root, "large.txt"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("written bytes=%d err=%v", len(got), err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdFiles(args, &stdout, &stderr); code != 0 || filesResult(t, stdout.String()).Code != fileedit.CodeExists {
		t.Fatalf("create-only staged write exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}
