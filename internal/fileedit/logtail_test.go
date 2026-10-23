package fileedit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

func logTailFixture(t *testing.T, content []byte) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, logTailPath), content, 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func executeLogTail(t *testing.T, root, cursor string) LogTailPage {
	t.Helper()
	res, err := Execute(root, Request{Op: OpLogTail, Cursor: cursor})
	if err != nil || res.Code != "" || res.LogTail == nil {
		t.Fatalf("log page: result=%+v error=%v", res, err)
	}
	page := *res.LogTail
	sum := sha256.Sum256(page.Content)
	if page.Content == nil || page.ContentSHA256 != hex.EncodeToString(sum[:]) || len(page.Content) > MaxLogTailBytes {
		t.Fatalf("invalid page content: bytes=%d digest=%q nil=%v", len(page.Content), page.ContentSHA256, page.Content == nil)
	}
	if page.HasMore != (page.NextCursor != "") || len(page.NextCursor) > MaxLogTailCursorBytes {
		t.Fatalf("invalid page cursor: next=%q more=%v", page.NextCursor, page.HasMore)
	}
	return page
}

func TestLogTailPagesNaturalLines(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		for _, count := range []int{200, 201, 405} {
			for _, terminated := range []bool{false, true} {
				t.Run(fmt.Sprintf("%q/%d/terminated=%v", newline, count, terminated), func(t *testing.T) {
					lines := make([]string, count)
					for i := range lines {
						lines[i] = fmt.Sprintf("%03d: 世界", i)
					}
					ending := ""
					if terminated {
						ending = newline
					}
					body := []byte(strings.Join(lines, newline) + ending)
					root := logTailFixture(t, body)
					first := executeLogTail(t, root, "")
					want := strings.Join(lines[max(0, count-200):], newline) + ending
					if string(first.Content) != want || first.ByteLimited || first.HasMore != (count > 200) {
						t.Fatalf("first page: bytes=%d want=%d more=%v limited=%v", len(first.Content), len(want), first.HasMore, first.ByteLimited)
					}
					recovered := append([]byte{}, first.Content...)
					page := first
					for pages := 1; page.HasMore; pages++ {
						if pages > 3 {
							t.Fatal("pagination did not reach the beginning")
						}
						previous, err := parseLogTailCursor(page.NextCursor)
						if err != nil || previous.Before != int64(len(body)-len(recovered)) {
							t.Fatalf("cursor does not precede returned bytes: %+v, %v", previous, err)
						}
						page = executeLogTail(t, root, page.NextCursor)
						if len(page.Content) == 0 || page.ByteLimited {
							t.Fatalf("unexpected older page: %+v", page)
						}
						recovered = append(page.Content, recovered...)
					}
					if !bytes.Equal(recovered, body) {
						t.Fatalf("pages changed log bytes: got=%d want=%d", len(recovered), len(body))
					}
				})
			}
		}
	}
}

type logTailReads struct {
	reader io.ReaderAt
	count  int
	bytes  int
	offset int64
}

func (r *logTailReads) ReadAt(p []byte, offset int64) (int, error) {
	r.count++
	r.bytes += len(p)
	r.offset = offset
	return r.reader.ReadAt(p, offset)
}

func TestLogTailReadsBoundedWindowFromLargeLog(t *testing.T) {
	root := logTailFixture(t, nil)
	f, err := os.OpenFile(filepath.Join(root, logTailPath), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tail := []byte("older\n" + strings.Repeat("latest entry\n", 200))
	const prefix = 6 << 20
	if _, err := f.WriteAt(tail, prefix); err != nil {
		t.Fatal(err)
	}
	want := []byte(strings.Repeat("latest entry\n", 200))
	page := executeLogTail(t, root, "")
	if !bytes.Equal(page.Content, want) || !page.HasMore || page.ByteLimited {
		t.Fatalf("large log tail: bytes=%d more=%v limited=%v", len(page.Content), page.HasMore, page.ByteLimited)
	}
	reader := &logTailReads{reader: f}
	end := int64(prefix + len(tail))
	got, _, _, err := readLogTail(reader, end)
	if err != nil || !bytes.Equal(got, want) || reader.count != 1 || reader.bytes > 256<<10+1 || reader.offset < end-(256<<10+1) {
		t.Fatalf("tail IO was not bounded: reads=%d bytes=%d offset=%d error=%v", reader.count, reader.bytes, reader.offset, err)
	}
}

func TestLogTailByteLimitedPagesPreserveAllBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"split UTF8", []byte(strings.Repeat("界", 2*MaxLogTailBytes/3+50))},
		{"split CRLF", []byte("prefix\r\n" + strings.Repeat("x", MaxLogTailBytes+50) + "\r\n" + strings.Repeat("y", MaxLogTailBytes-1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := logTailFixture(t, tc.body)
			page := executeLogTail(t, root, "")
			if !page.ByteLimited || !page.HasMore {
				t.Fatalf("long line was not limited: bytes=%d more=%v limited=%v", len(page.Content), page.HasMore, page.ByteLimited)
			}
			if tc.name == "split UTF8" && utf8.Valid(page.Content) {
				t.Fatal("fixture did not place the byte boundary within a UTF8 character")
			}
			recovered := append([]byte{}, page.Content...)
			for pages := 1; page.HasMore; pages++ {
				if pages > 4 {
					t.Fatal("byte-limited pagination did not advance")
				}
				cursor, err := parseLogTailCursor(page.NextCursor)
				if err != nil || cursor.Before != int64(len(tc.body)-len(recovered)) {
					t.Fatalf("noncontiguous byte cursor: %+v %v", cursor, err)
				}
				page = executeLogTail(t, root, page.NextCursor)
				if len(page.Content) == 0 {
					t.Fatal("pagination returned an empty noninitial page")
				}
				recovered = append(page.Content, recovered...)
			}
			if !bytes.Equal(recovered, tc.body) || !utf8.Valid(recovered) {
				t.Fatalf("pagination lost raw bytes: got=%d want=%d", len(recovered), len(tc.body))
			}
		})
	}
}

func TestLogTailCursorRejectsChangedLogs(t *testing.T) {
	for _, change := range []string{"replace", "append", "truncate", "modify", "delete"} {
		t.Run(change, func(t *testing.T) {
			body := []byte(strings.Repeat("line\n", 201))
			root := logTailFixture(t, body)
			page := executeLogTail(t, root, "")
			path := filepath.Join(root, logTailPath)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "replace":
				other := filepath.Join(root, "new.log")
				if err := os.WriteFile(other, body, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(other, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(other, path); err != nil {
					t.Fatal(err)
				}
			case "append":
				if err := os.WriteFile(path, append(body, 'x'), 0600); err != nil {
					t.Fatal(err)
				}
			case "truncate":
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			case "modify":
				body[0] = 'L'
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			res, err := Execute(root, Request{Op: OpLogTail, Cursor: page.NextCursor})
			if err != nil || res.Code != CodeConflict || res.LogTail != nil {
				t.Fatalf("changed log accepted: result=%+v error=%v", res, err)
			}
		})
	}
}

func TestLogTailRejectsInvalidCursors(t *testing.T) {
	root := logTailFixture(t, []byte("one line\n"))
	cursors := []string{strings.Repeat("A", 257), "not-base64?", "AA\n"}
	for _, raw := range []string{`{}`, `{"v":2,"s":10,"b":1}`, `{"v":1,"s":-1,"b":0}`, `{"v":1,"s":10,"b":-1}`, `{"v":1,"s":10,"b":11}`, `{"v":1,"s":10,"b":"1"}`, `{"v":1,"s":10,"b":1} {}`} {
		cursors = append(cursors, base64.RawURLEncoding.EncodeToString([]byte(raw)))
	}
	for _, cursor := range cursors {
		res, err := Execute(root, Request{Op: OpLogTail, Cursor: cursor})
		if err != nil || res.Code != CodeBadPath || res.LogTail != nil {
			t.Fatalf("invalid cursor accepted: result=%+v error=%v", res, err)
		}
	}
}

func TestLogTailReturnsEmptyPageWhenMissingOrEmpty(t *testing.T) {
	for _, fixture := range []string{"no logs", "no latest", "empty"} {
		t.Run(fixture, func(t *testing.T) {
			root := t.TempDir()
			if fixture != "no logs" {
				if err := os.Mkdir(filepath.Join(root, "logs"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if fixture == "empty" {
				if err := os.WriteFile(filepath.Join(root, logTailPath), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			page := executeLogTail(t, root, "")
			if len(page.Content) != 0 || page.ContentSHA256 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" || page.HasMore || page.ByteLimited {
				t.Fatalf("missing or empty log: %+v", page)
			}
		})
	}
}

func TestLogTailRefusesLinksAndProtectedAliases(t *testing.T) {
	for _, fixture := range []string{"internal file link", "external file link", "internal directory link", "external directory link", "properties hard link", "secret hard link", "fifo", "directory"} {
		t.Run(fixture, func(t *testing.T) {
			root := logTailFixture(t, nil)
			log := filepath.Join(root, logTailPath)
			if err := os.Remove(log); err != nil {
				t.Fatal(err)
			}
			switch fixture {
			case "internal file link", "external file link":
				folder := root
				if fixture == "external file link" {
					folder = t.TempDir()
				}
				target := filepath.Join(folder, "other.log")
				if err := os.WriteFile(target, []byte("fake log\n"), 0600); err != nil {
					t.Fatal(err)
				}
				link := target
				if fixture == "internal file link" {
					link = "../other.log"
				}
				if err := os.Symlink(link, log); err != nil {
					t.Fatal(err)
				}
			case "internal directory link", "external directory link":
				folder := root
				if fixture == "external directory link" {
					folder = t.TempDir()
				}
				target := filepath.Join(folder, "existing-logs")
				if err := os.Mkdir(target, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "latest.log"), []byte("fake log\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(root, "logs")); err != nil {
					t.Fatal(err)
				}
				link := target
				if fixture == "internal directory link" {
					link = "existing-logs"
				}
				if err := os.Symlink(link, filepath.Join(root, "logs")); err != nil {
					t.Fatal(err)
				}
			case "properties hard link", "secret hard link":
				name := propsPath
				if fixture == "secret hard link" {
					name = secretConfigPath
				}
				target := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("rcon.password=fake-rcon\nforwarding-secrets=fake-forward\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(target, log); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(log, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(log, 0755); err != nil {
					t.Fatal(err)
				}
			}
			res, err := Execute(root, Request{Op: OpLogTail})
			if err != nil || res.Code != CodeBadPath || res.LogTail != nil {
				t.Fatalf("unsafe log accepted: result=%+v error=%v", res, err)
			}
		})
	}
}

func TestEditorLogTailValidatesPageDigest(t *testing.T) {
	content := []byte("last entry\n")
	sum := sha256.Sum256(content)
	cursor := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"d":1,"i":2,"s":100,"m":3,"b":40}`))
	runner := &fakeRunner{payload: mustPayload(t, Result{LogTail: &LogTailPage{Content: content, ContentSHA256: hex.EncodeToString(sum[:])}})}
	editor := &Editor{Runner: runner, Config: Config{Image: "img"}}
	page, err := editor.LogTail(context.Background(), "survival", cursor)
	if err != nil || !bytes.Equal(page.Content, content) || len(runner.got) != 1 || runner.got[0].Op != OpLogTail || runner.got[0].Cursor != cursor || mutates(runner.got[0].Op) {
		t.Fatalf("log page: %+v params=%+v error=%v", page, runner.got, err)
	}
	for _, result := range []Result{{}, {LogTail: &LogTailPage{Content: []byte("damaged"), ContentSHA256: hex.EncodeToString(sum[:])}}} {
		runner.payload = mustPayload(t, result)
		if _, err := editor.LogTail(context.Background(), "survival", ""); !errors.Is(err, ErrReadDamaged) {
			t.Fatalf("damaged page returned %v", err)
		}
	}
	runner.payload = mustPayload(t, Result{LogTail: &LogTailPage{ContentSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}})
	page, err = editor.LogTail(context.Background(), "survival", "")
	if err != nil || page.Content == nil || len(page.Content) != 0 {
		t.Fatalf("empty page: %+v %v", page, err)
	}
	calls := runner.calls
	if _, err := editor.LogTail(context.Background(), "survival", "invalid?"); !errors.Is(err, ErrBadPath) || runner.calls != calls {
		t.Fatalf("invalid cursor reached runner: calls=%d want=%d error=%v", runner.calls, calls, err)
	}
}
