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

// The operations the editor supports: list a directory, read a file, write a
// file, make a directory, delete, rename, upload, and extract a .zip. The set is
// closed; there is no chmod, chown, link or copy. Every op resolves every path
// through os.Root (see Execute), and each mutating op carries its own
// containment note below.
//
// A write, upload or unzip DOES land arbitrary bytes at any path inside the mount, and
// that is a real capability rather than an oversight: the root is the server's
// whole working directory (see Config.WorldsRoot), so an owner can upload
// plugins/<x>.jar and Paper will load it on the next boot. It is the same power a
// hosting panel's file manager gives, scoped to a server the caller already owns
// and already controls through /command. Note what it is NOT scoped by: admin
// image curation. Images are admin-only (POST /images, POST /images/build) and
// modpack submissions need an admin verdict, so this is the one owner-tier route
// that lands executable code in a backend pod. That trade was made deliberately;
// if it is ever revisited, the guard belongs in land() below, the choke point
// write and upload route through, and in unzip's extractOne (unzip.go).
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

// mutates reports whether op changes the world, and so whether its Job gets the
// world mount read-write. Only list and read leave the world alone; anything else
// counts as a change. maintenance.JobKind draws the same line for the world-volume
// lock.
func mutates(op string) bool { return op != OpList && op != OpRead }

// validOp reports whether op is one the Job knows.
func validOp(op string) bool {
	switch op {
	case OpList, OpRead, OpWrite, OpMkdir, OpDelete, OpRename, OpUpload, OpUnzip:
		return true
	}
	return false
}

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
	// CodeExists is a create, mkdir, rename or upload whose target is already
	// there. None of them replaces anything unless told to (an upload's
	// Overwrite), so a name collision is reported rather than resolved.
	CodeExists = "exists"
	// CodeDigestMismatch is a write whose bytes do not hash to the SHA-256
	// felis-api computed over them (Request.ContentSHA256): they changed on the
	// way to the Job, and nothing was written.
	CodeDigestMismatch = "digest_mismatch"
)

// ResultPrefix marks the single stdout line carrying the JSON Result. The Job's
// output is read back through the pods/log subresource, which returns the
// container's stdout and stderr MERGED — so a Go runtime warning, a libc message,
// or anything else the container writes to stderr lands in the same stream. The
// marker is what makes the payload findable in that mixed stream: felis-api scans
// for the last line carrying this prefix rather than assuming the log is pure
// JSON. Without it any stray stderr byte would corrupt every response.
const ResultPrefix = "FELIS-FILES-RESULT: "

// ContentEnv names the environment variables the write path carries new file
// content in (base64). It travels on the Job spec felis-api creates, because
// felis-api holds `jobs: create` but NOT `secrets: create` in the minecraft
// namespace (internal/platform.APIMinecraftRole) — a Secret is not available to
// it, so the Job spec is the only channel into the Pod. The consequence is that
// written content is readable by anyone holding jobs:get in the minecraft
// namespace, which is a cluster-admin-level power; it is NOT readable by
// felis-operator, felis-reaper, or any weak Job SA, none of which hold that verb.
//
// The base64 is split across ContentEnv_0 … ContentEnv_<n-1>, with n in
// ContentPartsEnv. One variable cannot carry it: execve refuses any single
// environment string longer than MAX_ARG_STRLEN (32 pages, 128 KiB with 4 KiB
// pages), so a container whose one variable held the base64 of a 100 KiB file
// never started — the Pod failed with exit 255 before felis ran, and the save
// came back as an opaque 500. contentChunk keeps every part well under that.
const (
	ContentEnv      = "FELIS_FILE_CONTENT"
	ContentPartsEnv = ContentEnv + "_PARTS"
	contentChunk    = 64 << 10
)

// UploadTokenEnv carries the one-time token an upload Job presents to felis-api
// to fetch the bytes it lands (see Stage). Like the content it rides the Job
// spec, and it opens exactly one thing — the one upload that Job was created
// for, once.
const UploadTokenEnv = "FELIS_UPLOAD_TOKEN"

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
//   - MaxUploadBytes bounds an upload sent as ONE request body: the Cloudflare
//     edge refuses bodies over 100 MB on the Free and Pro plans, and 64 MiB
//     covers the largest plugin jars (a Geyser build is about 20 MiB) with room
//     to spare. It is felis-api's bound on that route only. A bigger file arrives
//     in parts and is bounded by nothing but the room on the server's volume,
//     which the Job checks before it fetches a byte (upload).
const (
	MaxWriteBytes  = 256 << 10 // 256 KiB
	MaxReadBytes   = 1 << 20   // 1 MiB
	MaxEntries     = 2000
	MaxUploadBytes = 64 << 20 // 64 MiB
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
// shape covers every op so the transport has exactly one thing to find and
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
	// conflict, the file as it is now. A client hands it back as the expected
	// hash of its next write (see write).
	SHA256 string `json:"sha256,omitempty"`
	// ContentSHA256 is, after a read, the hex digest of Content as handed out
	// (after any redaction), so felis-api can tell the bytes it got from the
	// bytes this Job sent (Editor.Read).
	ContentSHA256 string `json:"content_sha256,omitempty"`

	// Conflicts lists, relative to the root and sorted, the existing files an
	// unzip would replace: the first of them, up to MaxConflicts and 8 KiB of
	// names (see maxConflictBytes). ConflictCount is how many there are in all.
	Conflicts     []string `json:"conflicts,omitempty"`
	ConflictCount int      `json:"conflict_count,omitempty"`
	// Entry names what an unzip refused: the archive entry, or the path on the
	// server it collides with.
	Entry string `json:"entry,omitempty"`
	// Need and Avail are, on a no_space an upload or unzip saw coming, the bytes
	// it needs and the bytes the volume has free. A listing sets Avail too, to
	// -1 when it could not read it.
	Need  int64 `json:"need,omitempty"`
	Avail int64 `json:"avail,omitempty"`
	// Files and Bytes are what a successful unzip extracted.
	Files int   `json:"files,omitempty"`
	Bytes int64 `json:"bytes,omitempty"`
}

// Request is one file operation. Op decides which of the other fields it reads.
type Request struct {
	Op   string
	Path string
	// To is a rename's destination.
	To string
	// Content and Expect are a write's bytes and precondition: when Expect is
	// non-empty, the write lands only if the file's current SHA-256 (hex) equals
	// it. ContentSHA256, when set, is the SHA-256 (hex) felis-api computed over
	// Content; bytes that hash otherwise are not written (CodeDigestMismatch).
	Content       []byte
	Expect        string
	ContentSHA256 string
	// CreateOnly makes a write refuse a path that already exists. It is the
	// panel's "new file", which must never truncate a file it did not know was
	// there.
	CreateOnly bool
	// Upload is where an upload's bytes come from. Overwrite lets an upload
	// replace a file already at the path, and an unzip replace the files it
	// collides with.
	Upload    *Upload
	Overwrite bool
	// Progress, when set, hears how far an upload or unzip has got: bytes landed
	// so far out of the total. It is called from the copy loop, often; the caller
	// throttles.
	Progress func(done, total int64)
}

// Upload describes the bytes an upload lands. Size and SHA256 are what felis-api
// received from the caller; the fetched bytes must match both before they replace
// anything.
type Upload struct {
	Size   int64
	SHA256 string
	// Open starts the transfer. It runs only once the target has passed every
	// check, so a refused upload never pulls the bytes.
	Open func() (io.ReadCloser, error)
	// Landed, when set, runs once the bytes are in place, so felis-api can let
	// go of the copy it staged (Stage.Landed). Until then felis-api keeps it,
	// and a failed landing is started again without the bytes being sent again.
	Landed func()
}

// Execute performs one operation inside root and returns the Result to print.
// root is the in-Pod mount path of the server's world PVC; every path in req is
// relative to it.
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
// editor must not have. mkdir, delete and rename do path.Clean the path first (so
// a trailing slash cannot make them act on a link's target), and Clean keeps both
// a leading "/" and a leading "..", so an escape stays an escape.
//
// The error is an infrastructure failure: the world mount unopenable, an unknown
// op, or an upload whose transfer broke. A caller's mistake is a Result with a
// Code.
func Execute(root string, req Request) (Result, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		// The world mount itself is unopenable: infrastructure, not caller fault.
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

// list reads one directory. It does not recurse: a browser asks for one level at
// a time, and recursion would make both the result size and the traversal cost
// unbounded in a world directory. It also reports the room left on the volume
// (Avail), so the panel can refuse an upload the volume cannot take before
// sending a byte of it.
func list(r *os.Root, rootPath, path string) Result {
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
	// A full volume is Avail 0, which the JSON leaves out; -1 is a volume whose
	// free space could not be read, so the two stay apart.
	res := Result{Entries: entries, Truncated: truncated, Avail: -1}
	if avail, _, err := statfs(rootPath); err == nil {
		res.Avail = int64(min(avail, math.MaxInt64))
	}
	return res
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
	// The name check above answers the plain path with a clear reason; this one
	// catches the same file reached through a link (see Guard).
	withhold, redact := NewGuard(r).Rule(info)
	if withhold {
		return Result{Code: CodeBadPath, Error: fmt.Sprintf(
			"%s is the file holding the proxy forwarding secret, which is shared cluster-wide, and is not readable through the editor", name)}
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
	content := b
	if redact {
		content = RedactProps(b)
	}
	return Result{Content: content, SHA256: digest(b), ContentSHA256: digest(content)}
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

// redactSecretProps hides RCON and Limbo forwarding secrets when server.properties is read.
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
// Limbo stores its cluster forwarding key in this file too; that value is
// withheld by the same redaction and refreshed by the Limbo entrypoint.
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
		for _, key := range []string{rconPasswordKey, "forwarding-secrets"} {
			if bytes.HasPrefix(bytes.TrimSpace(line), []byte(key+"=")) {
				lines[i] = []byte(key + "=" + redactedValue)
			}
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// write replaces a file's contents. It does NOT create parent directories: every
// path this editor writes is an existing config file being corrected, so an
// unexpected mkdir would more likely be a typo materialising a stray directory in
// the world mount than an intent. A missing file is still created, so a config
// the server has not yet generated can be authored; createOnly refuses a path that
// already exists, which is how the panel's "new file" avoids truncating a file it
// did not know was there.
//
// expect, when set, is the SHA-256 the caller read the file at (Result.SHA256 of
// its read). A file that has changed since — another manager saved it, or the
// server rewrote it on its last run — is refused with CodeConflict instead of
// being overwritten, which is how two people editing the same file find out.
// The world lock (internal/maintenance) already serialises writes, so the check
// and the rename cannot interleave with another write.
//
// sum, when set, is the SHA-256 felis-api computed over content before handing
// it to the Job: content that hashes otherwise changed on the way, and is
// refused with CodeDigestMismatch before anything is touched.
func write(r *os.Root, name string, content []byte, sum, expect string, createOnly bool) Result {
	if sum != "" {
		if got := digest(content); got != sum {
			return Result{Code: CodeDigestMismatch, Error: fmt.Sprintf(
				"the content hashes to %s and was sent as %s; nothing was written", got, sum)}
		}
	}
	if len(content) > MaxWriteBytes {
		// Defence in depth: felis-api already refuses an oversized write with a 413
		// before rendering the Job. Re-checking here keeps the ceiling true even if
		// this entrypoint is ever driven directly.
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
	// A write's fill never returns a transfer error, so land's error is always nil.
	res, _ = land(r, name, target, mode, func(w io.Writer) error {
		_, err := w.Write(content)
		return err
	})
	if res.Code == "" {
		res.SHA256 = digest(content)
	}
	return res
}

// landingTarget decides where a write or upload lands and with what mode. A
// symlink at name is followed only while it stays inside the root (resolveLink), so
// a planted link to a file outside is refused and the rename in land replaces the
// file the link names, never the link itself. The target keeps its mode; a new file
// gets 0644.
//
// mayExist false refuses a name that is already there in any form — file,
// directory or link, dangling or not — before any link is followed.
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

// transferError marks an upload whose bytes could not be fetched intact. It is
// infrastructure — felis-api staged the bytes and serves them to this Job — so it
// leaves Execute as an error (a non-zero exit, a 500) rather than a caller-facing
// code.
type transferError struct{ err error }

func (e *transferError) Error() string { return "fetch upload: " + e.err.Error() }
func (e *transferError) Unwrap() error { return e.err }

// NameMax is the longest name a folder entry can have on the volumes a world
// lives on (NAME_MAX).
const NameMax = 255

// land atomically puts the bytes fill writes at target, the path landingTarget
// returned for name. Write and upload both land through it; unzip lands a whole
// tree at once and has its own path (unzip.go).
//
// The bytes go to a temporary sibling that is synced and then renamed over the
// target, so a full disk, a Job killed at its deadline or a crashed node leaves
// either the old file or the new one — never the zero-length or half-written
// server.properties an in-place truncate would, which is a server that no longer
// boots. The sibling gets mode and is handed to the game uid before the rename, so
// the file the server finds is never root's. On failure it is removed; only a kill
// between create and rename leaves one behind, named ".felis-edit-<hex>" so no
// loader mistakes it for a plugin jar or a config. The name is its own rather than
// the target's with a suffix, so a target named up to NameMax bytes can be written.
//
// A *transferError from fill comes back as the error; every other failure is a
// Result.
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
	// Chmod explicitly: the create mode passed through the umask, and a file the
	// owner had at 0664 or 0600 should come back the same.
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
	// Sync before the rename, or a crash could leave the new name pointing at
	// blocks that never reached the disk. Close is where a buffered-write error
	// surfaces, so its error is honoured rather than deferred-and-dropped.
	if err := syncWritten(f); err != nil {
		f.Close()
		return writeFailure(err, name), nil
	}
	if err := f.Close(); err != nil {
		return writeFailure(err, name), nil
	}
	// The Job runs as root, so the file it just created is root's. The server runs
	// as the game uid and could read it but never rewrite it — a config the panel
	// authored that Paper then fails to save. Best effort: the server's
	// prepare-data initContainer re-owns anything left behind on its next start.
	_ = ownWritten(r, tmp)
	if err := r.Rename(tmp, target); err != nil {
		return writeFailure(err, name), nil
	}
	renamed = true
	syncDir(r, path.Dir(target))
	return Result{}, nil
}

// syncDir syncs a directory so an entry just added, renamed or removed survives a
// crash too. Best effort: the change has happened and reporting failure would lie.
func syncDir(r *os.Root, dir string) {
	if d, err := r.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// upload lands a file fetched from felis-api (see Stage). Everything that can
// refuse it is checked before u.Open, so a refused upload never pulls its bytes.
// The fetched bytes must match both the size and the SHA-256 felis-api received;
// either mismatch is a broken transfer, and the target is left as it was. A
// success has landed exactly what felis-api staged, whose digest it already holds.
//
// There is no size ceiling here. The volume is the bound, and it is checked up
// front: the new bytes land beside the file they replace until the rename, so
// they need their whole size free whatever is already at the path.
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
		// One byte past Size so a source that sends more than it promised is seen.
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

// sourceReader tags the source's read errors as transfer errors, so land can tell
// a broken fetch from the volume filling up underneath the copy.
type sourceReader struct{ r io.Reader }

func (s sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		err = &transferError{err}
	}
	return n, err
}

// mkdir makes one directory, handed to the game uid. It does not make parents:
// the panel creates a folder inside the one it is showing, so a missing parent is
// a stale view, reported as such.
//
// The path is cleaned first so a trailing slash cannot slip past the "." check.
// Clean keeps a leading "/" or "..", so os.Root still sees — and refuses — an
// escape.
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

// remove deletes a file, a link or a whole directory. RemoveAll removes a symlink
// itself, never what it points at, and os.Root keeps it inside the mount; the Lstat
// is there because RemoveAll reports nothing for a path that is not there. The
// root itself is refused — emptying a server's whole volume is a reset, which has
// its own path.
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

// rename moves from to to, both inside the root. It never replaces: a destination
// that exists is CodeExists, so a mistyped name cannot silently destroy another
// file. A missing destination folder is not made.
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

// guardedPaths are the paths read guards by name: secretConfigPath is refused and
// propsPath has its RCON password redacted. Moving either — or the config folder
// holding the first — to another name would make the next read hand back what the
// guard withholds, so rename refuses them.
var guardedPaths = []string{secretConfigPath, path.Dir(secretConfigPath), propsPath}

// guardMove refuses a rename whose source is a guarded path under any name: the
// comparison is by file identity, so "./config", a link's target or a folder
// reached through a link are all caught.
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
