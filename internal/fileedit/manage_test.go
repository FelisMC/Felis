package fileedit

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// mustRead returns a file's content, failing the test if it is not there.
func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// assertAbsent fails if anything, a dangling link included, is at p.
func assertAbsent(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists (%v), want nothing there", p, err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// exec runs one request and fails the test on an infrastructure error.
func exec(t *testing.T, root string, req Request) Result {
	t.Helper()
	res, err := Execute(root, req)
	if err != nil {
		t.Fatalf("Execute(%+v): %v", req, err)
	}
	return res
}

// TestWriteCreateOnly: the panel's "new file" lands only where nothing is.
func TestWriteCreateOnly(t *testing.T) {
	root, _ := worldRoot(t)
	create := func(name string) Result {
		return exec(t, root, Request{Op: OpWrite, Path: name, Content: []byte("new: true\n"), CreateOnly: true})
	}

	t.Run("a free path is created", func(t *testing.T) {
		res := create("config/new.yml")
		if res.Code != "" || res.SHA256 != digest([]byte("new: true\n")) {
			t.Fatalf("result = %+v", res)
		}
		if got := mustRead(t, filepath.Join(root, "config", "new.yml")); got != "new: true\n" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("an existing file is refused and left alone", func(t *testing.T) {
		if res := create("server.properties"); res.Code != CodeExists {
			t.Fatalf("code = %q, want %q", res.Code, CodeExists)
		}
		if got := mustRead(t, filepath.Join(root, "server.properties")); got != "motd=hello\n" {
			t.Fatalf("server.properties became %q", got)
		}
	})

	t.Run("a folder is refused as existing", func(t *testing.T) {
		if res := create("config"); res.Code != CodeExists {
			t.Fatalf("code = %q, want %q", res.Code, CodeExists)
		}
	})

	// A plain write follows a link inside the root and creates what it names; a
	// create must not, or "new file" would land somewhere the caller never named.
	t.Run("a dangling link is refused, never followed", func(t *testing.T) {
		symlink(t, "config/elsewhere.yml", filepath.Join(root, "dangling.yml"))
		if res := create("dangling.yml"); res.Code != CodeExists {
			t.Fatalf("code = %q, want %q", res.Code, CodeExists)
		}
		assertAbsent(t, filepath.Join(root, "config", "elsewhere.yml"))
	})
}

func TestMkdir(t *testing.T) {
	root, outside := worldRoot(t)
	mkdir := func(name string) Result { return exec(t, root, Request{Op: OpMkdir, Path: name}) }

	t.Run("makes the folder and hands it to the game uid", func(t *testing.T) {
		var owned []string
		prev := ownWritten
		ownWritten = func(_ *os.Root, name string) error {
			owned = append(owned, name)
			return os.ErrPermission
		}
		defer func() { ownWritten = prev }()
		if res := mkdir("config/sub/"); res.Code != "" {
			t.Fatalf("result = %+v", res)
		}
		if info, err := os.Lstat(filepath.Join(root, "config", "sub")); err != nil || !info.IsDir() {
			t.Fatalf("config/sub = %v, %v; want a folder", info, err)
		}
		if len(owned) != 1 || owned[0] != "config/sub" {
			t.Fatalf("owned = %v, want [config/sub]", owned)
		}
	})

	t.Run("an existing name is exists", func(t *testing.T) {
		for _, name := range []string{"config", "server.properties"} {
			if res := mkdir(name); res.Code != CodeExists {
				t.Errorf("%s: code = %q, want %q", name, res.Code, CodeExists)
			}
		}
	})

	t.Run("a missing parent is not_found and is not made", func(t *testing.T) {
		res := mkdir("plugins/Essentials")
		if res.Code != CodeNotFound || res.Error != "folder plugins does not exist" {
			t.Fatalf("result = %+v", res)
		}
		assertAbsent(t, filepath.Join(root, "plugins"))
	})

	t.Run("the root itself is bad_path", func(t *testing.T) {
		for _, name := range []string{"", ".", "./", "config/.."} {
			if res := mkdir(name); res.Code != CodeBadPath || res.Error != "a folder needs a name" {
				t.Errorf("%q: result = %+v", name, res)
			}
		}
	})

	t.Run("an escape is bad_path and makes nothing outside", func(t *testing.T) {
		symlink(t, outside, filepath.Join(root, "escape-link"))
		for _, name := range []string{"../outside/made", "escape-link/made", filepath.Join(outside, "made")} {
			if res := mkdir(name); res.Code != CodeBadPath {
				t.Errorf("%s: code = %q, want %q", name, res.Code, CodeBadPath)
			}
		}
		assertAbsent(t, filepath.Join(outside, "made"))
	})
}

func TestRemove(t *testing.T) {
	root, outside := worldRoot(t)
	remove := func(name string) Result { return exec(t, root, Request{Op: OpDelete, Path: name}) }

	t.Run("a file", func(t *testing.T) {
		if res := remove("config/paper.yml"); res.Code != "" {
			t.Fatalf("result = %+v", res)
		}
		assertAbsent(t, filepath.Join(root, "config", "paper.yml"))
	})

	t.Run("a folder with everything in it", func(t *testing.T) {
		deep := filepath.Join(root, "plugins", "Essentials")
		if err := os.MkdirAll(deep, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(deep, "config.yml"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if res := remove("plugins"); res.Code != "" {
			t.Fatalf("result = %+v", res)
		}
		assertAbsent(t, filepath.Join(root, "plugins"))
	})

	// A link is removed itself. With a trailing slash the kernel would resolve
	// it to the folder it names, whose contents would then go instead.
	t.Run("a link, never what it points at", func(t *testing.T) {
		keep := filepath.Join(root, "keep")
		if err := os.MkdirAll(keep, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(keep, "a.txt"), []byte("kept"), 0o644); err != nil {
			t.Fatal(err)
		}
		symlink(t, "keep", filepath.Join(root, "keep-link"))
		symlink(t, outside, filepath.Join(root, "escape-link"))
		for _, name := range []string{"keep-link/", "escape-link"} {
			if res := remove(name); res.Code != "" {
				t.Fatalf("%s: result = %+v", name, res)
			}
			assertAbsent(t, filepath.Join(root, strings.TrimSuffix(name, "/")))
		}
		if got := mustRead(t, filepath.Join(keep, "a.txt")); got != "kept" {
			t.Fatalf("keep/a.txt = %q", got)
		}
		if got := mustRead(t, filepath.Join(outside, "secret.txt")); got != "TOP-SECRET" {
			t.Fatalf("outside/secret.txt = %q", got)
		}
	})

	t.Run("the root itself is bad_path", func(t *testing.T) {
		for _, name := range []string{"", ".", "./", "config/.."} {
			if res := remove(name); res.Code != CodeBadPath {
				t.Errorf("%q: code = %q, want %q", name, res.Code, CodeBadPath)
			}
		}
		mustRead(t, filepath.Join(root, "server.properties"))
	})

	t.Run("an escape is bad_path and deletes nothing outside", func(t *testing.T) {
		symlink(t, outside, filepath.Join(root, "escape-dir"))
		for _, name := range []string{"../outside/secret.txt", "escape-dir/secret.txt", "../outside"} {
			if res := remove(name); res.Code != CodeBadPath {
				t.Errorf("%s: code = %q, want %q", name, res.Code, CodeBadPath)
			}
		}
		if got := mustRead(t, filepath.Join(outside, "secret.txt")); got != "TOP-SECRET" {
			t.Fatalf("outside/secret.txt = %q", got)
		}
	})

	t.Run("a missing path is not_found", func(t *testing.T) {
		if res := remove("absent.txt"); res.Code != CodeNotFound {
			t.Fatalf("code = %q, want %q", res.Code, CodeNotFound)
		}
	})
}

func TestRename(t *testing.T) {
	rename := func(t *testing.T, root, from, to string) Result {
		return exec(t, root, Request{Op: OpRename, Path: from, To: to})
	}

	t.Run("a file moves", func(t *testing.T) {
		root, _ := worldRoot(t)
		if res := rename(t, root, "config/paper.yml", "paper.yml"); res.Code != "" {
			t.Fatalf("result = %+v", res)
		}
		if got := mustRead(t, filepath.Join(root, "paper.yml")); got != "verbose: false\n" {
			t.Fatalf("paper.yml = %q", got)
		}
		assertAbsent(t, filepath.Join(root, "config", "paper.yml"))
	})

	t.Run("a folder moves with its contents", func(t *testing.T) {
		root, _ := worldRoot(t)
		if err := os.MkdirAll(filepath.Join(root, "plugins"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "plugins", "a.jar"), []byte("jar"), 0o644); err != nil {
			t.Fatal(err)
		}
		if res := rename(t, root, "plugins/", "plugins-off"); res.Code != "" {
			t.Fatalf("result = %+v", res)
		}
		if got := mustRead(t, filepath.Join(root, "plugins-off", "a.jar")); got != "jar" {
			t.Fatalf("plugins-off/a.jar = %q", got)
		}
		assertAbsent(t, filepath.Join(root, "plugins"))
	})

	// A trailing slash would make the kernel act on what a link names; the link is
	// what was asked for.
	t.Run("a link moves itself, never what it names", func(t *testing.T) {
		root, _ := worldRoot(t)
		if err := os.MkdirAll(filepath.Join(root, "keep"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "keep", "a.jar"), []byte("jar"), 0o644); err != nil {
			t.Fatal(err)
		}
		symlink(t, "keep", filepath.Join(root, "keep-link"))
		if res := rename(t, root, "keep-link/", "moved-link"); res.Code != "" {
			t.Fatalf("result = %+v", res)
		}
		if info, err := os.Lstat(filepath.Join(root, "moved-link")); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("moved-link = %v, %v; want the link", info, err)
		}
		if got := mustRead(t, filepath.Join(root, "keep", "a.jar")); got != "jar" {
			t.Fatalf("keep/a.jar = %q", got)
		}
		assertAbsent(t, filepath.Join(root, "keep-link"))
	})

	t.Run("an existing destination is exists and both stay", func(t *testing.T) {
		root, _ := worldRoot(t)
		if res := rename(t, root, "config/paper.yml", "server.properties"); res.Code != CodeExists {
			t.Fatalf("code = %q, want %q", res.Code, CodeExists)
		}
		if got := mustRead(t, filepath.Join(root, "server.properties")); got != "motd=hello\n" {
			t.Fatalf("server.properties = %q", got)
		}
		mustRead(t, filepath.Join(root, "config", "paper.yml"))
	})

	t.Run("a missing destination folder is not_found and is not made", func(t *testing.T) {
		root, _ := worldRoot(t)
		res := rename(t, root, "config/paper.yml", "disabled/paper.yml")
		if res.Code != CodeNotFound || res.Error != "folder disabled does not exist" {
			t.Fatalf("result = %+v", res)
		}
		mustRead(t, filepath.Join(root, "config", "paper.yml"))
		assertAbsent(t, filepath.Join(root, "disabled"))
	})

	t.Run("a missing source is not_found", func(t *testing.T) {
		root, _ := worldRoot(t)
		if res := rename(t, root, "absent.txt", "b.txt"); res.Code != CodeNotFound {
			t.Fatalf("code = %q, want %q", res.Code, CodeNotFound)
		}
	})

	t.Run("the root is bad_path either way", func(t *testing.T) {
		root, _ := worldRoot(t)
		for _, tc := range [][2]string{
			{".", "x"}, {"", "x"}, {"./", "x"},
			{"config/paper.yml", "."}, {"config/paper.yml", "./"}, {"config/paper.yml", "config/.."},
		} {
			if res := rename(t, root, tc[0], tc[1]); res.Code != CodeBadPath {
				t.Errorf("%q -> %q: code = %q, want %q", tc[0], tc[1], res.Code, CodeBadPath)
			}
		}
		mustRead(t, filepath.Join(root, "config", "paper.yml"))
	})

	// read withholds these by name, so under another name they would come back
	// whole. Every spelling of them, and every way of reaching them, is refused.
	t.Run("the paths read guards cannot move", func(t *testing.T) {
		root, _ := worldRoot(t)
		if err := os.WriteFile(filepath.Join(root, "config", "paper-global.yml"), []byte("secret: k"), 0o644); err != nil {
			t.Fatal(err)
		}
		symlink(t, "config", filepath.Join(root, "cfg-link"))
		for _, from := range []string{
			"server.properties", "./server.properties",
			"config/paper-global.yml", "config//paper-global.yml",
			"config", "config/", "./config",
			"cfg-link/paper-global.yml",
		} {
			res := rename(t, root, from, "moved")
			if res.Code != CodeBadPath || !strings.Contains(res.Error, "managed by felis") {
				t.Errorf("%s: result = %+v, want the managed refusal", from, res)
			}
		}
		assertAbsent(t, filepath.Join(root, "moved"))
		mustRead(t, filepath.Join(root, "config", "paper-global.yml"))
		mustRead(t, filepath.Join(root, "server.properties"))
	})

	// When the guarded name is itself a link, what it names is guarded too, and so
	// is the link.
	t.Run("the target of a guarded link cannot move", func(t *testing.T) {
		root, _ := worldRoot(t)
		if err := os.Rename(filepath.Join(root, "config"), filepath.Join(root, "real-config")); err != nil {
			t.Fatal(err)
		}
		symlink(t, "real-config", filepath.Join(root, "config"))
		if err := os.Rename(filepath.Join(root, "server.properties"), filepath.Join(root, "real.properties")); err != nil {
			t.Fatal(err)
		}
		symlink(t, "real.properties", filepath.Join(root, "server.properties"))
		// The links themselves too: under another name, a read would follow one to
		// what it guards.
		for _, from := range []string{"real-config", "real.properties", "config", "server.properties"} {
			if res := rename(t, root, from, "moved"); res.Code != CodeBadPath {
				t.Errorf("%s: code = %q, want %q", from, res.Code, CodeBadPath)
			}
		}
		assertAbsent(t, filepath.Join(root, "moved"))
	})

	t.Run("a neighbour of a guarded path still moves", func(t *testing.T) {
		root, _ := worldRoot(t)
		if res := rename(t, root, "config/paper.yml", "config/paper.yml.bak"); res.Code != "" {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("an escape is bad_path either way", func(t *testing.T) {
		root, outside := worldRoot(t)
		symlink(t, outside, filepath.Join(root, "escape-link"))
		for _, tc := range [][2]string{
			{"../outside/secret.txt", "stolen.txt"},
			{"escape-link/secret.txt", "stolen.txt"},
			{"config/paper.yml", "../outside/planted.yml"},
			{"config/paper.yml", "escape-link/planted.yml"},
		} {
			if res := rename(t, root, tc[0], tc[1]); res.Code != CodeBadPath {
				t.Errorf("%s -> %s: code = %q, want %q", tc[0], tc[1], res.Code, CodeBadPath)
			}
		}
		assertAbsent(t, filepath.Join(root, "stolen.txt"))
		assertAbsent(t, filepath.Join(outside, "planted.yml"))
		mustRead(t, filepath.Join(root, "config", "paper.yml"))
		if got := mustRead(t, filepath.Join(outside, "secret.txt")); got != "TOP-SECRET" {
			t.Fatalf("outside/secret.txt = %q", got)
		}
	})
}

// fakeSource is an upload's bytes as the Job would fetch them.
type fakeSource struct {
	body    string
	opened  int
	err     error // returned by Open
	readErr error // returned by the body once it runs out
}

func (s *fakeSource) upload(size int64, sum string) *Upload {
	return &Upload{Size: size, SHA256: sum, Open: func() (io.ReadCloser, error) {
		s.opened++
		if s.err != nil {
			return nil, s.err
		}
		var r io.Reader = strings.NewReader(s.body)
		if s.readErr != nil {
			r = io.MultiReader(r, errReader{s.readErr})
		}
		return io.NopCloser(r), nil
	}}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestUpload(t *testing.T) {
	const jar = "PK\x03\x04 plugin bytes"
	whole := func(s *fakeSource) *Upload { return s.upload(int64(len(s.body)), digest([]byte(s.body))) }
	send := func(t *testing.T, root, name string, u *Upload, overwrite bool) (Result, error) {
		t.Helper()
		return Execute(root, Request{Op: OpUpload, Path: name, Upload: u, Overwrite: overwrite})
	}

	t.Run("lands the bytes as a new file", func(t *testing.T) {
		root, _ := worldRoot(t)
		src := &fakeSource{body: jar}
		res, err := send(t, root, "config/Geyser.jar", whole(src), false)
		if err != nil || res.Code != "" {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if got := mustRead(t, filepath.Join(root, "config", "Geyser.jar")); got != jar {
			t.Fatalf("content = %q", got)
		}
		info, _ := os.Stat(filepath.Join(root, "config", "Geyser.jar"))
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("mode = %v, want 0644", info.Mode().Perm())
		}
		assertNoTemporaries(t, filepath.Join(root, "config"))
	})

	t.Run("an existing path is exists and the bytes are never fetched", func(t *testing.T) {
		root, _ := worldRoot(t)
		symlink(t, "config/elsewhere.jar", filepath.Join(root, "dangling.jar"))
		for _, name := range []string{"server.properties", "dangling.jar"} {
			src := &fakeSource{body: jar}
			res, err := send(t, root, name, whole(src), false)
			if err != nil || res.Code != CodeExists || src.opened != 0 {
				t.Fatalf("%s: result = %+v, %v, opened %d; want exists and no fetch", name, res, err, src.opened)
			}
		}
		if got := mustRead(t, filepath.Join(root, "server.properties")); got != "motd=hello\n" {
			t.Fatalf("server.properties = %q", got)
		}
		assertAbsent(t, filepath.Join(root, "config", "elsewhere.jar"))
	})

	t.Run("overwrite replaces the file and keeps its mode", func(t *testing.T) {
		root, _ := worldRoot(t)
		props := filepath.Join(root, "server.properties")
		if err := os.Chmod(props, 0o600); err != nil {
			t.Fatal(err)
		}
		res, err := send(t, root, "server.properties", whole(&fakeSource{body: jar}), true)
		if err != nil || res.Code != "" {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if got := mustRead(t, props); got != jar {
			t.Fatalf("content = %q", got)
		}
		if info, _ := os.Stat(props); info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600 kept", info.Mode().Perm())
		}
	})

	t.Run("a folder is bad_path and the bytes are never fetched", func(t *testing.T) {
		root, _ := worldRoot(t)
		src := &fakeSource{body: jar}
		res, err := send(t, root, "config", whole(src), true)
		if err != nil || res.Code != CodeBadPath || src.opened != 0 {
			t.Fatalf("result = %+v, %v, opened %d", res, err, src.opened)
		}
	})

	t.Run("over the cap is too_large and never fetched; at the cap is fetched", func(t *testing.T) {
		root, _ := worldRoot(t)
		src := &fakeSource{body: jar}
		res, err := send(t, root, "big.jar", src.upload(MaxUploadBytes+1, ""), false)
		if err != nil || res.Code != CodeTooLarge || src.opened != 0 {
			t.Fatalf("result = %+v, %v, opened %d", res, err, src.opened)
		}
		// At the cap the size passes and the transfer starts; this source then
		// comes up short, which is a broken transfer rather than a refusal.
		if _, err := send(t, root, "big.jar", src.upload(MaxUploadBytes, ""), false); err == nil || src.opened != 1 {
			t.Fatalf("at the cap: err = %v, opened %d; want a fetch", err, src.opened)
		}
	})

	// Anything that says the bytes did not arrive intact is the Job failing, not a
	// caller mistake, and whatever was at the path stays exactly as it was.
	t.Run("a broken transfer is an error and changes nothing", func(t *testing.T) {
		for name, u := range map[string]func(*fakeSource) *Upload{
			"short":         func(s *fakeSource) *Upload { return s.upload(int64(len(jar))+1, digest([]byte(jar))) },
			"long":          func(s *fakeSource) *Upload { return s.upload(int64(len(jar))-1, digest([]byte(jar[:len(jar)-1]))) },
			"wrong sha256":  func(s *fakeSource) *Upload { return s.upload(int64(len(jar)), digest([]byte("other"))) },
			"open fails":    func(s *fakeSource) *Upload { s.err = errors.New("connection refused"); return whole(s) },
			"source breaks": func(s *fakeSource) *Upload { s.readErr = errors.New("connection reset"); return whole(s) },
			// The source's own error must not read as the volume filling up.
			"source ENOSPC": func(s *fakeSource) *Upload { s.readErr = syscall.ENOSPC; return whole(s) },
		} {
			t.Run(name, func(t *testing.T) {
				root, _ := worldRoot(t)
				res, err := send(t, root, "server.properties", u(&fakeSource{body: jar}), true)
				var te *transferError
				if !errors.As(err, &te) || res.Code != "" {
					t.Fatalf("result = %+v, err = %v; want a transfer error", res, err)
				}
				if got := mustRead(t, filepath.Join(root, "server.properties")); got != "motd=hello\n" {
					t.Fatalf("server.properties became %q", got)
				}
				assertNoTemporaries(t, root)
			})
		}
	})

	t.Run("a full volume is no_space and changes nothing", func(t *testing.T) {
		root, _ := worldRoot(t)
		prev := syncWritten
		syncWritten = func(*os.File) error { return syscall.ENOSPC }
		defer func() { syncWritten = prev }()
		res, err := send(t, root, "server.properties", whole(&fakeSource{body: jar}), true)
		if err != nil || res.Code != CodeNoSpace {
			t.Fatalf("result = %+v, %v; want no_space", res, err)
		}
		if got := mustRead(t, filepath.Join(root, "server.properties")); got != "motd=hello\n" {
			t.Fatalf("server.properties became %q", got)
		}
		assertNoTemporaries(t, root)
	})

	t.Run("an escape is bad_path and never fetched", func(t *testing.T) {
		root, outside := worldRoot(t)
		symlink(t, outside, filepath.Join(root, "escape-link"))
		symlink(t, "../../outside/secret.txt", filepath.Join(root, "config", "climb"))
		for _, name := range []string{"../outside/x.jar", "escape-link/x.jar", "config/climb"} {
			src := &fakeSource{body: jar}
			res, err := send(t, root, name, whole(src), true)
			if err != nil || res.Code != CodeBadPath || src.opened != 0 {
				t.Errorf("%s: result = %+v, %v, opened %d", name, res, err, src.opened)
			}
		}
		assertAbsent(t, filepath.Join(outside, "x.jar"))
		if got := mustRead(t, filepath.Join(outside, "secret.txt")); got != "TOP-SECRET" {
			t.Fatalf("outside/secret.txt = %q", got)
		}
	})

	t.Run("no source is an error", func(t *testing.T) {
		root, _ := worldRoot(t)
		if _, err := send(t, root, "x.jar", nil, false); err == nil {
			t.Fatal("an upload without a source must fail")
		}
	})
}

func TestExecuteRefusesAnUnknownOp(t *testing.T) {
	root, _ := worldRoot(t)
	if _, err := Execute(root, Request{Op: "chmod", Path: "server.properties"}); err == nil {
		t.Fatal("an unknown op must fail")
	}
}
