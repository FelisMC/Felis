package fileedit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"go.yaml.in/yaml/v3"
)

const fakePaperGlobal = `# retained header
proxies:
  velocity:
    secret: 'fake-proxy-credential' # retained private comment
    enabled: true
chunk-loading-advanced:
  auto-config-send-distance: true
  player-max-concurrent-chunk-generates: -1
  player-max-concurrent-chunk-loads: 2
collisions:
  enable-player-collisions: true
  send-full-pos-for-hard-colliding-entities: false
commands:
  suggest-player-names-when-null-tab-completions: true
console:
  enable-brigadier-completions: false # retained switch comment
  enable-brigadier-highlighting: true
messages:
  use-display-name-in-quit-message: false
  no-permission: "Denied"
  kick:
    connection-throttle: 'Wait'
scoreboards:
  save-empty-scoreboard-teams: true
  track-plugin-scoreboards: false
player-auto-save:
  max-per-tick: 20
  rate: -1
watchdog:
  early-warning-delay: -20
  early-warning-every: 5000
chunk-loading-basic:
  player-max-chunk-generate-rate: 3.5
  player-max-chunk-load-rate: -1
  player-max-chunk-send-rate: 75
future.setting:
  enabled: true
  untouched: [one, 2, false, null]
`

func paperGlobalFixture(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	putTransferFile(t, root, secretConfigPath, content)
	return root
}

func paperGlobalPatchRequest(t *testing.T, values map[string]any, expect string) Request {
	t.Helper()
	content, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return Request{Op: OpPaperGlobalPatch, Content: content, ContentSHA256: digest(content), Expect: expect}
}

func assertPaperGlobalSafe(t *testing.T, result Result) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil || bytes.Contains(raw, []byte("fake-proxy-credential")) {
		t.Fatalf("protected result exposed private content: marshal=%v", err)
	}
}

func TestPaperGlobalReadExposesOnlyApprovedScalars(t *testing.T) {
	root := paperGlobalFixture(t, fakePaperGlobal)
	res, err := Execute(root, Request{Op: OpPaperGlobalRead})
	if err != nil || res.Code != "" || res.PaperGlobal == nil || len(res.PaperGlobal.Values) != 20 || res.PaperGlobal.SHA256 != digest([]byte(fakePaperGlobal)) || len(res.Content) != 0 {
		t.Fatalf("approved read: code=%q config=%v error=%v", res.Code, res.PaperGlobal != nil, err)
	}
	assertPaperGlobalSafe(t, res)
	if fmt.Sprint(res.PaperGlobal.Values["watchdog.early-warning-delay"]) != "-20" || res.PaperGlobal.Values["messages.kick.connection-throttle"] != "Wait" || res.PaperGlobal.Values["console.enable-brigadier-completions"] != false {
		t.Fatal("existing approved values were changed or omitted")
	}
	if err := os.Link(filepath.Join(root, secretConfigPath), filepath.Join(root, "alias.yml")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{secretConfigPath, "alias.yml"} {
		ordinary, err := Execute(root, Request{Op: OpRead, Path: name})
		if err != nil || ordinary.Code != CodeBadPath || len(ordinary.Content) != 0 {
			t.Fatalf("ordinary read bypassed guard: name=%q code=%q error=%v", name, ordinary.Code, err)
		}
	}
	filtered, err := Execute(root, Request{Op: OpPaperGlobalRead})
	if err != nil || filtered.Code != "" || filtered.PaperGlobal == nil {
		t.Fatalf("filtered canonical read with existing hard link: %+v %v", filtered, err)
	}
	assertPaperGlobalSafe(t, filtered)
}

func TestPaperGlobalPatchPreservesUnknownDataAndComments(t *testing.T) {
	root := paperGlobalFixture(t, fakePaperGlobal)
	values := map[string]any{"console.enable-brigadier-completions": true, "player-auto-save.rate": 40, "chunk-loading-basic.player-max-chunk-generate-rate": 0.25, "messages.no-permission": "New message"}
	res, err := Execute(root, paperGlobalPatchRequest(t, values, digest([]byte(fakePaperGlobal))))
	if err != nil || res.Code != "" || res.PaperGlobal == nil {
		t.Fatalf("patch: code=%q error=%v", res.Code, err)
	}
	assertPaperGlobalSafe(t, res)
	content, err := os.ReadFile(filepath.Join(root, secretConfigPath))
	if err != nil || res.PaperGlobal.SHA256 != digest(content) || res.PaperGlobal.SHA256 == digest([]byte(fakePaperGlobal)) {
		t.Fatalf("saved hash: %v", err)
	}
	var before, after map[string]any
	if yaml.Unmarshal([]byte(fakePaperGlobal), &before) != nil || yaml.Unmarshal(content, &after) != nil || !reflect.DeepEqual(before["proxies"], after["proxies"]) || !reflect.DeepEqual(before["future.setting"], after["future.setting"]) || !reflect.DeepEqual(before["watchdog"], after["watchdog"]) {
		t.Fatal("unselected configuration changed")
	}
	for _, text := range []string{"# retained header", "# retained private comment", "# retained switch comment", "fake-proxy-credential"} {
		if !bytes.Contains(content, []byte(text)) {
			t.Fatalf("saved document lost retained content %q", text)
		}
	}
	if !(bytes.Index(content, []byte("proxies:")) < bytes.Index(content, []byte("console:")) && bytes.Index(content, []byte("console:")) < bytes.Index(content, []byte("future.setting:"))) {
		t.Fatal("top-level field order changed")
	}
	if res.PaperGlobal.Values["console.enable-brigadier-completions"] != true || fmt.Sprint(res.PaperGlobal.Values["player-auto-save.rate"]) != "40" || res.PaperGlobal.Values["messages.no-permission"] != "New message" {
		t.Fatal("explicit patch values were not saved")
	}
	info, err := os.Stat(filepath.Join(root, secretConfigPath))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("saved configuration permissions: %v", err)
	}
	assertNoTemporaries(t, root)
}

func TestPaperGlobalNoOpPreservesExactDocumentAndInode(t *testing.T) {
	for _, values := range []map[string]any{{}, {"console.enable-brigadier-completions": false, "chunk-loading-basic.player-max-chunk-generate-rate": 3.5, "messages.no-permission": "Denied"}} {
		original := strings.ReplaceAll(fakePaperGlobal, "\n", "\r\n")
		root := paperGlobalFixture(t, original)
		path := filepath.Join(root, secretConfigPath)
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		res, err := Execute(root, paperGlobalPatchRequest(t, values, digest([]byte(original))))
		content, readErr := os.ReadFile(path)
		after, statErr := os.Stat(path)
		if err != nil || readErr != nil || statErr != nil || res.Code != "" || res.PaperGlobal == nil || res.PaperGlobal.SHA256 != digest([]byte(original)) || string(content) != original || !os.SameFile(before, after) {
			t.Fatalf("semantic no-op rewrote the document: code=%q errors=%v/%v/%v", res.Code, err, readErr, statErr)
		}
		assertNoTemporaries(t, root)
	}
}

func TestValidatePaperGlobalPatchChecksOnlyExplicitChanges(t *testing.T) {
	for _, values := range []map[string]any{nil, {}, {"player-auto-save.rate": -1}, {"watchdog.early-warning-delay": 0}, {"player-auto-save.max-per-tick": math.MaxInt32}, {"chunk-loading-basic.player-max-chunk-load-rate": -1.0}, {"chunk-loading-basic.player-max-chunk-load-rate": 0.125}, {"chunk-loading-basic.player-max-chunk-load-rate": 1e200}, {"messages.no-permission": strings.Repeat("界", 1365) + "x"}} {
		if err := ValidatePaperGlobalPatch(values); err != nil {
			t.Fatalf("valid explicit change refused: %v", err)
		}
	}
	for _, values := range []map[string]any{
		{"proxies.velocity.secret": "fake-proxy-credential"}, {"messages": map[string]any{"no-permission": "text"}}, {"messages.no-permission": nil}, {"console.enable-brigadier-completions": "true"},
		{"player-auto-save.rate": -2}, {"player-auto-save.rate": 1.5}, {"player-auto-save.rate": int64(math.MaxInt32) + 1}, {"watchdog.early-warning-delay": -1},
		{"chunk-loading-basic.player-max-chunk-load-rate": -0.5}, {"chunk-loading-basic.player-max-chunk-load-rate": math.Inf(1)}, {"chunk-loading-basic.player-max-chunk-load-rate": math.NaN()},
		{"messages.no-permission": strings.Repeat("界", 1366)}, {"messages.no-permission": "bad\x00text"}, {"messages.no-permission": string([]byte{0xff})},
	} {
		if err := ValidatePaperGlobalPatch(values); !errors.Is(err, ErrBadPath) || strings.Contains(err.Error(), "fake-proxy") {
			t.Fatalf("invalid patch validation returned %v", err)
		}
	}
}

func TestPaperGlobalRejectsUnsafeYAMLWithoutDisclosingSource(t *testing.T) {
	for name, content := range map[string]string{
		"anchor":             "private: &fake-proxy-credential value\n",
		"alias":              "private: *fake-proxy-credential\n",
		"merge":              "private:\n  <<: {secret: fake-proxy-credential}\n",
		"duplicate":          "private: {key: value, 'key': fake-proxy-credential}\n",
		"numeric key":        "private: {1: fake-proxy-credential}\n",
		"complex key":        "? [one, two]\n: fake-proxy-credential\n",
		"multiple documents": "private: value\n---\nsecret: fake-proxy-credential\n",
		"scalar document":    "fake-proxy-credential\n",
		"syntax":             "private: [fake-proxy-credential\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := paperGlobalFixture(t, content)
			for _, req := range []Request{{Op: OpPaperGlobalRead}, paperGlobalPatchRequest(t, map[string]any{}, digest([]byte(content)))} {
				res, err := Execute(root, req)
				if err != nil || res.Code != CodeBadPath || res.PaperGlobal != nil || len(res.Content) != 0 {
					t.Fatalf("unsafe YAML accepted: code=%q error=%v", res.Code, err)
				}
				assertPaperGlobalSafe(t, res)
			}
			body, err := os.ReadFile(filepath.Join(root, secretConfigPath))
			if err != nil || string(body) != content {
				t.Fatal("rejected YAML was changed")
			}
			assertNoTemporaries(t, root)
		})
	}
}

func TestPaperGlobalPatchRefusesMissingAndIncompatibleFields(t *testing.T) {
	for _, content := range []string{
		"private: fake-proxy-credential\n",
		"console: retained\n",
		"console:\n  enable-brigadier-completions: 'false'\n",
		"console.enable-brigadier-completions: false\n",
	} {
		root := paperGlobalFixture(t, content)
		read, err := Execute(root, Request{Op: OpPaperGlobalRead})
		if err != nil || read.Code != "" || read.PaperGlobal == nil || len(read.PaperGlobal.Values) != 0 {
			t.Fatalf("missing or incompatible field was exposed: code=%q error=%v", read.Code, err)
		}
		res, err := Execute(root, paperGlobalPatchRequest(t, map[string]any{"console.enable-brigadier-completions": true}, read.PaperGlobal.SHA256))
		if err != nil || res.Code != CodeBadPath {
			t.Fatalf("missing or incompatible field was created: code=%q error=%v", res.Code, err)
		}
		body, err := os.ReadFile(filepath.Join(root, secretConfigPath))
		if err != nil || string(body) != content {
			t.Fatal("refused field edit changed original text")
		}
		assertPaperGlobalSafe(t, read)
		assertPaperGlobalSafe(t, res)
		assertNoTemporaries(t, root)
	}
}

func TestPaperGlobalPreservesUnknownScalarTags(t *testing.T) {
	content := "console:\n  enable-brigadier-completions: false\nmessages:\n  no-permission: !future retained\nprivate: !future fake-proxy-credential\n"
	root := paperGlobalFixture(t, content)
	read, err := Execute(root, Request{Op: OpPaperGlobalRead})
	if err != nil || read.Code != "" || read.PaperGlobal == nil || len(read.PaperGlobal.Values) != 1 {
		t.Fatalf("custom tag filtering: code=%q error=%v", read.Code, err)
	}
	res, err := Execute(root, paperGlobalPatchRequest(t, map[string]any{"console.enable-brigadier-completions": true}, read.PaperGlobal.SHA256))
	if err != nil || res.Code != "" {
		t.Fatalf("unrelated edit with custom tags: code=%q error=%v", res.Code, err)
	}
	body, err := os.ReadFile(filepath.Join(root, secretConfigPath))
	if err != nil || !bytes.Contains(body, []byte("no-permission: !future retained")) || !bytes.Contains(body, []byte("private: !future fake-proxy-credential")) {
		t.Fatal("custom scalar tags were changed")
	}
	assertPaperGlobalSafe(t, read)
	assertPaperGlobalSafe(t, res)
}

func TestPaperGlobalInputChecksPreserveOriginal(t *testing.T) {
	root := paperGlobalFixture(t, fakePaperGlobal)
	base := paperGlobalPatchRequest(t, map[string]any{"console.enable-brigadier-completions": true}, digest([]byte(fakePaperGlobal)))
	for _, tc := range []struct {
		name   string
		code   string
		change func(*Request)
	}{
		{"digest", CodeDigestMismatch, func(r *Request) { r.ContentSHA256 = strings.Repeat("0", 64) }},
		{"required digest", CodeDigestMismatch, func(r *Request) { r.ContentSHA256 = "" }},
		{"required hash", CodeBadPath, func(r *Request) { r.Expect = "" }},
		{"invalid hash", CodeBadPath, func(r *Request) { r.Expect = strings.Repeat("A", 64) }},
		{"stale hash", CodeConflict, func(r *Request) { r.Expect = strings.Repeat("0", 64) }},
		{"fixed path", CodeBadPath, func(r *Request) { r.Path = secretConfigPath }},
		{"null patch", CodeBadPath, func(r *Request) { r.Content = []byte("null"); r.ContentSHA256 = digest(r.Content) }},
		{"oversized patch", CodeTooLarge, func(r *Request) {
			r.Content = bytes.Repeat([]byte(" "), (64<<10)+1)
			r.ContentSHA256 = digest(r.Content)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.change(&req)
			res, err := Execute(root, req)
			if err != nil || res.Code != tc.code {
				t.Fatalf("input check: code=%q want=%q error=%v", res.Code, tc.code, err)
			}
			assertPaperGlobalSafe(t, res)
			body, err := os.ReadFile(filepath.Join(root, secretConfigPath))
			if err != nil || string(body) != fakePaperGlobal {
				t.Fatal("invalid patch changed configuration")
			}
			assertNoTemporaries(t, root)
		})
	}
}

func TestPaperGlobalRejectsLinksAndNonRegularTargets(t *testing.T) {
	for _, fixture := range []string{"file symlink", "directory symlink", "hard link", "fifo", "directory"} {
		t.Run(fixture, func(t *testing.T) {
			root := paperGlobalFixture(t, fakePaperGlobal)
			path := filepath.Join(root, secretConfigPath)
			switch fixture {
			case "file symlink":
				if err := os.Rename(path, filepath.Join(root, "private.yml")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../private.yml", path); err != nil {
					t.Fatal(err)
				}
			case "directory symlink":
				if err := os.Rename(filepath.Join(root, "config"), filepath.Join(root, "existing-config")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("existing-config", filepath.Join(root, "config")); err != nil {
					t.Fatal(err)
				}
			case "hard link":
				if err := os.Link(path, filepath.Join(root, "alias.yml")); err != nil {
					t.Fatal(err)
				}
			case "fifo", "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if fixture == "fifo" {
					if err := syscall.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			requests := []Request{paperGlobalPatchRequest(t, map[string]any{"console.enable-brigadier-completions": true}, digest([]byte(fakePaperGlobal)))}
			if fixture != "hard link" {
				requests = append(requests, Request{Op: OpPaperGlobalRead})
			}
			for _, req := range requests {
				res, err := Execute(root, req)
				if err != nil || res.Code != CodeBadPath || res.PaperGlobal != nil {
					t.Fatalf("unsafe target: code=%q error=%v", res.Code, err)
				}
				assertPaperGlobalSafe(t, res)
			}
		})
	}
}

func TestPaperGlobalAtomicFailuresKeepOriginal(t *testing.T) {
	for _, failure := range []error{syscall.ENOSPC, errors.New("fake-proxy-credential")} {
		root := paperGlobalFixture(t, fakePaperGlobal)
		previous := syncWritten
		syncWritten = func(f *os.File) error {
			info, err := f.Stat()
			if err != nil || info.Mode().Perm() != 0600 || !InternalPath(filepath.Base(f.Name())) {
				t.Fatal("temporary configuration was not private")
			}
			return failure
		}
		res, err := Execute(root, paperGlobalPatchRequest(t, map[string]any{"console.enable-brigadier-completions": true}, digest([]byte(fakePaperGlobal))))
		syncWritten = previous
		want := CodeBadPath
		if errors.Is(failure, syscall.ENOSPC) {
			want = CodeNoSpace
		}
		if err != nil || res.Code != want {
			t.Fatalf("failed landing: code=%q want=%q error=%v", res.Code, want, err)
		}
		assertPaperGlobalSafe(t, res)
		body, err := os.ReadFile(filepath.Join(root, secretConfigPath))
		if err != nil || string(body) != fakePaperGlobal {
			t.Fatal("failed write changed the original")
		}
		assertNoTemporaries(t, root)
	}
}

func TestPaperGlobalRechecksIdentityAndHashBeforeLanding(t *testing.T) {
	for _, change := range []string{"same hash new inode", "changed bytes", "new parent", "new hard link"} {
		t.Run(change, func(t *testing.T) {
			root := paperGlobalFixture(t, fakePaperGlobal)
			path := filepath.Join(root, secretConfigPath)
			previous := syncWritten
			defer func() { syncWritten = previous }()
			syncWritten = func(f *os.File) error {
				if err := f.Sync(); err != nil {
					return err
				}
				switch change {
				case "same hash new inode":
					other := filepath.Join(root, "replacement.yml")
					if err := os.WriteFile(other, []byte(fakePaperGlobal), 0600); err != nil {
						return err
					}
					return os.Rename(other, path)
				case "changed bytes":
					return os.WriteFile(path, []byte(fakePaperGlobal+"# concurrent change\n"), 0600)
				case "new parent":
					if err := os.Rename(filepath.Join(root, "config"), filepath.Join(root, "old-config")); err != nil {
						return err
					}
					if err := os.Mkdir(filepath.Join(root, "config"), 0700); err != nil {
						return err
					}
					return os.WriteFile(path, []byte(fakePaperGlobal), 0600)
				case "new hard link":
					return os.Link(path, filepath.Join(root, "alias.yml"))
				}
				return nil
			}
			res, err := Execute(root, paperGlobalPatchRequest(t, map[string]any{"console.enable-brigadier-completions": true}, digest([]byte(fakePaperGlobal))))
			if err != nil || res.Code != CodeConflict {
				t.Fatalf("concurrent change accepted: code=%q error=%v", res.Code, err)
			}
			assertPaperGlobalSafe(t, res)
			body, err := os.ReadFile(path)
			want := fakePaperGlobal
			if change == "changed bytes" {
				want += "# concurrent change\n"
			}
			if err != nil || string(body) != want {
				t.Fatal("concurrent file was overwritten")
			}
			assertNoTemporaries(t, root)
		})
	}
}

func TestPaperGlobalBoundsReadAndEncodedWrite(t *testing.T) {
	oversized := fakePaperGlobal + "#" + strings.Repeat("x", (5<<20)-len(fakePaperGlobal))
	root := paperGlobalFixture(t, oversized)
	res, err := Execute(root, Request{Op: OpPaperGlobalRead})
	if err != nil || res.Code != CodeTooLarge {
		t.Fatalf("oversized read: code=%q error=%v", res.Code, err)
	}
	nearLimit := fakePaperGlobal + "#" + strings.Repeat("x", (5<<20)-len(fakePaperGlobal)-20) + "\n"
	root = paperGlobalFixture(t, nearLimit)
	res, err = Execute(root, paperGlobalPatchRequest(t, map[string]any{"messages.no-permission": strings.Repeat("a", 4096)}, digest([]byte(nearLimit))))
	if err != nil || res.Code != CodeTooLarge {
		t.Fatalf("oversized encoded write: code=%q error=%v", res.Code, err)
	}
	body, err := os.ReadFile(filepath.Join(root, secretConfigPath))
	if err != nil || string(body) != nearLimit {
		t.Fatal("size refusal changed the original")
	}
	assertNoTemporaries(t, root)
}

func TestEditorPaperGlobalValidatesWorkerResponses(t *testing.T) {
	sum := digest([]byte(fakePaperGlobal))
	runner := &fakeRunner{payload: mustPayload(t, Result{PaperGlobal: &PaperGlobalConfig{Values: map[string]any{"watchdog.early-warning-delay": -20}, SHA256: sum}})}
	editor := &Editor{Runner: runner, Config: Config{Image: "img"}}
	config, err := editor.ReadPaperGlobal(context.Background(), "survival")
	if err != nil || config.Values["watchdog.early-warning-delay"] != json.Number("-20") || runner.got[0].Op != OpPaperGlobalRead || runner.got[0].Path != "" {
		t.Fatalf("filtered read: %v", err)
	}
	if _, err := editor.PatchPaperGlobal(context.Background(), "survival", map[string]any{"console.enable-brigadier-completions": true}, sum); err != nil {
		t.Fatal(err)
	}
	params := runner.got[1]
	if params.Op != OpPaperGlobalPatch || params.Expect != sum || string(params.Content) != `{"console.enable-brigadier-completions":true}` {
		t.Fatal("patch parameters were not preserved")
	}
	for _, response := range []Result{{}, {PaperGlobal: &PaperGlobalConfig{SHA256: "invalid"}}, {PaperGlobal: &PaperGlobalConfig{SHA256: sum, Values: map[string]any{"proxies.velocity.secret": "fake-proxy-credential"}}}, {PaperGlobal: &PaperGlobalConfig{SHA256: sum, Values: map[string]any{"messages.no-permission": nil}}}, {PaperGlobal: &PaperGlobalConfig{SHA256: sum, Values: map[string]any{"player-auto-save.rate": 1.5}}}} {
		runner.payload = mustPayload(t, response)
		if _, err := editor.ReadPaperGlobal(context.Background(), "survival"); !errors.Is(err, ErrReadDamaged) {
			t.Fatalf("damaged response accepted: %v", err)
		}
	}
	runner.payload = mustPayload(t, Result{Code: CodeBadPath, Error: "fake-proxy-credential"})
	if _, err := editor.ReadPaperGlobal(context.Background(), "survival"); !errors.Is(err, ErrBadPath) || strings.Contains(err.Error(), "fake-proxy") {
		t.Fatalf("unsafe worker error: %v", err)
	}
	runner.err = errors.New("fake-proxy-credential")
	if _, err := editor.ReadPaperGlobal(context.Background(), "survival"); err == nil || strings.Contains(err.Error(), "fake-proxy") {
		t.Fatalf("unsafe infrastructure error: %v", err)
	}
}
