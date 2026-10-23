package fileedit

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
)

const (
	logTailPath           = "logs/latest.log"
	logTailLines          = 200
	MaxLogTailBytes       = 256 << 10
	MaxLogTailCursorBytes = 256
)

type LogTailPage struct {
	Content       []byte `json:"content"`
	ContentSHA256 string `json:"content_sha256"`
	NextCursor    string `json:"next_cursor,omitempty"`
	HasMore       bool   `json:"has_more"`
	ByteLimited   bool   `json:"byte_limited"`
}

type logTailCursor struct {
	Version  int    `json:"v"`
	Device   uint64 `json:"d"`
	Inode    uint64 `json:"i"`
	Size     int64  `json:"s"`
	Modified int64  `json:"m"`
	Before   int64  `json:"b"`
}

func parseLogTailCursor(encoded string) (logTailCursor, error) {
	var cursor logTailCursor
	if encoded == "" {
		return cursor, nil
	}
	if len(encoded) > MaxLogTailCursorBytes {
		return cursor, ErrBadPath
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return cursor, ErrBadPath
	}
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.Version != 1 || cursor.Size < 0 || cursor.Before < 0 || cursor.Before > cursor.Size {
		return cursor, ErrBadPath
	}
	return cursor, nil
}

func logTailIdentity(info fs.FileInfo) (logTailCursor, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return logTailCursor{}, false
	}
	return logTailCursor{Version: 1, Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Size: info.Size(), Modified: info.ModTime().UnixNano(), Before: info.Size()}, true
}

func logTail(r *os.Root, encoded string) Result {
	cursor, err := parseLogTailCursor(encoded)
	if err != nil {
		return Result{Code: CodeBadPath, Error: "invalid log cursor"}
	}
	result := Result{LogTail: &LogTailPage{Content: []byte{}, ContentSHA256: digest(nil)}}
	changed := Result{Code: CodeConflict, Error: "the log file changed; reload its latest page"}
	guard := NewGuard(r)
	if res := noLinks(r, logTailPath); res.Code != "" {
		if res.Code == CodeNotFound {
			if encoded != "" {
				return changed
			}
			return result
		}
		return res
	}
	info, err := r.Lstat(logTailPath)
	if err != nil {
		return changed
	}
	if !info.Mode().IsRegular() {
		return Result{Code: CodeBadPath, Error: "the log must be a regular file"}
	}
	f, err := r.OpenFile(logTailPath, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return changed
		}
		return failure(err, logTailPath)
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil {
		return failure(err, logTailPath)
	}
	if !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return changed
	}
	if res := noLinks(r, logTailPath); res.Code != "" {
		return changed
	}
	withhold, redact := guard.Rule(current)
	currentWithhold, currentRedact := NewGuard(r).Rule(current)
	if withhold || redact || currentWithhold || currentRedact {
		return Result{Code: CodeBadPath, Error: "protected configuration cannot be read as a log"}
	}
	snapshot, ok := logTailIdentity(current)
	if !ok {
		return Result{Code: CodeBadPath, Error: "the log file identity is unavailable"}
	}
	before := snapshot.Before
	if encoded != "" {
		expected := snapshot
		expected.Before = cursor.Before
		if expected != cursor {
			return changed
		}
		before = cursor.Before
	}
	content, next, limited, err := readLogTail(f, before)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return changed
	}
	if err != nil {
		return failure(err, logTailPath)
	}
	after, err := f.Stat()
	if err != nil {
		return failure(err, logTailPath)
	}
	identity, ok := logTailIdentity(after)
	if !ok || identity != snapshot {
		return changed
	}
	landed, err := r.Lstat(logTailPath)
	if err != nil || !landed.Mode().IsRegular() || !os.SameFile(after, landed) {
		return changed
	}
	if res := noLinks(r, logTailPath); res.Code != "" {
		return changed
	}
	withhold, redact = NewGuard(r).Rule(after)
	if withhold || redact {
		return Result{Code: CodeBadPath, Error: "protected configuration cannot be read as a log"}
	}
	page := result.LogTail
	page.Content, page.ContentSHA256 = content, digest(content)
	page.HasMore, page.ByteLimited = next > 0, limited
	if page.HasMore {
		snapshot.Before = next
		raw, _ := json.Marshal(snapshot)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return result
}

func readLogTail(reader io.ReaderAt, before int64) ([]byte, int64, bool, error) {
	if before == 0 {
		return []byte{}, 0, false, nil
	}
	start := max(int64(0), before-MaxLogTailBytes)
	readStart := max(int64(0), start-1)
	// One preceding byte distinguishes a complete first line from a split CRLF.
	raw := make([]byte, before-readStart)
	if _, err := reader.ReadAt(raw, readStart); err != nil {
		return nil, 0, false, err
	}
	content := raw[start-readStart:]
	completeStart := start == 0 || raw[0] == '\n' || raw[0] == '\r' && content[0] != '\n'
	end := len(content)
	if content[end-1] == '\n' {
		end--
		if end > 0 && content[end-1] == '\r' {
			end--
		}
	} else if content[end-1] == '\r' {
		end--
	}
	lines := 1
	for i := end - 1; i >= 0; i-- {
		if content[i] != '\n' && content[i] != '\r' {
			continue
		}
		if lines == logTailLines {
			return content[i+1:], start + int64(i+1), false, nil
		}
		lines++
		if content[i] == '\n' && i > 0 && content[i-1] == '\r' {
			i--
		}
	}
	if completeStart {
		return content, start, start > 0 && lines < logTailLines, nil
	}
	if at := bytes.IndexAny(content, "\r\n"); at >= 0 {
		at++
		if content[at-1] == '\r' && at < len(content) && content[at] == '\n' {
			at++
		}
		if at < len(content) {
			return content[at:], start + int64(at), true, nil
		}
	}
	return content, start, true, nil
}
