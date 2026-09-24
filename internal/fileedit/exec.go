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
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"felis.lolicon.best/internal/naming"
)

// The three operations the editor supports. The set is deliberately closed and
// tiny: list a directory, read a file, write a file. There is no rename, delete,
// or chmod — each would need its own containment and audit story, and none is
// required to fix a broken server.properties, which is what this subsystem exists
// for.
//
// A write DOES accept arbitrary bytes at any path inside the mount, and that is a
// real capability rather than an oversight: the root is the server's whole working
// directory (see Config.WorldsRoot), so an owner can write plugins/<x>.jar and
// Paper will load it on the next boot. It is the same power a hosting panel's file
// manager gives, scoped to a server the caller already owns and already controls
// through /command. Note what it is NOT scoped by: admin image curation. Images
// are admin-only (POST /images, POST /images/build) and modpack submissions need
// an admin verdict, so this is the one owner-tier route that lands executable code
// in a backend pod. That trade was made deliberately; if it is ever revisited, the
// guard belongs in write() below, which is the single choke point all three
// callers route through.
const (
	OpList  = "list"
	OpRead  = "read"
	OpWrite = "write"
)

// Result codes. A failure that is the CALLER's fault travels back as a Result
// with a Code rather than as a non-zero exit, so felis-api can map it onto a
// precise 4xx (handlers_files.go) instead of collapsing every failure into "the
// Job died" 500. Only an infrastructure failure — the world mount unreadable, the
// result unprintable — exits non-zero.
const (
	CodeBadPath  = "bad_path"  // escapes the world root, is absolute, or is otherwise unopenable
	CodeNotFound = "not_found" // resolves inside the root but nothing is there
	CodeTooLarge = "too_large" // the file exceeds MaxReadBytes
	// CodeConflict is a write whose expected hash no longer matches the file: it
	// changed (or vanished) after the caller read it, so saving would silently
	// discard someone else's edit.
	CodeConflict = "conflict"
	// CodeNoSpace is a write the volume had no room for. The file is unchanged
	// (the write is atomic), so this is the caller's volume being full rather
	// than a bad request.
	CodeNoSpace = "no_space"
)

// ResultPrefix marks the single stdout line carrying the JSON Result. The Job's
// output is read back through the pods/log subresource, which returns the
// container's stdout and stderr MERGED — so a Go runtime warning, a libc message,
// or anything else the container writes to stderr lands in the same stream. The
// marker is what makes the payload findable in that mixed stream: felis-api scans
// for the last line carrying this prefix rather than assuming the log is pure
// JSON. Without it any stray stderr byte would corrupt every response.
const ResultPrefix = "FELIS-FILES-RESULT: "

// ContentEnv is the environment variable the write path carries new file content
// in (base64). It travels on the Job spec felis-api creates, because felis-api
// holds `jobs: create` but NOT `secrets: create` in the minecraft namespace
// (internal/platform.APIMinecraftRole) — a Secret is not available to it, so the
// Job spec is the only channel into the Pod. The consequence is that written
// content is readable by anyone holding jobs:get in the minecraft namespace,
// which is a cluster-admin-level power; it is NOT readable by felis-operator,
// felis-reaper, or any weak Job SA, none of which hold that verb.
const ContentEnv = "FELIS_FILE_CONTENT"

// Size and count ceilings. Every one of them exists because the result travels
// through a Kubernetes object or a pod log, neither of which is an unbounded pipe:
//
//   - MaxWriteBytes bounds the env var on the Job spec. etcd refuses an object
//     over ~1.5MiB, and the base64 of the content is ~4/3 of it, so 256KiB leaves
//     an order of magnitude of headroom for the rest of the spec. Any real
//     server.properties / ops.json / bukkit.yml is a few KiB.
//   - MaxReadBytes bounds what a read pulls back through the pod log INTO
//     felis-api's memory. Without it a caller could name a 500MiB region file and
//     make the API buffer it — a trivial memory DoS from an ordinary owner-tier
//     request. 1MiB comfortably covers every config file and refuses world data.
//   - MaxEntries bounds a listing. A world's region/ directory legitimately holds
//     thousands of .mca files, so this truncates rather than errors (Truncated
//     says so), keeping the log line bounded while still being useful.
const (
	MaxWriteBytes = 256 << 10 // 256 KiB
	MaxReadBytes  = 1 << 20   // 1 MiB
	MaxEntries    = 2000
)

// Entry is one directory entry in a listing. It carries only what a file browser
// needs to render a row and decide whether the entry is descendable; mode bits,
// ownership, and inode data are deliberately absent — they are not actionable
// through this editor (there is no chmod/chown op) and would only widen what a
// listing discloses about the node.
type Entry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// Result is the single JSON object the Job prints and felis-api parses back. One
// shape covers all three ops so the transport has exactly one thing to find and
// unmarshal; the op decides which fields are populated.
//
// Content is []byte, so encoding/json base64-encodes it on the way out and
// decodes it on the way back with no hand-rolled codec. That is what makes the
// read path binary-safe: a config file with a stray non-UTF-8 byte round-trips
// intact instead of being mangled into U+FFFD by a string round-trip.
type Result struct {
	// Code and Error are set together on a caller-fault failure; both empty means
	// the op succeeded.
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`

	Entries []Entry `json:"entries,omitempty"`
	Content []byte  `json:"content,omitempty"`
	// Truncated reports that the listing hit MaxEntries and is incomplete, so a
	// client renders "showing first N" rather than silently implying the directory
	// is smaller than it is.
	Truncated bool `json:"truncated,omitempty"`
	// SHA256 is the hex digest of the file's on-disk bytes: after a read, the file
	// as read (before any redaction); after a write, the bytes written; on a
	// conflict, the file as it is now. A client hands it back as the expected hash
	// of its next write (see write).
	SHA256 string `json:"sha256,omitempty"`
}

// Execute performs op on the file named by path, resolved inside root, and returns
// the Result to print. root is the in-Pod mount path of the server's world PVC;
// path is the caller-supplied relative path underneath it.
//
// CONTAINMENT INVARIANT: every filesystem access goes through *os.Root, never
// through a path string this function assembled. os.Root is the stdlib's
// escape-proof directory handle — it resolves each component against the open root
// descriptor and refuses any traversal that would leave it, whether by "..", by an
// absolute path, or by a SYMLINK pointing outside. That last case is why the
// string-prefix check in internal/backup/tarlocal.go is not reused here: a prefix
// test validates the path as text, then opens it as a path, and between those two
// steps a symlink can be swapped in (TOCTOU). A world directory holds
// attacker-influenced content — players create files through ordinary gameplay,
// and plugins create more — so a symlink escaping to /etc or to another server's
// mount is a live threat, not a theoretical one. os.Root closes it structurally:
// there is no window between the check and the open because they are the same
// operation.
//
// The path is passed to os.Root verbatim apart from mapping "" to ".". In
// particular an ABSOLUTE path is NOT rewritten into a relative one — it is handed
// to os.Root as-is and refused. Silently reinterpreting "/etc/passwd" as
// "<root>/etc/passwd" would turn an unambiguous escape attempt into a successful
// read of a file the caller did not name, which is exactly the confusion this
// editor must not have.
//
// expect is a write's precondition: when non-empty, the write lands only if the
// file's current SHA-256 (hex) equals it. It is ignored by list and read.
func Execute(root, op, path string, content []byte, expect string) (Result, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		// The world mount itself is unopenable: infrastructure, not caller fault.
		return Result{}, fmt.Errorf("open world root %q: %w", root, err)
	}
	defer r.Close()

	if path == "" {
		path = "."
	}

	switch op {
	case OpList:
		return list(r, path), nil
	case OpRead:
		return read(r, path), nil
	case OpWrite:
		return write(r, path, content, expect), nil
	default:
		return Result{}, fmt.Errorf("unknown op %q", op)
	}
}

// list reads one directory. It does not recurse: a browser asks for one level at
// a time, and recursion would make both the result size and the traversal cost
// unbounded in a world directory.
func list(r *os.Root, path string) Result {
	f, err := r.Open(path)
	if err != nil {
		return failure(err, path)
	}
	defer f.Close()

	// ReadDir(MaxEntries+1) reads one MORE than the ceiling so the overflow is
	// detectable without walking the whole directory: if the extra entry came back,
	// the listing is truncated. io.EOF means the directory ended within the limit.
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
		// Info() can fail on an entry deleted between the ReadDir and the stat (a
		// running plugin rotating a log, say). That is not a reason to fail the whole
		// listing, so the entry is reported with a zero size/mtime rather than dropped
		// — a name that exists is still useful to the caller.
		if info, err := de.Info(); err == nil {
			e.Size, e.ModTime = info.Size(), info.ModTime()
		}
		entries = append(entries, e)
	}
	return Result{Entries: entries, Truncated: truncated}
}

// secretConfigPath is the one file in a world mount holding PLATFORM secret
// material rather than the owner's own configuration. felis-lobby's entrypoint
// writes FELIS_FORWARDING_SECRET into it on every boot, and that value is
// identical on every backend in the cluster — operator.buildEnv injects one Secret
// everywhere. Reading it out of a server you own would therefore hand you the
// Velocity modern-forwarding handshake key for EVERYONE's servers: cross-tenant
// material that merely happens to sit in your volume. Every other path here is the
// caller's own data, which is why this is the only denial.
//
// Refusing it costs no legitimate repair. The entrypoint rewrites the file whole
// on every boot and its own header says "Do not hand-edit", so an edit made
// through this editor could never survive a restart anyway. Only the READ is
// denied; a write is left alone because writing the file leaks nothing and is
// equally futile.
//
// An exact match on one cleaned path, not a pattern. This is the whole
// known exposure — grep FELIS_FORWARDING_SECRET across deploy/ — and if another
// image ever persists a platform secret into the mount, add its path here rather
// than inventing a matcher.
const secretConfigPath = "config/paper-global.yml"

// read returns a file's bytes. It stats first so an oversized file is refused
// BEFORE any of it is buffered — checking after the read would mean the memory
// blow-up this ceiling exists to prevent has already happened.
func read(r *os.Root, name string) Result {
	// path.Clean, not a raw compare: "./config/paper-global.yml",
	// "config//paper-global.yml" and "config/../config/paper-global.yml" all name
	// the same file, and a string equality test would wave every one of them
	// through. Cleaning collapses them to the single canonical form this matches.
	// Slash-based path (not filepath) is correct because the Job container is always
	// Linux, whatever the developer machine rendering the spec runs.
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
	if info.Size() > MaxReadBytes {
		return Result{Code: CodeTooLarge, Error: fmt.Sprintf(
			"%s is %d bytes; the editor reads at most %d", name, info.Size(), MaxReadBytes)}
	}

	// LimitReader is belt-and-braces against the file growing between the Stat and
	// the read: the ceiling then holds on the bytes actually buffered, not merely on
	// the size observed a moment earlier.
	b, err := io.ReadAll(io.LimitReader(f, MaxReadBytes))
	if err != nil {
		return failure(err, name)
	}
	return Result{Content: redactSecretProps(name, b), SHA256: digest(b)}
}

// propsPath is the server's main config file, and rconPasswordKey the one line in
// it the editor must not hand back (spec §286: RCON 密码绝不下发前端).
const (
	propsPath       = "server.properties"
	rconPasswordKey = "rcon.password"
	// redactedValue is deliberately not empty: a blank value would read as "RCON has
	// no password", which is a very different and much more alarming claim than "you
	// are not being shown it".
	redactedValue = "<redacted by felis>"
)

// redactSecretProps blanks the RCON password when server.properties is read.
//
// Unlike secretConfigPath this is a value redaction rather than a whole-file
// denial, because the file is not platform material that merely happens to sit in
// the volume — it is the single most-edited config a server owner has (MOTD,
// view-distance, difficulty, gamemode), and refusing it outright would cost real
// repair to hide one line. The password is also per-server and garbage-collected
// with it, so unlike the cluster-wide forwarding secret it leaks nothing about
// anyone else's server; it is withheld because §286 draws the line at the frontend
// regardless of blast radius, and because the console already gives an owner every
// capability the password would.
//
// The write path is left alone on purpose, mirroring the reasoning at
// secretConfigPath: felis-lobby's entrypoint rewrites all three rcon keys from the
// injected Secret on every boot, so saving the placeholder back cannot lock the
// control plane out — the next restart restores the real value. That is what makes
// redaction safe here; without the boot-time rewrite this would be a footgun.
func redactSecretProps(name string, content []byte) []byte {
	if path.Clean(name) != propsPath {
		return content
	}
	lines := bytes.Split(content, []byte("\n"))
	for i, line := range lines {
		// TrimSpace before matching: a properties key may be indented, and the
		// trailing \r of a CRLF file would otherwise ride along into the value.
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte(rconPasswordKey+"=")) {
			lines[i] = []byte(rconPasswordKey + "=" + redactedValue)
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// write replaces a file's contents. It does NOT create parent directories: every
// path this editor writes is an existing config file being corrected, so an
// unexpected mkdir would more likely be a typo materialising a stray directory in
// the world mount than an intent. A missing file is still created, so a config
// the server has not yet generated can be authored.
//
// The replacement is atomic. The bytes go to a temporary sibling that is synced
// and then renamed over the target, so a full disk, a Job killed at its deadline
// or a crashed node leaves either the old file or the new one — never the
// zero-length or half-written server.properties an in-place truncate would, which
// is a server that no longer boots. The sibling keeps the target's mode and is
// handed to the game uid before the rename, so the file the server finds is never
// root's. On failure it is removed; only a kill between create and rename leaves
// one behind, named ".<file>.felis-edit-<hex>" so no loader mistakes it for a
// plugin jar or a config.
//
// expect, when set, is the SHA-256 the caller read the file at (Result.SHA256 of
// its read). A file that has changed since — another manager saved it, or the
// server rewrote it on its last run — is refused with CodeConflict instead of
// being overwritten, which is how two people editing the same file find out.
// The world lock (internal/maintenance) already serialises writes, so the check
// and the rename cannot interleave with another write.
//
// os.Root applies the same containment to every step. A symlink at the target is
// followed only while it stays inside the root (resolveLink), so a planted link
// to a file outside is refused and the rename replaces the file the link names,
// never the link itself.
func write(r *os.Root, name string, content []byte, expect string) Result {
	if len(content) > MaxWriteBytes {
		// Defence in depth: felis-api already refuses an oversized write with a 413
		// before rendering the Job. Re-checking here keeps the ceiling true even if
		// this entrypoint is ever driven directly.
		return Result{Code: CodeTooLarge, Error: fmt.Sprintf(
			"content is %d bytes; the editor writes at most %d", len(content), MaxWriteBytes)}
	}
	target, res := resolveLink(r, name)
	if res.Code != "" {
		return res
	}

	mode := fs.FileMode(0o644)
	info, err := r.Lstat(target)
	switch {
	case err == nil && info.IsDir():
		return Result{Code: CodeBadPath, Error: fmt.Sprintf("%s is a directory, not a file", name)}
	case err == nil && !info.Mode().IsRegular():
		return Result{Code: CodeBadPath, Error: fmt.Sprintf("%s is not a regular file", name)}
	case err == nil:
		mode = info.Mode().Perm()
	case !errors.Is(err, fs.ErrNotExist):
		return failure(err, name)
	}

	if expect != "" {
		if res := checkUnchanged(r, name, target, expect); res.Code != "" {
			return res
		}
	}

	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return Result{Code: CodeBadPath, Error: fmt.Sprintf("generate a temporary name: %v", err)}
	}
	tmp := path.Join(path.Dir(target), "."+path.Base(target)+".felis-edit-"+hex.EncodeToString(suffix[:]))
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return writeFailure(err, name)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = r.Remove(tmp)
		}
	}()
	// Chmod explicitly: the create mode passed through the umask, and a file the
	// owner had at 0664 or 0600 should come back the same.
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return writeFailure(err, name)
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return writeFailure(err, name)
	}
	// Sync before the rename, or a crash could leave the new name pointing at
	// blocks that never reached the disk. Close is where a buffered-write error
	// surfaces, so its error is honoured rather than deferred-and-dropped.
	if err := syncWritten(f); err != nil {
		f.Close()
		return writeFailure(err, name)
	}
	if err := f.Close(); err != nil {
		return writeFailure(err, name)
	}
	// The Job runs as root, so the file it just created is root's. The server runs
	// as the game uid and could read it but never rewrite it — a config the panel
	// authored that Paper then fails to save. Best effort: the server's
	// prepare-data initContainer re-owns anything left behind on its next start.
	_ = ownWritten(r, tmp)
	if err := r.Rename(tmp, target); err != nil {
		return writeFailure(err, name)
	}
	renamed = true
	// The rename lives in the directory; sync it so the new entry survives a crash
	// too. Best effort: the content has landed and reporting failure would lie.
	if d, err := r.Open(path.Dir(target)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return Result{SHA256: digest(content)}
}

// writeFailure is failure for the steps that move bytes, where a full volume is
// the likely cause and deserves its own answer: the file is still whole, and the
// fix is to free space, not to change the path.
func writeFailure(err error, name string) Result {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return Result{Code: CodeNoSpace, Error: fmt.Sprintf(
			"the server's volume is full; %s was left unchanged", name)}
	}
	return failure(err, name)
}

// maxLinkHops bounds resolveLink, matching the kernel's own loop limit in spirit:
// a link cycle is a bad path, not a hang.
const maxLinkHops = 8

// resolveLink follows symlinks at the final component of name and returns the
// path of the file they lead to, relative to the root. An absolute link target is
// refused (os.Root would refuse it anyway); a relative one is resolved against
// the link's directory and must stay inside the root, which the next Lstat
// through os.Root enforces.
func resolveLink(r *os.Root, name string) (string, Result) {
	cur := name
	for range maxLinkHops {
		info, err := r.Lstat(cur)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			// Missing (a new file) or not a link: the path names itself. Any other
			// Lstat error surfaces from the caller's own Lstat of the same path.
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

// checkUnchanged compares the file's current digest with expect. A file larger
// than MaxReadBytes cannot be the one the caller read, so it is a conflict without
// hashing it.
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

// digest is the hex SHA-256 carried in Result.SHA256.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// syncWritten flushes the temporary sibling. A var so a test can fail it the way
// a full disk would.
var syncWritten = func(f *os.File) error { return f.Sync() }

// ownWritten hands a written file to the game uid. os.Root.Chown follows a symlink
// only within the root, so this can never re-own a file outside the mount. A var so
// tests, which cannot chown, can observe the call.
var ownWritten = func(r *os.Root, name string) error {
	return r.Chown(name, int(naming.GameUID), int(naming.GameGID))
}

// failure maps a filesystem error onto a caller-facing Result code. Anything that
// is genuinely "nothing is there" becomes not_found; EVERYTHING else — including
// every os.Root containment refusal — becomes bad_path.
//
// That default is deliberate. os.Root reports an escape as a *fs.PathError with no
// exported sentinel to match on, so the mapping cannot test for "escaped"
// positively; it tests for the one benign case it can name and refuses the rest.
// Failing closed this way means a future os.Root error kind is reported as a bad
// path rather than leaking through as a success.
//
// The error text is included because it is generated by the stdlib from the
// caller's OWN path inside their OWN world mount, so it discloses nothing they
// could not learn by listing — and it is the difference between a usable "no such
// file" and an opaque 400.
func failure(err error, path string) Result {
	if errors.Is(err, fs.ErrNotExist) {
		return Result{Code: CodeNotFound, Error: fmt.Sprintf("%s does not exist", path)}
	}
	return Result{Code: CodeBadPath, Error: err.Error()}
}

// Print writes r as the single marked stdout line the Job's reader looks for. The
// JSON is written with no indentation on purpose: the payload must occupy exactly
// ONE log line, because the reader identifies it by a line prefix. An indented
// encoding would split it across lines and make it unfindable.
func Print(w io.Writer, res Result) error {
	b, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s%s\n", ResultPrefix, b)
	return err
}
