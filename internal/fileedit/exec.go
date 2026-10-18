package fileedit

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"felis.lolicon.best/internal/naming"
)

// World owners can upload executable plugins inside their server mount; all paths remain contained by os.Root.
const (
	OpList   = "list"
	OpRead   = "read"
	OpWrite  = "write"
	OpMkdir  = "mkdir"
	OpDelete = "delete"
	OpRename = "rename"
	OpUpload = "upload"
	OpUnzip  = "unzip"
)

func mutates(op string) bool { return op != OpList && op != OpRead }

func validOp(op string) bool {
	switch op {
	case OpList, OpRead, OpWrite, OpMkdir, OpDelete, OpRename, OpUpload, OpUnzip:
		return true
	}
	return false
}

// Caller mistakes return Result codes; infrastructure failures return errors and exit nonzero.
const (
	CodeBadPath  = "bad_path"
	CodeNotFound = "not_found"
	CodeTooLarge = "too_large"
	CodeConflict = "conflict"
	// CodeNoSpace preserves the original file because replacement is atomic.
	CodeNoSpace        = "no_space"
	CodeExists         = "exists"
	CodeDigestMismatch = "digest_mismatch"
)

// ResultPrefix identifies compact JSON in merged stdout and stderr; readers select the last marked line.
const ResultPrefix = "FELIS-FILES-RESULT: "

// ContentEnv splits base64 content across variables below execve's per-string limit; jobs:get can read the content in the spec.
const (
	ContentEnv      = "FELIS_FILE_CONTENT"
	ContentPartsEnv = ContentEnv + "_PARTS"
	contentChunk    = 64 << 10
)

// UploadTokenEnv grants one fetch of this Job's staged upload.
const UploadTokenEnv = "FELIS_UPLOAD_TOKEN"

// Write size bounds the Job spec, read and listing limits bound pod-log memory, and one-request uploads stay below the edge body limit.
const (
	MaxWriteBytes  = 256 << 10
	MaxReadBytes   = 1 << 20
	MaxEntries     = 2000
	MaxUploadBytes = 64 << 20
)

type Entry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// Result.Content is []byte so JSON preserves arbitrary bytes through base64 encoding.
type Result struct {
	// Code and Error are both empty on success.
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`

	Entries   []Entry `json:"entries,omitempty"`
	Content   []byte  `json:"content,omitempty"`
	Truncated bool    `json:"truncated,omitempty"`
	// SHA256 covers on-disk bytes before redaction, or the current file on conflict; use it as the next write's expected hash.
	SHA256 string `json:"sha256,omitempty"`
	// ContentSHA256 covers transmitted bytes after redaction.
	ContentSHA256 string `json:"content_sha256,omitempty"`

	// Conflicts is sorted and capped by count and name bytes; ConflictCount reports the total.
	Conflicts     []string `json:"conflicts,omitempty"`
	ConflictCount int      `json:"conflict_count,omitempty"`
	Entry         string   `json:"entry,omitempty"`
	// Need and Avail are bytes; listings use Avail=-1 when free space is unknown.
	Need  int64 `json:"need,omitempty"`
	Avail int64 `json:"avail,omitempty"`
	Files int   `json:"files,omitempty"`
	Bytes int64 `json:"bytes,omitempty"`
}

type Request struct {
	Op   string
	Path string
	To   string
	// Expect conditions writes on the current file hash; ContentSHA256 verifies incoming bytes before any modification.
	Content       []byte
	Expect        string
	ContentSHA256 string
	CreateOnly    bool
	Upload        *Upload
	Overwrite     bool
	// Progress runs frequently from the copy loop; the caller must throttle updates.
	Progress func(done, total int64)
}

// Upload verifies both declared size and SHA-256 before replacing the destination.
type Upload struct {
	Size   int64
	SHA256 string
	// Open runs only after target checks pass so rejected uploads do not fetch bytes.
	Open func() (io.ReadCloser, error)
	// Landed releases staging only after success; failed Jobs can retry without reuploading.
	Landed func()
}

// Execute routes every filesystem access through os.Root; absolute paths and escapes remain errors rather than being reinterpreted.
func Execute(root string, req Request) (Result, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return Result{}, fmt.Errorf("open world root %q: %w", root, err)
	}
	defer r.Close()

	path := req.Path
	if path == "" {
		path = "."
	}

	switch req.Op {
	case OpList:
		return list(r, root, path), nil
	case OpRead:
		return read(r, path), nil
	case OpWrite:
		return write(r, path, req.Content, req.ContentSHA256, req.Expect, req.CreateOnly), nil
	case OpMkdir:
		return mkdir(r, path), nil
	case OpDelete:
		return remove(r, path), nil
	case OpRename:
		return rename(r, path, req.To), nil
	case OpUpload:
		if req.Upload == nil {
			return Result{}, errors.New("an upload needs a source")
		}
		return upload(r, root, path, *req.Upload, req.Overwrite, req.Progress)
	case OpUnzip:
		return unzip(r, root, path, req.Overwrite, req.Progress), nil
	default:
		return Result{}, fmt.Errorf("unknown op %q", req.Op)
	}
}

func list(r *os.Root, rootPath, path string) Result {
	f, err := r.Open(path)
	if err != nil {
		return failure(err, path)
	}
	defer f.Close()

	// Read one extra entry to detect truncation without traversing the whole directory.
	dirents, err := f.ReadDir(MaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return failure(err, path)
	}

	truncated := len(dirents) > MaxEntries
	if truncated {
		dirents = dirents[:MaxEntries]
	}

	entries := make([]Entry, 0, len(dirents))
	for _, de := range dirents {
		e := Entry{Name: de.Name(), IsDir: de.IsDir()}
		// Retain names with zero size and time when an entry disappears between ReadDir and Info.
		if info, err := de.Info(); err == nil {
			e.Size, e.ModTime = info.Size(), info.ModTime()
		}
		entries = append(entries, e)
	}
	res := Result{Entries: entries, Truncated: truncated, Avail: -1}
	if avail, _, err := statfs(rootPath); err == nil {
		res.Avail = int64(min(avail, math.MaxInt64))
	}
	return res
}

// secretConfigPath holds the shared forwarding key; deny reads to prevent cross-server credential exposure.
const secretConfigPath = "config/paper-global.yml"

func read(r *os.Root, name string) Result {
	// Use Linux path cleaning to recognize alternate spellings of the protected config path.
	if path.Clean(name) == secretConfigPath {
		return Result{Code: CodeBadPath, Error: fmt.Sprintf(
			"%s holds the proxy forwarding secret, which is shared cluster-wide, and is not readable through the editor", name)}
	}

	f, err := r.Open(name)
	if err != nil {
		return failure(err, name)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return failure(err, name)
	}
	if info.IsDir() {
		return Result{Code: CodeBadPath, Error: fmt.Sprintf("%s is a directory, not a file", name)}
	}
	withhold, redact := NewGuard(r).Rule(info)
	if withhold {
		return Result{Code: CodeBadPath, Error: fmt.Sprintf(
			"%s is the file holding the proxy forwarding secret, which is shared cluster-wide, and is not readable through the editor", name)}
	}
	if info.Size() > MaxReadBytes {
		return Result{Code: CodeTooLarge, Error: fmt.Sprintf(
			"%s is %d bytes; the editor reads at most %d", name, info.Size(), MaxReadBytes)}
	}

	// LimitReader also bounds files that grow after Stat.
	b, err := io.ReadAll(io.LimitReader(f, MaxReadBytes))
	if err != nil {
		return failure(err, name)
	}
	content := b
	if redact {
		content = RedactProps(b)
	}
	return Result{Content: content, SHA256: digest(b), ContentSHA256: digest(content)}
}

const (
	propsPath       = "server.properties"
	rconPasswordKey = "rcon.password"
	redactedValue   = "<redacted by felis>"
)

// redactSecretProps hides RCON and Limbo keys; entrypoints restore the real values on boot if placeholders are saved.
func redactSecretProps(name string, content []byte) []byte {
	if path.Clean(name) != propsPath {
		return content
	}
	lines := bytes.Split(content, []byte("\n"))
	for i, line := range lines {
		for _, key := range []string{rconPasswordKey, "forwarding-secrets"} {
			if bytes.HasPrefix(bytes.TrimSpace(line), []byte(key+"=")) {
				lines[i] = []byte(key + "=" + redactedValue)
			}
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// write checks the expected file hash and incoming content digest before atomic replacement; it never creates parent directories.
func write(r *os.Root, name string, content []byte, sum, expect string, createOnly bool) Result {
	if sum != "" {
		if got := digest(content); got != sum {
			return Result{Code: CodeDigestMismatch, Error: fmt.Sprintf(
				"the content hashes to %s and was sent as %s; nothing was written", got, sum)}
		}
	}
	if len(content) > MaxWriteBytes {
		return Result{Code: CodeTooLarge, Error: fmt.Sprintf(
			"content is %d bytes; the editor writes at most %d", len(content), MaxWriteBytes)}
	}
	target, mode, res := landingTarget(r, name, !createOnly)
	if res.Code != "" {
		return res
	}
	if name == naming.ExperienceConfigFile && target != name {
		return Result{Code: CodeBadPath, Error: "the experience config must be a regular file, not a symlink"}
	}
	if expect != "" {
		if res := checkUnchanged(r, name, target, expect); res.Code != "" {
			return res
		}
	}
	res, _ = land(r, name, target, mode, func(w io.Writer) error {
		_, err := w.Write(content)
		return err
	})
	if res.Code == "" {
		res.SHA256 = digest(content)
	}
	return res
}

// landingTarget follows only contained symlinks and preserves target mode; mayExist=false rejects every existing path, including dangling links.
func landingTarget(r *os.Root, name string, mayExist bool) (string, fs.FileMode, Result) {
	if !mayExist {
		if _, err := r.Lstat(name); err == nil {
			return "", 0, Result{Code: CodeExists, Error: fmt.Sprintf("%s already exists", name)}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", 0, failure(err, name)
		}
	}
	target, res := resolveLink(r, name)
	if res.Code != "" {
		return "", 0, res
	}
	info, err := r.Lstat(target)
	switch {
	case err == nil && info.IsDir():
		return "", 0, Result{Code: CodeBadPath, Error: fmt.Sprintf("%s is a directory, not a file", name)}
	case err == nil && !info.Mode().IsRegular():
		return "", 0, Result{Code: CodeBadPath, Error: fmt.Sprintf("%s is not a regular file", name)}
	case err == nil:
		return target, info.Mode().Perm(), Result{}
	case errors.Is(err, fs.ErrNotExist):
		return target, 0o644, Result{}
	default:
		return "", 0, failure(err, name)
	}
}

// transferError distinguishes failed upload fetches from caller-facing filesystem errors.
type transferError struct{ err error }

func (e *transferError) Error() string { return "fetch upload: " + e.err.Error() }
func (e *transferError) Unwrap() error { return e.err }

const NameMax = 255

// land writes and syncs a temporary sibling before rename so failures preserve either the old or complete new file.
func land(r *os.Root, name, target string, mode fs.FileMode, fill func(io.Writer) error) (Result, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return Result{Code: CodeBadPath, Error: fmt.Sprintf("generate a temporary name: %v", err)}, nil
	}
	tmp := path.Join(path.Dir(target), ".felis-edit-"+hex.EncodeToString(suffix[:]))
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return writeFailure(err, name), nil
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = r.Remove(tmp)
		}
	}()
	// Apply mode explicitly because creation permissions are filtered by umask.
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return writeFailure(err, name), nil
	}
	if err := fill(f); err != nil {
		f.Close()
		var te *transferError
		if errors.As(err, &te) {
			return Result{}, err
		}
		return writeFailure(err, name), nil
	}
	// Sync before rename for durability and check Close errors instead of discarding buffered-write failures.
	if err := syncWritten(f); err != nil {
		f.Close()
		return writeFailure(err, name), nil
	}
	if err := f.Close(); err != nil {
		return writeFailure(err, name), nil
	}
	// Return ownership to the game UID so the server can rewrite the file; prepare-data repairs leftovers on restart.
	_ = ownWritten(r, tmp)
	if err := r.Rename(tmp, target); err != nil {
		return writeFailure(err, name), nil
	}
	renamed = true
	syncDir(r, path.Dir(target))
	return Result{}, nil
}

// syncDir is best effort after a committed change so a flush failure cannot make the completed edit appear retryable.
func syncDir(r *os.Root, dir string) {
	if d, err := r.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// upload checks available space before fetching and requires the full replacement size free beside the existing file.
func upload(r *os.Root, rootPath, name string, u Upload, overwrite bool, progress func(done, total int64)) (Result, error) {
	target, mode, res := landingTarget(r, name, overwrite)
	if res.Code != "" {
		return res, nil
	}
	if avail, _, err := statfs(rootPath); err == nil && uint64(u.Size) > avail {
		return Result{Code: CodeNoSpace, Need: u.Size, Avail: int64(min(avail, math.MaxInt64)), Error: fmt.Sprintf(
			"%s is %d bytes and the server's volume has %d free; nothing was changed", name, u.Size, avail)}, nil
	}
	res, err := land(r, name, target, mode, func(w io.Writer) error {
		body, err := u.Open()
		if err != nil {
			return &transferError{err}
		}
		defer body.Close()
		h := sha256.New()
		var done int64
		out := io.MultiWriter(w, h)
		if progress != nil {
			out = countingWriter{out, func(n int) { done += int64(n); progress(done, u.Size) }}
		}
		n, err := io.Copy(out, sourceReader{io.LimitReader(body, u.Size+1)})
		if err != nil {
			return err
		}
		if n != u.Size {
			return &transferError{fmt.Errorf("got %d bytes, expected %d", n, u.Size)}
		}
		if sum := hex.EncodeToString(h.Sum(nil)); sum != u.SHA256 {
			return &transferError{fmt.Errorf("got sha256 %s, expected %s", sum, u.SHA256)}
		}
		return nil
	})
	if err == nil && res.Code == "" && u.Landed != nil {
		u.Landed()
	}
	return res, err
}

// sourceReader distinguishes network read errors from destination-space failures.
type sourceReader struct{ r io.Reader }

func (s sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		err = &transferError{err}
	}
	return n, err
}

func mkdir(r *os.Root, name string) Result {
	name = path.Clean(name)
	if name == "." {
		return Result{Code: CodeBadPath, Error: "a folder needs a name"}
	}
	if err := r.Mkdir(name, 0o755); err != nil {
		switch {
		case errors.Is(err, fs.ErrExist):
			return Result{Code: CodeExists, Error: fmt.Sprintf("%s already exists", name)}
		case errors.Is(err, fs.ErrNotExist):
			return Result{Code: CodeNotFound, Error: fmt.Sprintf("folder %s does not exist", path.Dir(name))}
		}
		return writeFailure(err, name)
	}
	_ = ownWritten(r, name)
	syncDir(r, path.Dir(name))
	return Result{}
}

// remove deletes symlinks without following them and refuses the world root; Lstat detects missing paths that RemoveAll ignores.
func remove(r *os.Root, name string) Result {
	name = path.Clean(name)
	if name == "." {
		return Result{Code: CodeBadPath, Error: "the server's root folder cannot be deleted"}
	}
	if _, err := r.Lstat(name); err != nil {
		return failure(err, name)
	}
	if err := r.RemoveAll(name); err != nil {
		return failure(err, name)
	}
	syncDir(r, path.Dir(name))
	return Result{}
}

func rename(r *os.Root, from, to string) Result {
	from, to = path.Clean(from), path.Clean(to)
	if from == "." || to == "." {
		return Result{Code: CodeBadPath, Error: "the server's root folder cannot be moved"}
	}
	info, err := r.Lstat(from)
	if err != nil {
		return failure(err, from)
	}
	if res := guardMove(r, from, info); res.Code != "" {
		return res
	}
	if _, err := r.Lstat(to); err == nil {
		return Result{Code: CodeExists, Error: fmt.Sprintf("%s already exists", to)}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return failure(err, to)
	}
	if err := r.Rename(from, to); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Result{Code: CodeNotFound, Error: fmt.Sprintf("folder %s does not exist", path.Dir(to))}
		}
		return failure(err, to)
	}
	syncDir(r, path.Dir(from))
	syncDir(r, path.Dir(to))
	return Result{}
}

// guardedPaths cannot be renamed because moving secret-bearing files would bypass name-based read protection.
var guardedPaths = []string{secretConfigPath, path.Dir(secretConfigPath), propsPath}

// guardMove compares file identity so aliases and contained symlinks cannot bypass protected-path checks.
func guardMove(r *os.Root, from string, info fs.FileInfo) Result {
	for _, g := range guardedPaths {
		for _, stat := range []func(string) (fs.FileInfo, error){r.Lstat, r.Stat} {
			if gi, err := stat(g); err == nil && os.SameFile(info, gi) {
				return Result{Code: CodeBadPath, Error: fmt.Sprintf(
					"%s is managed by felis and cannot be moved or renamed", from)}
			}
		}
	}
	return Result{}
}

func writeFailure(err error, name string) Result {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return Result{Code: CodeNoSpace, Error: fmt.Sprintf(
			"the server's volume is full; %s was left unchanged", name)}
	}
	return failure(err, name)
}

const maxLinkHops = 8

// resolveLink follows final-component symlinks relative to their containing directory and rejects absolute or escaping targets.
func resolveLink(r *os.Root, name string) (string, Result) {
	cur := name
	for range maxLinkHops {
		info, err := r.Lstat(cur)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			return cur, Result{}
		}
		dest, err := r.Readlink(cur)
		if err != nil {
			return "", failure(err, name)
		}
		if path.IsAbs(dest) {
			return "", Result{Code: CodeBadPath, Error: fmt.Sprintf(
				"%s is a symbolic link to %s, outside the server's files", name, dest)}
		}
		cur = path.Join(path.Dir(cur), dest)
		if cur == ".." || strings.HasPrefix(cur, "../") {
			return "", Result{Code: CodeBadPath, Error: fmt.Sprintf(
				"%s is a symbolic link leading outside the server's files", name)}
		}
	}
	return "", Result{Code: CodeBadPath, Error: fmt.Sprintf("%s: too many levels of symbolic links", name)}
}

// checkUnchanged treats files beyond MaxReadBytes as conflicts because they could not be the version the client read.
func checkUnchanged(r *os.Root, name, target, expect string) Result {
	conflict := func(now, why string) Result {
		return Result{Code: CodeConflict, SHA256: now, Error: fmt.Sprintf(
			"%s %s since it was opened; reload it, or save again to overwrite", name, why)}
	}
	f, err := r.Open(target)
	if errors.Is(err, fs.ErrNotExist) {
		return conflict("", "was deleted")
	}
	if err != nil {
		return failure(err, name)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return failure(err, name)
	}
	if info.Size() > MaxReadBytes {
		return conflict("", "has changed")
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, MaxReadBytes+1)); err != nil {
		return failure(err, name)
	}
	if now := hex.EncodeToString(h.Sum(nil)); now != expect {
		return conflict(now, "has changed")
	}
	return Result{}
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var syncWritten = func(f *os.File) error { return f.Sync() }

// ownWritten changes ownership through os.Root so symlinks cannot redirect it outside the mount.
var ownWritten = func(r *os.Root, name string) error {
	return r.Chown(name, int(naming.GameUID), int(naming.GameGID))
}

// failure maps missing paths to not_found and other filesystem errors to bad_path, including os.Root containment failures.
func failure(err error, path string) Result {
	if errors.Is(err, fs.ErrNotExist) {
		return Result{Code: CodeNotFound, Error: fmt.Sprintf("%s does not exist", path)}
	}
	return Result{Code: CodeBadPath, Error: err.Error()}
}

// Print emits compact JSON on one marked log line; multiline output cannot be recovered by the prefix reader.
func Print(w io.Writer, res Result) error {
	b, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s%s\n", ResultPrefix, b)
	return err
}
