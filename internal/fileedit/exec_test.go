package fileedit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// worldRoot builds a throwaway world directory with a couple of files and returns
// its path, plus the path of a sibling directory OUTSIDE it holding a secret. The
// sibling stands in for /etc — anything the editor must never reach — so an escape
// that succeeds is observable as the secret's contents coming back, not merely as
// a missing error.
func worldRoot(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "world")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside, filepath.Join(root, "config")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	write := func(p, content string) {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	write(filepath.Join(root, "server.properties"), "motd=hello\n")
	write(filepath.Join(root, "config", "paper.yml"), "verbose: false\n")
	write(filepath.Join(outside, "secret.txt"), "TOP-SECRET")
	return root, outside
}

// run executes one list, read or write the way the Job does.
func run(root, op, path string, content []byte, expect string) (Result, error) {
	return Execute(root, Request{Op: op, Path: path, Content: content, Expect: expect})
}

// TestExecuteContainment is the security test of this package. The world directory
// holds attacker-influenced content (players and plugins create files in it), so
// each vector below is a path a caller could genuinely supply to try to leave the
// world mount. Every one MUST be refused — a refusal is CodeBadPath (or, where the
// kernel resolves it to nothing at all, CodeNotFound), never a successful read.
//
// The assertion is deliberately doubled: the Result must carry a failure Code AND
// the secret's contents must not appear in it. Checking only the code would pass a
// hypothetical future regression that returned a code alongside populated content.
func TestExecuteContainment(t *testing.T) {
	root, outside := worldRoot(t)

	// A symlink INSIDE the world pointing OUTSIDE it — the vector a string-prefix
	// check cannot stop and the reason this package uses os.Root. The link is a
	// perfectly ordinary file to a prefix test ("world/escape-link" is under
	// "world/"), yet opening it lands on the secret.
	if err := os.Symlink(outside, filepath.Join(root, "escape-link")); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	// A symlink pointing at an absolute path outside the root, planted at the exact
	// name a caller would then "read" — the write-through-a-planted-symlink shape.
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "planted.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	vectors := []struct {
		name string
		path string
	}{
		{"parent traversal", "../outside/secret.txt"},
		{"nested parent traversal", "config/../../outside/secret.txt"},
		{"absolute path", filepath.Join(outside, "secret.txt")},
		{"absolute path to etc", "/etc/passwd"},
		{"symlinked directory", "escape-link/secret.txt"},
		{"symlinked file", "planted.txt"},
		{"traversal past the filesystem root", "../../../../../../etc/passwd"},
	}

	for _, v := range vectors {
		t.Run("read "+v.name, func(t *testing.T) {
			res, err := run(root, OpRead, v.path, nil, "")
			if err != nil {
				t.Fatalf("Execute returned an infrastructure error, want a contained refusal: %v", err)
			}
			if res.Code == "" {
				t.Fatalf("path %q was ALLOWED (content=%q) — containment breached", v.path, res.Content)
			}
			if bytes.Contains(res.Content, []byte("TOP-SECRET")) {
				t.Fatalf("path %q leaked out-of-root content despite code %q", v.path, res.Code)
			}
		})
	}

	// The write side must be contained by the same invariant: a planted symlink
	// must not become a write into the file it points at.
	t.Run("write through a planted symlink is refused", func(t *testing.T) {
		res, err := run(root, OpWrite, "planted.txt", []byte("pwned"), "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.Code == "" {
			t.Fatal("write through a symlink leaving the root was ALLOWED")
		}
		b, err := os.ReadFile(filepath.Join(outside, "secret.txt"))
		if err != nil {
			t.Fatalf("read secret: %v", err)
		}
		if string(b) != "TOP-SECRET" {
			t.Fatalf("out-of-root file was MODIFIED through the symlink: %q", b)
		}
	})

	t.Run("write escaping by traversal is refused", func(t *testing.T) {
		res, err := run(root, OpWrite, "../outside/new.txt", []byte("pwned"), "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.Code == "" {
			t.Fatal("write via ../ was ALLOWED")
		}
		if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
			t.Fatal("a file was created outside the world root")
		}
	})

	t.Run("list escaping by traversal is refused", func(t *testing.T) {
		res, err := run(root, OpList, "../outside", nil, "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.Code == "" {
			t.Fatalf("listing outside the root was ALLOWED: %+v", res.Entries)
		}
	})
}

// TestExecuteHappyPath proves the three ops actually work inside the root, so the
// containment test above cannot be trivially satisfied by a function that refuses
// everything.
func TestExecuteHappyPath(t *testing.T) {
	root, _ := worldRoot(t)

	t.Run("list the world root", func(t *testing.T) {
		res, err := run(root, OpList, "", nil, "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.Code != "" {
			t.Fatalf("unexpected failure %s: %s", res.Code, res.Error)
		}
		got := map[string]Entry{}
		for _, e := range res.Entries {
			got[e.Name] = e
		}
		if _, ok := got["server.properties"]; !ok {
			t.Fatalf("server.properties missing from listing: %+v", res.Entries)
		}
		if e, ok := got["config"]; !ok || !e.IsDir {
			t.Fatalf("config should be listed as a directory: %+v", got["config"])
		}
		if e := got["server.properties"]; e.Size != int64(len("motd=hello\n")) {
			t.Fatalf("size = %d, want %d", e.Size, len("motd=hello\n"))
		}
	})

	t.Run("list a subdirectory", func(t *testing.T) {
		res, err := run(root, OpList, "config", nil, "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.Code != "" || len(res.Entries) != 1 || res.Entries[0].Name != "paper.yml" {
			t.Fatalf("unexpected listing: %+v (code %q)", res.Entries, res.Code)
		}
	})

	t.Run("a listing reports the room left on the volume", func(t *testing.T) {
		prev := statfs
		t.Cleanup(func() { statfs = prev })
		var asked string
		statfs = func(dir string) (uint64, uint64, error) { asked = dir; return 12345, 99999, nil }
		res, err := run(root, OpList, "config", nil, "")
		if err != nil || res.Code != "" {
			t.Fatalf("Execute: %v %+v", err, res)
		}
		if res.Avail != 12345 || asked != root {
			t.Fatalf("avail = %d measured at %q, want 12345 at %q", res.Avail, asked, root)
		}

		statfs = func(string) (uint64, uint64, error) { return math.MaxUint64, math.MaxUint64, nil }
		if res, _ := run(root, OpList, "config", nil, ""); res.Avail != math.MaxInt64 {
			t.Fatalf("avail = %d, want it clamped to %d", res.Avail, int64(math.MaxInt64))
		}

		statfs = func(string) (uint64, uint64, error) { return 0, 99999, nil }
		if res, _ := run(root, OpList, "config", nil, ""); res.Avail != 0 {
			t.Fatalf("avail = %d on a full volume, want 0", res.Avail)
		}

		statfs = func(string) (uint64, uint64, error) { return 1, 1, errors.New("no statfs") }
		res, err = run(root, OpList, "config", nil, "")
		if err != nil || res.Code != "" || len(res.Entries) != 1 || res.Avail != -1 {
			t.Fatalf("a volume that cannot be measured still lists, with its room -1 (unknown): %v %+v", err, res)
		}
	})

	t.Run("read a file", func(t *testing.T) {
		res, err := run(root, OpRead, "server.properties", nil, "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.Code != "" || string(res.Content) != "motd=hello\n" {
			t.Fatalf("content = %q, code = %q", res.Content, res.Code)
		}
	})

	t.Run("write replaces content, then reads back", func(t *testing.T) {
		if res, err := run(root, OpWrite, "server.properties", []byte("motd=changed\n"), ""); err != nil || res.Code != "" {
			t.Fatalf("write failed: %v / %+v", err, res)
		}
		b, err := os.ReadFile(filepath.Join(root, "server.properties"))
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(b) != "motd=changed\n" {
			t.Fatalf("on-disk content = %q, want the written bytes", b)
		}
	})

	t.Run("write creates a new file but not parent directories", func(t *testing.T) {
		var owned []string
		prev := ownWritten
		ownWritten = func(_ *os.Root, name string) error {
			owned = append(owned, name)
			return os.ErrPermission // a test runner cannot chown; the write must still succeed
		}
		defer func() { ownWritten = prev }()
		if res, err := run(root, OpWrite, "ops.json", []byte("[]"), ""); err != nil || res.Code != "" {
			t.Fatalf("creating a new file should succeed: %v / %+v", err, res)
		}
		if len(owned) != 1 || !strings.HasPrefix(owned[0], ".felis-edit-") {
			t.Errorf("files handed to the game uid = %v, want the one temporary sibling of ops.json", owned)
		}
		res, err := run(root, OpWrite, "nope/deep.txt", []byte("x"), "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.Code == "" {
			t.Fatal("writing into a non-existent directory should fail, not mkdir it")
		}
	})

	t.Run("missing file reads as not_found", func(t *testing.T) {
		res, err := run(root, OpRead, "absent.txt", nil, "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.Code != CodeNotFound {
			t.Fatalf("code = %q, want %q", res.Code, CodeNotFound)
		}
	})

	t.Run("reading a directory is bad_path, not a garbled read", func(t *testing.T) {
		res, err := run(root, OpRead, "config", nil, "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.Code != CodeBadPath {
			t.Fatalf("code = %q, want %q", res.Code, CodeBadPath)
		}
	})

	t.Run("oversized write is refused", func(t *testing.T) {
		res, err := run(root, OpWrite, "big.txt", make([]byte, MaxWriteBytes+1), "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.Code != CodeTooLarge {
			t.Fatalf("code = %q, want %q", res.Code, CodeTooLarge)
		}
	})

	t.Run("oversized read is refused before buffering", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "huge.bin"), make([]byte, MaxReadBytes+1), 0o644); err != nil {
			t.Fatalf("write huge: %v", err)
		}
		res, err := run(root, OpRead, "huge.bin", nil, "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res.Code != CodeTooLarge {
			t.Fatalf("code = %q, want %q", res.Code, CodeTooLarge)
		}
	})
}

// TestReadIsBinarySafe proves the []byte/base64 round-trip preserves bytes that a
// string round-trip would destroy. A config file with a stray non-UTF-8 byte must
// come back byte-identical, or "read, edit one line, write" would silently corrupt
// the rest of the file.
func TestReadIsBinarySafe(t *testing.T) {
	root, _ := worldRoot(t)
	raw := []byte{0xff, 0xfe, 'o', 'k', 0x00, 0x80}
	if err := os.WriteFile(filepath.Join(root, "raw.bin"), raw, 0o644); err != nil {
		t.Fatalf("write raw: %v", err)
	}

	res, err := run(root, OpRead, "raw.bin", nil, "")
	if err != nil || res.Code != "" {
		t.Fatalf("read failed: %v / %+v", err, res)
	}

	// Round-trip through the wire encoding, which is how felis-api actually receives it.
	var buf bytes.Buffer
	if err := Print(&buf, res); err != nil {
		t.Fatalf("Print: %v", err)
	}
	line := strings.TrimPrefix(strings.TrimSpace(buf.String()), ResultPrefix)
	var back Result
	if err := json.Unmarshal([]byte(line), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !bytes.Equal(back.Content, raw) {
		t.Fatalf("content = % x, want % x", back.Content, raw)
	}
}

// TestPrintIsOneMarkedLine pins the transport contract: felis-api finds the payload
// by scanning merged stdout+stderr for ResultPrefix, so the payload must be exactly
// one line and must carry the marker. An indented encoder would break the reader.
func TestPrintIsOneMarkedLine(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(&buf, Result{Entries: []Entry{{Name: "a"}, {Name: "b"}}}); err != nil {
		t.Fatalf("Print: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, ResultPrefix) {
		t.Fatalf("output lacks the marker: %q", out)
	}
	if n := strings.Count(strings.TrimSuffix(out, "\n"), "\n"); n != 0 {
		t.Fatalf("payload spans %d extra lines; it must be exactly one", n)
	}
}

// TestReadRefusesTheForwardingSecret pins the one path denial in this package. The
// value in config/paper-global.yml is the SAME on every backend in the cluster, so
// a read here is not a caller reading their own data — it is the Velocity handshake
// key for everyone else's servers.
//
// The equivalent-spelling cases are the substance of this test. A bare string
// compare against the constant would pass the first case and wave through all the
// rest, which is exactly the bug this guards; each alternative below names the same
// file to the kernel, so each must be refused identically.
func TestReadRefusesTheForwardingSecret(t *testing.T) {
	root, _ := worldRoot(t)
	const secret = "secret: aVeryRealForwardingKey"
	if err := os.WriteFile(filepath.Join(root, "config", "paper-global.yml"),
		[]byte(secret), 0o644); err != nil {
		t.Fatalf("seed paper-global.yml: %v", err)
	}

	for _, spelling := range []string{
		"config/paper-global.yml",
		"./config/paper-global.yml",
		"config//paper-global.yml",
		"config/../config/paper-global.yml",
		"config/./paper-global.yml",
	} {
		res, err := run(root, OpRead, spelling, nil, "")
		if err != nil {
			t.Fatalf("%s: Execute: %v", spelling, err)
		}
		if res.Code != CodeBadPath {
			t.Errorf("%s: code = %q, want %q — an equivalent spelling must not bypass the denial",
				spelling, res.Code, CodeBadPath)
		}
		if strings.Contains(string(res.Content), "aVeryRealForwardingKey") {
			t.Errorf("%s: the forwarding secret leaked into the result", spelling)
		}
	}

	// The denial is READ-only and exact: a neighbouring file in the same directory
	// stays readable, or the guard would have broken ordinary config repair.
	res, err := run(root, OpRead, "config/paper.yml", nil, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Code != "" {
		t.Errorf("config/paper.yml: code = %q, want success — the denial must not widen", res.Code)
	}

	// Writing it is still allowed: it leaks nothing, and the lobby entrypoint
	// rewrites the file whole on every boot regardless.
	res, err = run(root, OpWrite, "config/paper-global.yml", []byte("proxies: {}\n"), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Code != "" {
		t.Errorf("write code = %q, want success — only the read is denied", res.Code)
	}
}

// TestReadRedactsRconPassword pins spec §286 (RCON 密码绝不下发前端) on the one
// path that could leak it: server.properties is the file owners edit most, so it
// is readable — but the RCON password in it is the control plane's command
// credential for that server, and the editor must not hand it back.
func TestReadRedactsRconPassword(t *testing.T) {
	root := t.TempDir()
	props := "motd=hello\nrcon.password=hunter2\nrcon.port=25575\nenable-rcon=true\n"
	if err := os.WriteFile(filepath.Join(root, "server.properties"), []byte(props), 0o644); err != nil {
		t.Fatalf("write server.properties: %v", err)
	}
	// A same-named file in a subdirectory must NOT be treated as the real one: the
	// redaction keys off the cleaned path, and a plugin is free to keep its own
	// server.properties anywhere in the volume.
	if err := os.MkdirAll(filepath.Join(root, "plugins"), 0o755); err != nil {
		t.Fatalf("mkdir plugins: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugins", "server.properties"), []byte("rcon.password=notmine\n"), 0o644); err != nil {
		t.Fatalf("write nested server.properties: %v", err)
	}

	res, err := run(root, OpRead, "server.properties", nil, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The hash is the file on disk, the one a save's expect_sha256 is checked
	// against, not the redacted copy the editor shows.
	if sum := sha256.Sum256([]byte(props)); res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 = %s, want the hash of the file as stored", res.SHA256)
	}
	// The content digest is of the copy handed out, so the bytes that arrive
	// can be checked against it.
	if sum := sha256.Sum256(res.Content); res.ContentSHA256 != hex.EncodeToString(sum[:]) || res.ContentSHA256 == res.SHA256 {
		t.Fatalf("content_sha256 = %s, want the hash of the redacted copy (%x), apart from sha256 %s", res.ContentSHA256, sum, res.SHA256)
	}
	got := string(res.Content)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("read returned the RCON password (spec §286):\n%s", got)
	}
	if !strings.Contains(got, redactedValue) {
		t.Fatalf("read did not mark the password as withheld:\n%s", got)
	}
	// Redaction must not cost the owner the rest of the file — that is the whole
	// reason this is a value redaction and not a whole-file denial.
	for _, keep := range []string{"motd=hello", "rcon.port=25575", "enable-rcon=true"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("redaction dropped %q from the file:\n%s", keep, got)
		}
	}

	nested, err := run(root, OpRead, "plugins/server.properties", nil, "")
	if err != nil {
		t.Fatalf("Execute nested: %v", err)
	}
	if !strings.Contains(string(nested.Content), "notmine") {
		t.Fatalf("a nested server.properties was redacted; only the world root's is the real one:\n%s", nested.Content)
	}
}

// TestReadGuardsLinksToGuardedFiles: a plugin runs as the game uid and can leave
// a link to either guarded file anywhere in the world. Read under the link's
// name, the forwarding secret is still refused and the RCON password still
// redacted, whether the link is symbolic, a hard link, or a linked folder.
func TestReadGuardsLinksToGuardedFiles(t *testing.T) {
	root, _ := worldRoot(t)
	const secret = "secret: aVeryRealForwardingKey"
	if err := os.WriteFile(filepath.Join(root, "config", "paper-global.yml"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server.properties"), []byte("motd=hi\nrcon.password=hunter2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink(t, "config/paper-global.yml", filepath.Join(root, "sym.yml"))
	symlink(t, "config", filepath.Join(root, "cfg-link"))
	symlink(t, "server.properties", filepath.Join(root, "sym.properties"))
	for _, l := range [][2]string{
		{"config/paper-global.yml", "hard.yml"},
		{"server.properties", "hard.properties"},
	} {
		if err := os.Link(filepath.Join(root, l[0]), filepath.Join(root, l[1])); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range []string{"sym.yml", "cfg-link/paper-global.yml", "hard.yml"} {
		res, err := run(root, OpRead, name, nil, "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Code != CodeBadPath || strings.Contains(string(res.Content), "aVeryReal") {
			t.Errorf("%s: result = %+v, want bad_path and no secret", name, res)
		}
	}
	for _, name := range []string{"sym.properties", "hard.properties"} {
		res, err := run(root, OpRead, name, nil, "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := "motd=hi\nrcon.password=" + redactedValue + "\n"; res.Code != "" || string(res.Content) != want {
			t.Errorf("%s: result = %+v, want content %q", name, res, want)
		}
	}
}

// TestWriteIsAtomic is the durability contract: a write that fails part-way leaves
// the original file byte-for-byte intact and no stray sibling behind, and a write
// that succeeds keeps the file's mode.
func TestWriteIsAtomic(t *testing.T) {
	root, _ := worldRoot(t)
	props := filepath.Join(root, "server.properties")

	t.Run("a failed flush leaves the old file and no temporary", func(t *testing.T) {
		prev := syncWritten
		syncWritten = func(*os.File) error { return syscall.ENOSPC }
		defer func() { syncWritten = prev }()
		res, err := run(root, OpWrite, "server.properties", []byte("motd=half"), "")
		if err != nil || res.Code != CodeNoSpace {
			t.Fatalf("Execute = %+v, %v; want no_space", res, err)
		}
		if b, _ := os.ReadFile(props); string(b) != "motd=hello\n" {
			t.Fatalf("original became %q after a failed write", b)
		}
		assertNoTemporaries(t, root)
	})

	t.Run("the file keeps its mode", func(t *testing.T) {
		if err := os.Chmod(props, 0o600); err != nil {
			t.Fatal(err)
		}
		if res, err := run(root, OpWrite, "server.properties", []byte("motd=x\n"), ""); err != nil || res.Code != "" {
			t.Fatalf("write: %v / %+v", err, res)
		}
		info, err := os.Stat(props)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600 kept", info.Mode().Perm())
		}
		assertNoTemporaries(t, root)
	})

	t.Run("a name as long as a folder allows is written", func(t *testing.T) {
		long := strings.Repeat("n", NameMax-4) + ".yml"
		res, err := Execute(root, Request{Op: OpWrite, Path: "config/" + long, Content: []byte("a: 1\n"), CreateOnly: true})
		if err != nil || res.Code != "" {
			t.Fatalf("write a %d-byte name: %v / %+v", len(long), err, res)
		}
		if b, _ := os.ReadFile(filepath.Join(root, "config", long)); string(b) != "a: 1\n" {
			t.Fatalf("content = %q", b)
		}
		assertNoTemporaries(t, filepath.Join(root, "config"))
	})

	t.Run("a link inside the root is written through, not replaced", func(t *testing.T) {
		if err := os.Symlink("config/paper.yml", filepath.Join(root, "paper-link.yml")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if res, err := run(root, OpWrite, "paper-link.yml", []byte("verbose: true\n"), ""); err != nil || res.Code != "" {
			t.Fatalf("write: %v / %+v", err, res)
		}
		if b, _ := os.ReadFile(filepath.Join(root, "config", "paper.yml")); string(b) != "verbose: true\n" {
			t.Fatalf("link target = %q, want the new content", b)
		}
		if info, err := os.Lstat(filepath.Join(root, "paper-link.yml")); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the link itself was replaced: %v %v", info, err)
		}
	})

	t.Run("a relative link climbing out of the root is refused", func(t *testing.T) {
		if err := os.Symlink("../../outside/secret.txt", filepath.Join(root, "config", "climb")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		res, err := run(root, OpWrite, "config/climb", []byte("pwned"), "")
		if err != nil || res.Code != CodeBadPath {
			t.Fatalf("Execute = %+v, %v; want bad_path", res, err)
		}
	})
}

// TestWriteDetectsConcurrentChange: a save carrying the hash its read returned
// lands only while the file is still what was read.
func TestWriteDetectsConcurrentChange(t *testing.T) {
	root, _ := worldRoot(t)
	props := filepath.Join(root, "server.properties")
	if err := os.WriteFile(props, []byte("motd=hello\nrcon.password=hunter2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	read, err := run(root, OpRead, "server.properties", nil, "")
	if err != nil || read.Code != "" || len(read.SHA256) != 64 {
		t.Fatalf("read = %+v, %v; want content and a sha256", read, err)
	}
	// The hash is of the file on disk, not the redacted copy handed out, or a save
	// of an unchanged server.properties would always conflict.
	if bytes.Contains(read.Content, []byte("hunter2")) {
		t.Fatal("rcon password was not redacted")
	}

	// Someone else saves in between.
	if err := os.WriteFile(props, []byte("motd=theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := run(root, OpWrite, "server.properties", []byte("motd=mine\n"), read.SHA256)
	if err != nil || res.Code != CodeConflict {
		t.Fatalf("stale write = %+v, %v; want a conflict", res, err)
	}
	if b, _ := os.ReadFile(props); string(b) != "motd=theirs\n" {
		t.Fatalf("stale write overwrote the other edit: %q", b)
	}
	if res.SHA256 != digest([]byte("motd=theirs\n")) {
		t.Fatalf("conflict sha256 = %q, want the file's current hash", res.SHA256)
	}

	// With the current hash the save lands and reports the new one.
	res, err = run(root, OpWrite, "server.properties", []byte("motd=mine\n"), res.SHA256)
	if err != nil || res.Code != "" || res.SHA256 != digest([]byte("motd=mine\n")) {
		t.Fatalf("fresh write = %+v, %v", res, err)
	}

	// A file deleted since it was read is a conflict too, never a silent re-create.
	if err := os.Remove(props); err != nil {
		t.Fatal(err)
	}
	res, err = run(root, OpWrite, "server.properties", []byte("motd=mine\n"), res.SHA256)
	if err != nil || res.Code != CodeConflict {
		t.Fatalf("write over a deleted file = %+v, %v; want a conflict", res, err)
	}
	if _, err := os.Stat(props); err == nil {
		t.Fatal("a conditional write re-created a deleted file")
	}
}

// TestWriteChecksTheContentDigest: a write lands only bytes that hash to the
// SHA-256 felis-api sent with them; bytes changed on the way touch nothing.
func TestWriteChecksTheContentDigest(t *testing.T) {
	root, _ := worldRoot(t)
	props := filepath.Join(root, "server.properties")
	if err := os.WriteFile(props, []byte("motd=hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sent := sha256.Sum256([]byte("motd=mine\n"))
	req := Request{Op: OpWrite, Path: "server.properties", Content: []byte("motd=mint\n"), ContentSHA256: hex.EncodeToString(sent[:])}
	res, err := Execute(root, req)
	if err != nil || res.Code != CodeDigestMismatch {
		t.Fatalf("changed write = %+v, %v; want %s", res, err, CodeDigestMismatch)
	}
	if b, _ := os.ReadFile(props); string(b) != "motd=hello\n" {
		t.Fatalf("a changed write replaced the file with %q", b)
	}
	assertNoTemporaries(t, root)

	req.Content = []byte("motd=mine\n")
	if res, err := Execute(root, req); err != nil || res.Code != "" {
		t.Fatalf("write as sent = %+v, %v", res, err)
	}
	if b, _ := os.ReadFile(props); string(b) != "motd=mine\n" {
		t.Fatalf("on disk %q, want the bytes as sent", b)
	}
}

func assertNoTemporaries(t *testing.T, dir string) {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range des {
		if strings.Contains(de.Name(), ".felis-edit-") {
			t.Fatalf("temporary %s left behind", de.Name())
		}
	}
}

func TestReadRedactsLimboForwardingSecret(t *testing.T) {
	root := t.TempDir()
	props := "spawn-x=8\nforwarding-secrets=shared-key\nvelocity-modern=true\n"
	if err := os.WriteFile(filepath.Join(root, "server.properties"), []byte(props), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "server.properties"), filepath.Join(root, "copy.properties")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"server.properties", "copy.properties"} {
		res, err := run(root, OpRead, path, nil, "")
		if err != nil || res.Code != "" {
			t.Fatalf("read: %+v %v", res, err)
		}
		if strings.Contains(string(res.Content), "shared-key") || !strings.Contains(string(res.Content), "spawn-x=8") {
			t.Fatalf("unsafe redaction: %s", res.Content)
		}
	}
}
