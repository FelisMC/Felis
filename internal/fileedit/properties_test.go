package fileedit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const fakeSecretProperties = "# retained comment\nmotd=before\nrcon.password=fake-rcon\nforwarding-secrets=fake-forward\nunknown.setting=retained\nmax-players=20\n"

func TestWritePreservesRedactedProperties(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, propsPath, fakeSecretProperties)
	read, err := Execute(root, Request{Op: OpRead, Path: propsPath})
	if err != nil || read.Code != "" || bytes.Contains(read.Content, []byte("fake-rcon")) || bytes.Contains(read.Content, []byte("fake-forward")) {
		t.Fatalf("redacted read: %+v %v", read, err)
	}
	content := bytes.Replace(read.Content, []byte("max-players=20"), []byte("max-players=24"), 1)
	want := []byte(strings.Replace(fakeSecretProperties, "max-players=20", "max-players=24", 1))
	res, err := Execute(root, Request{Op: OpWrite, Path: propsPath, Content: content, ContentSHA256: digest(content), Expect: read.SHA256})
	if err != nil || res.Code != "" || res.SHA256 != digest(want) || res.SHA256 == digest(content) {
		t.Fatalf("redacted save: %+v %v", res, err)
	}
	body, err := os.ReadFile(filepath.Join(root, propsPath))
	if err != nil || !bytes.Equal(body, want) {
		t.Fatalf("ordinary edit lost stored properties: %q %v", body, err)
	}
	again, err := Execute(root, Request{Op: OpRead, Path: propsPath})
	if err != nil || again.Code != "" || !bytes.Equal(again.Content, content) || again.SHA256 != res.SHA256 {
		t.Fatalf("read after save: %+v %v", again, err)
	}
	assertNoTemporaries(t, root)
}

func TestWriteRedactedPropertiesPreservesNaturalLineEndings(t *testing.T) {
	for _, tc := range []struct{ name, ending string }{{"CRLF", "\r\n"}, {"CR", "\r"}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			original := strings.Join([]string{
				"  rcon.password=fake-rcon",
				"motd=before",
				"\tforwarding-secrets=fake-forward",
				"# retained comment",
				"",
				"unknown.setting=retained",
				"max-players=20",
			}, tc.ending)
			putTransferFile(t, root, propsPath, original)
			read, err := Execute(root, Request{Op: OpRead, Path: propsPath})
			wantRedacted := strings.NewReplacer("fake-rcon", redactedValue, "fake-forward", redactedValue).Replace(original)
			if err != nil || read.Code != "" || string(read.Content) != wantRedacted {
				t.Fatalf("redacted document changed prefixes or line endings: %q %+v %v", read.Content, read, err)
			}
			content := bytes.Replace(read.Content, []byte("max-players=20"), []byte("max-players=24"), 1)
			res, err := Execute(root, Request{Op: OpWrite, Path: propsPath, Content: content, ContentSHA256: digest(content), Expect: read.SHA256})
			want := strings.Replace(original, "max-players=20", "max-players=24", 1)
			if err != nil || res.Code != "" || res.SHA256 != digest([]byte(want)) {
				t.Fatalf("redacted save: %+v %v", res, err)
			}
			body, err := os.ReadFile(filepath.Join(root, propsPath))
			if err != nil || string(body) != want {
				t.Fatalf("roundtrip changed text outside the ordinary edit: %q %v; want %q", body, err, want)
			}
		})
	}
}

func TestWriteRedactedPropertiesKeepsConcurrentEdit(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, propsPath, fakeSecretProperties)
	read, err := Execute(root, Request{Op: OpRead, Path: propsPath})
	if err != nil || read.Code != "" {
		t.Fatalf("read: %+v %v", read, err)
	}
	current := strings.Replace(fakeSecretProperties, "fake-rcon", "newer-fake-rcon", 1)
	putTransferFile(t, root, propsPath, current)
	content := bytes.Replace(read.Content, []byte("max-players=20"), []byte("max-players=24"), 1)
	res, err := Execute(root, Request{Op: OpWrite, Path: propsPath, Content: content, ContentSHA256: digest(content), Expect: read.SHA256})
	if err != nil || res.Code != CodeConflict || res.SHA256 != digest([]byte(current)) {
		t.Fatalf("stale redacted save: %+v %v", res, err)
	}
	body, err := os.ReadFile(filepath.Join(root, propsPath))
	if err != nil || string(body) != current {
		t.Fatalf("concurrent edit changed: %q %v", body, err)
	}
	assertNoTemporaries(t, root)
}

func TestWriteRedactedPropertiesKeepsExplicitChangesAndDeletions(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"explicit values", "rcon.password=explicit-rcon\nforwarding-secrets=explicit-forward\n", "rcon.password=explicit-rcon\nforwarding-secrets=explicit-forward\n"},
		{"delete both", "motd=after\n", "motd=after\n"},
		{"delete forwarding", "rcon.password=" + redactedValue + "\n", "rcon.password=fake-rcon\n"},
		{"delete rcon", "forwarding-secrets=" + redactedValue + "\n", "forwarding-secrets=fake-forward\n"},
		{"mixed values", "rcon.password=" + redactedValue + "\nforwarding-secrets=explicit-forward\n", "rcon.password=fake-rcon\nforwarding-secrets=explicit-forward\n"},
		{"exact placeholders only", "rcon.password=" + redactedValue + " \nforwarding-secrets=" + redactedValue + "-suffix\n", "rcon.password=" + redactedValue + " \nforwarding-secrets=" + redactedValue + "-suffix\n"},
		{"carriage return lines", "motd=after\r\nrcon.password=" + redactedValue + "\r\nforwarding-secrets=" + redactedValue + "\r\n", "motd=after\r\nrcon.password=fake-rcon\r\nforwarding-secrets=fake-forward\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			putTransferFile(t, root, propsPath, fakeSecretProperties)
			content := []byte(tc.content)
			res, err := Execute(root, Request{Op: OpWrite, Path: propsPath, Content: content, ContentSHA256: digest(content), Expect: digest([]byte(fakeSecretProperties))})
			if err != nil || res.Code != "" || res.SHA256 != digest([]byte(tc.want)) {
				t.Fatalf("save: %+v %v", res, err)
			}
			body, err := os.ReadFile(filepath.Join(root, propsPath))
			if err != nil || string(body) != tc.want {
				t.Fatalf("saved document=%q error=%v; want %q", body, err, tc.want)
			}
		})
	}
}

func TestWriteRedactedPropertiesThroughEquivalentSymbolicPaths(t *testing.T) {
	for _, name := range []string{"direct.properties", "nested.properties", "directory-link/server.properties", "relocated-root"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			putTransferFile(t, root, propsPath, fakeSecretProperties)
			symlink(t, propsPath, filepath.Join(root, "direct.properties"))
			symlink(t, "direct.properties", filepath.Join(root, "nested.properties"))
			symlink(t, ".", filepath.Join(root, "directory-link"))
			requestPath := name
			if name == "relocated-root" {
				if err := os.Rename(filepath.Join(root, propsPath), filepath.Join(root, "actual.properties")); err != nil {
					t.Fatal(err)
				}
				symlink(t, "actual.properties", filepath.Join(root, propsPath))
				requestPath = propsPath
			}
			read, err := Execute(root, Request{Op: OpRead, Path: requestPath})
			if err != nil || read.Code != "" {
				t.Fatalf("read: %+v %v", read, err)
			}
			content := bytes.Replace(read.Content, []byte("max-players=20"), []byte("max-players=24"), 1)
			res, err := Execute(root, Request{Op: OpWrite, Path: requestPath, Content: content, ContentSHA256: digest(content), Expect: read.SHA256})
			if err != nil || res.Code != "" {
				t.Fatalf("symbolic save: %+v %v", res, err)
			}
			body, err := os.ReadFile(filepath.Join(root, propsPath))
			want := strings.Replace(fakeSecretProperties, "max-players=20", "max-players=24", 1)
			if err != nil || string(body) != want {
				t.Fatalf("real properties were not preserved: %q %v", body, err)
			}
			again, err := Execute(root, Request{Op: OpRead, Path: requestPath})
			if err != nil || again.Code != "" || bytes.Contains(again.Content, []byte("fake-rcon")) || bytes.Contains(again.Content, []byte("fake-forward")) {
				t.Fatalf("symbolic read leaked preserved values: %+v %v", again, err)
			}
		})
	}
}

func TestWriteRedactedPropertiesRefusesExistingHardLinks(t *testing.T) {
	for _, name := range []string{propsPath, "hard.properties"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			putTransferFile(t, root, propsPath, fakeSecretProperties)
			if err := os.Link(filepath.Join(root, propsPath), filepath.Join(root, "hard.properties")); err != nil {
				t.Fatal(err)
			}
			read, err := Execute(root, Request{Op: OpRead, Path: name})
			if err != nil || read.Code != "" {
				t.Fatalf("read: %+v %v", read, err)
			}
			before := tree(t, root)
			res, err := Execute(root, Request{Op: OpWrite, Path: name, Content: read.Content, ContentSHA256: read.ContentSHA256, Expect: read.SHA256})
			if err != nil || res.Code != CodeBadPath {
				t.Fatalf("hard-link save: %+v %v", res, err)
			}
			assertSameTree(t, before, tree(t, root))
			for _, alias := range []string{propsPath, "hard.properties"} {
				again, err := Execute(root, Request{Op: OpRead, Path: alias})
				if err != nil || again.Code != "" || bytes.Contains(again.Content, []byte("fake-rcon")) || bytes.Contains(again.Content, []byte("fake-forward")) {
					t.Fatalf("hard-link read leaked values: %+v %v", again, err)
				}
			}
		})
	}
}

func TestWriteRedactedPropertiesDoesNotBorrowSecretsForOtherFiles(t *testing.T) {
	for _, name := range []string{"plugins/server.properties", "config/paper-global.yml", "new.properties"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			putTransferFile(t, root, propsPath, fakeSecretProperties)
			putTransferFile(t, root, name, "ordinary setting\n")
			content := []byte("rcon.password=" + redactedValue + "\nforwarding-secrets=" + redactedValue + "\n")
			res, err := Execute(root, Request{Op: OpWrite, Path: name, Content: content, ContentSHA256: digest(content)})
			if err != nil || res.Code != "" {
				t.Fatalf("ordinary write: %+v %v", res, err)
			}
			body, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || !bytes.Equal(body, content) {
				t.Fatalf("another file borrowed stored values: %q %v", body, err)
			}
		})
	}
}

func TestWriteRedactedPropertiesChecksInputBeforeRestoring(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, propsPath, fakeSecretProperties)
	if err := os.Link(filepath.Join(root, propsPath), filepath.Join(root, "hard.properties")); err != nil {
		t.Fatal(err)
	}
	content := []byte("rcon.password=" + redactedValue + "\nforwarding-secrets=" + redactedValue + "\n")
	before := tree(t, root)
	for _, tc := range []struct {
		name      string
		content   []byte
		sum, code string
	}{
		{"digest", content, strings.Repeat("0", 64), CodeDigestMismatch},
		{"size", append(append([]byte{}, content...), bytes.Repeat([]byte("x"), 5<<20)...), "", CodeTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Execute(root, Request{Op: OpWrite, Path: propsPath, Content: tc.content, ContentSHA256: tc.sum})
			if err != nil || res.Code != tc.code {
				t.Fatalf("invalid input: %+v %v", res, err)
			}
			assertSameTree(t, before, tree(t, root))
		})
	}
}

func TestWriteExplicitPropertiesThroughHardLinksKeepsExistingBehavior(t *testing.T) {
	for _, content := range []string{"rcon.password=explicit-rcon\nforwarding-secrets=explicit-forward\n", "motd=deleted-secrets\n"} {
		t.Run(content, func(t *testing.T) {
			root := t.TempDir()
			putTransferFile(t, root, propsPath, fakeSecretProperties)
			if err := os.Link(filepath.Join(root, propsPath), filepath.Join(root, "hard.properties")); err != nil {
				t.Fatal(err)
			}
			res, err := Execute(root, Request{Op: OpWrite, Path: "hard.properties", Content: []byte(content), ContentSHA256: digest([]byte(content)), Expect: digest([]byte(fakeSecretProperties))})
			if err != nil || res.Code != "" {
				t.Fatalf("explicit hard-link save: %+v %v", res, err)
			}
			body, err := os.ReadFile(filepath.Join(root, "hard.properties"))
			if err != nil || string(body) != content {
				t.Fatalf("explicit values changed: %q %v", body, err)
			}
			original, err := os.ReadFile(filepath.Join(root, propsPath))
			if err != nil || string(original) != fakeSecretProperties {
				t.Fatalf("other hard link changed: %q %v", original, err)
			}
		})
	}
}

func TestWriteRedactedPropertiesChecksRestoredSize(t *testing.T) {
	for _, extra := range []string{"", "motd=extra\n"} {
		t.Run(extra, func(t *testing.T) {
			root := t.TempDir()
			original := "rcon.password=" + strings.Repeat("r", (5<<20)-len("rcon.password=\n")) + "\n"
			putTransferFile(t, root, propsPath, original)
			content := []byte("rcon.password=" + redactedValue + "\n" + extra)
			res, err := Execute(root, Request{Op: OpWrite, Path: propsPath, Content: content, ContentSHA256: digest(content), Expect: digest([]byte(original))})
			if err != nil || extra == "" && (res.Code != "" || res.SHA256 != digest([]byte(original))) || extra != "" && res.Code != CodeTooLarge {
				t.Fatalf("restored size: %+v %v", res, err)
			}
			body, err := os.ReadFile(filepath.Join(root, propsPath))
			if err != nil || string(body) != original {
				t.Fatalf("stored properties changed on size check: bytes=%d error=%v", len(body), err)
			}
			assertNoTemporaries(t, root)
		})
	}
}

func TestWriteRedactedPropertiesStaysAtomicOnSyncFailure(t *testing.T) {
	root := t.TempDir()
	putTransferFile(t, root, propsPath, fakeSecretProperties)
	read, err := Execute(root, Request{Op: OpRead, Path: propsPath})
	if err != nil || read.Code != "" {
		t.Fatalf("read: %+v %v", read, err)
	}
	before := tree(t, root)
	previous := syncWritten
	syncWritten = func(*os.File) error { return syscall.ENOSPC }
	defer func() { syncWritten = previous }()
	content := bytes.Replace(read.Content, []byte("max-players=20"), []byte("max-players=24"), 1)
	res, err := Execute(root, Request{Op: OpWrite, Path: propsPath, Content: content, ContentSHA256: digest(content), Expect: read.SHA256})
	if err != nil || res.Code != CodeNoSpace {
		t.Fatalf("failed save: %+v %v", res, err)
	}
	assertSameTree(t, before, tree(t, root))
	assertNoTemporaries(t, root)
}
