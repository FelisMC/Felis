package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/naming"
)

// FileEditor is the server-file-editor surface the API depends on: list a
// directory, read a file, write a file, all inside one server's world volume. It
// is the repair lever for the case no other endpoint covers — a server that will
// not boot because one line of a config file is wrong.
//
// Like Restorer and Backuper it only describes the operation, never the mechanism:
// felis-api cannot touch a world in-process (the world PVC is ReadWriteOnce and
// owned by the operator's StatefulSet), so the production implementation hands off
// to a one-shot Job and reads the result back through pods/log — see
// internal/fileedit, which explains why that transport needs no RBAC felis-api
// does not already hold. Unlike Restorer and Backuper these calls are
// SYNCHRONOUS: the caller wants the listing or the bytes, so the handler blocks on
// the Job (seconds, dominated by Pod scheduling) rather than answering 202.
//
// It is an interface so the handlers are unit-tested against a fake; the
// production implementation is *fileedit.Editor. Using fileedit.Entry directly
// mirrors how ImageBuilder uses build.Request/build.Image rather than restating a
// parallel type on this side of the seam.
//
// It returns fileedit.ErrNotFound / ErrBadPath / ErrTooLarge / ErrConflict /
// ErrNoSpace / ErrExists, which writeFileEditError maps to 404 / 400 / 413 / 409 /
// 507 / 409.
//
// Read and Write return the file's SHA-256 (hex); Upload lands exactly the bytes
// src describes or fails. Write's expect is the
// hash a client read the file at; when set, a file that changed since is refused
// with ErrConflict instead of being overwritten. createOnly and a false overwrite
// refuse an existing path with ErrExists.
type FileEditor interface {
	List(ctx context.Context, server, path string) (entries []fileedit.Entry, truncated bool, err error)
	Read(ctx context.Context, server, path string) (content []byte, sha256 string, err error)
	Write(ctx context.Context, server, path string, content []byte, expect string, createOnly bool) (sha256 string, err error)
	Mkdir(ctx context.Context, server, path string) error
	Delete(ctx context.Context, server, path string) error
	Rename(ctx context.Context, server, path, to string) error
	Upload(ctx context.Context, server, path string, src fileedit.UploadSource, overwrite bool) error
}

// writeFileRequest is the PUT /servers/{name}/file body. Content is []byte, so
// encoding/json requires it to be base64 — which is what makes the write path
// binary-safe: a config file with CRLF line endings, a UTF-8 BOM, or a stray
// non-UTF-8 byte round-trips intact instead of being mangled by a string decode.
//
// It is a *[]byte, NOT a []byte, for the same reason permissionRequest.Value is a
// *bool: a plain slice makes "absent", "null", and "" indistinguishable, so a body
// of {} would decode to nil and TRUNCATE the target file to zero bytes while
// answering 200 — a client serialisation bug silently destroying the very config
// the caller opened this endpoint to repair. nil now means "the field was omitted"
// and is refused; an explicit "" is still a legitimate deliberate truncate.
//
// ExpectSHA256 is optional. The panel always sends the hash its read returned,
// so a save over a file someone else changed in the meantime answers 409
// file_changed; omitting it (a script, or "overwrite anyway") writes
// unconditionally. CreateOnly is the panel's "new file": the write lands only if
// nothing is at the path yet (409 file_exists otherwise), so it can never
// truncate a file the caller did not know was there. The two cannot be combined —
// one says the file exists, the other that it must not.
type writeFileRequest struct {
	Content      *[]byte `json:"content"`
	ExpectSHA256 string  `json:"expect_sha256,omitempty"`
	CreateOnly   bool    `json:"create_only,omitempty"`
}

// handleListFiles serves GET /api/v1/servers/{name}/files?path=… — one directory's
// entries inside the server's world volume. An absent or empty path lists the
// world root.
//
// The path travels as a QUERY parameter, not a path segment, for the same reason
// handleRemoveImage takes ?ref=: a file path contains '/' and does not round-trip
// through a single {placeholder}. It is passed to the executor unmodified — this
// handler deliberately performs NO path validation, because the only containment
// that can be trusted is the one applied at the moment of opening the file, inside
// the Job, by os.Root (see fileedit.Execute). A pre-validating handler would
// invite exactly the false confidence that makes string-prefix containment fail.
func (a *API) handleListFiles(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")

	entries, truncated, err := a.Files.List(r.Context(), name, path)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	if entries == nil {
		entries = []fileedit.Entry{} // an empty directory is [], never null
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": path, "entries": entries, "truncated": truncated,
	})
}

// handleReadFile serves GET /api/v1/servers/{name}/file?path=… — one file's bytes,
// base64-encoded by encoding/json's []byte handling. Reading is capped at
// fileedit.MaxReadBytes inside the Job; an oversized file is 413, not a truncated
// read, because a config editor that silently returned half a file would let a
// subsequent save destroy the other half.
func (a *API) handleReadFile(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path, ok := requirePath(w, r)
	if !ok {
		return
	}

	content, sum, err := a.Files.Read(r.Context(), name, path)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	if content == nil {
		content = []byte{} // an empty file is "", never null
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "content": content, "sha256": sum})
}

// handleWriteFile serves PUT /api/v1/servers/{name}/file?path=… — replace a file's
// contents, creating the file if absent (but never its parent directories).
//
// The size ceiling is enforced here, before the executor renders a Job, so an
// oversized write is a clean 413 rather than an opaque rejection from the API
// server when the Job spec breaches etcd's object limit. A body so large it also
// breaches the shared 1 MiB envelope cap is refused earlier still, by decodeJSON,
// as a 400 — the ceilings are layered, and the specific one answers first for
// every plausible input.
//
// A write is audited; the two read operations are not, matching how the codebase
// audits state changes (backup.create, image.admit) and not reads.
func (a *API) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path, ok := requirePath(w, r)
	if !ok {
		return
	}

	var body writeFileRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	// decodeJSON enforces only DisallowUnknownFields, which rejects a MISSPELLED
	// field but not an omitted one — so presence is checked here, exactly as the
	// required ?path= is checked above and as docs/openapi.yaml already declares.
	if body.Content == nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"the content field is required"))
		return
	}
	if len(*body.Content) > fileedit.MaxWriteBytes {
		writeError(w, r, newError(http.StatusRequestEntityTooLarge, "too_large",
			"file content is %d bytes; the editor writes at most %d",
			len(*body.Content), fileedit.MaxWriteBytes))
		return
	}
	// The hash rides the Job's argv, so only its one legitimate shape is let
	// through: 64 lowercase hex digits, exactly what a read returned.
	if body.ExpectSHA256 != "" && !sha256Hex.MatchString(body.ExpectSHA256) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"expect_sha256 must be the 64-digit lowercase hex sha256 a read returned"))
		return
	}
	if body.CreateOnly && body.ExpectSHA256 != "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"create_only and expect_sha256 cannot be combined"))
		return
	}

	// A write holds the world volume for its Job's lifetime (internal/maintenance);
	// reads and listings do not, since a read-only mount cannot hurt a server
	// starting beside it.
	release, ok := a.acquireWorld(w, r, name, maintenance.KindFileWrite, "stop the server before editing its files")
	if !ok {
		return
	}
	defer release()

	sum, err := a.Files.Write(r.Context(), name, path, *body.Content, body.ExpectSHA256, body.CreateOnly)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}

	a.auditFile(r, "file.write", name, path, nil)
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "status": "written", "sha256": sum})
}

// requirePath reads the required ?path= query parameter, answering 400 when it
// is absent.
func requirePath(w http.ResponseWriter, r *http.Request) (string, bool) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"the ?path= query parameter is required"))
		return "", false
	}
	return path, true
}

// handleMkdir serves POST /api/v1/servers/{name}/files/mkdir?path=… — make one
// folder. Its parent must exist (404 otherwise) and nothing may be at the path yet
// (409 file_exists). Like every file change it holds the world lock and is
// audited.
func (a *API) handleMkdir(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path, ok := requirePath(w, r)
	if !ok {
		return
	}
	release, ok := a.acquireWorld(w, r, name, maintenance.KindFileWrite, "stop the server before editing its files")
	if !ok {
		return
	}
	defer release()

	if err := a.Files.Mkdir(r.Context(), name, path); err != nil {
		writeFileEditError(w, r, err)
		return
	}
	a.auditFile(r, "file.mkdir", name, path, nil)
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "status": "created"})
}

// handleDeleteFile serves DELETE /api/v1/servers/{name}/file?path=… — delete a
// file, a symlink (never what it points at), or a folder with everything in it.
// The world root itself is refused (400 bad_path). The panel confirms first; this
// route does not, because a script that says DELETE means it.
func (a *API) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path, ok := requirePath(w, r)
	if !ok {
		return
	}
	release, ok := a.acquireWorld(w, r, name, maintenance.KindFileWrite, "stop the server before editing its files")
	if !ok {
		return
	}
	defer release()

	if err := a.Files.Delete(r.Context(), name, path); err != nil {
		writeFileEditError(w, r, err)
		return
	}
	a.auditFile(r, "file.delete", name, path, nil)
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "status": "deleted"})
}

// renameFileRequest is the POST /servers/{name}/files/rename body: the new path,
// relative to the world root like ?path=.
type renameFileRequest struct {
	To string `json:"to"`
}

// handleRenameFile serves POST /api/v1/servers/{name}/files/rename?path=… — move
// a file or folder to body.to. It never replaces: an existing destination is 409
// file_exists. server.properties, config/paper-global.yml and config/ cannot be
// moved (400 bad_path), since under another name the read path would no longer
// know to withhold their secrets.
func (a *API) handleRenameFile(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path, ok := requirePath(w, r)
	if !ok {
		return
	}
	var body renameFileRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.To == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "the to field is required"))
		return
	}
	release, ok := a.acquireWorld(w, r, name, maintenance.KindFileWrite, "stop the server before editing its files")
	if !ok {
		return
	}
	defer release()

	if err := a.Files.Rename(r.Context(), name, path, body.To); err != nil {
		writeFileEditError(w, r, err)
		return
	}
	a.auditFile(r, "file.rename", name, path, map[string]any{"to": body.To})
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "to": body.To, "status": "renamed"})
}

// handleUploadFile serves PUT /api/v1/servers/{name}/files/upload?path=… — land
// the raw request body as a file, up to fileedit.MaxUploadBytes. An existing file
// is 409 file_exists unless ?overwrite=true.
//
// The body is staged on felis-api's disk first (fileedit.Stage) and fetched from
// there by the Job, on the internal face, with a one-time token: it fits in
// neither a Job spec nor an environment. The world lock is taken only once the
// body has arrived, so a slow upload does not hold off a backup; the stopped gate
// ran before the body was read and the lock re-checks that nothing started since.
//
// Content-Length is required (411 length_required): the stage reserves room for
// the declared size before a byte is written, and a size promised up front is
// what lets a short body be told from a whole one.
func (a *API) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path, ok := requirePath(w, r)
	if !ok {
		return
	}
	if a.FileStage == nil || a.InternalBaseURL == "" {
		writeError(w, r, newError(http.StatusServiceUnavailable, "files_unavailable",
			"uploads are not configured"))
		return
	}
	if r.ContentLength < 0 {
		writeError(w, r, newError(http.StatusLengthRequired, "length_required",
			"an upload needs a Content-Length"))
		return
	}
	if r.ContentLength > fileedit.MaxUploadBytes {
		writeError(w, r, newError(http.StatusRequestEntityTooLarge, "too_large",
			"the file is %d bytes; uploads are at most %d", r.ContentLength, fileedit.MaxUploadBytes))
		return
	}
	overwrite := r.URL.Query().Get("overwrite") == "true"

	staged, drop, err := a.FileStage.Put(r.Body, r.ContentLength)
	switch {
	case errors.Is(err, fileedit.ErrStageFull):
		writeError(w, r, newError(http.StatusInsufficientStorage, "upload_staging_full",
			"felis has no room to take this upload right now; try again later or ask an admin"))
		return
	case errors.Is(err, fileedit.ErrShortUpload):
		writeError(w, r, newError(http.StatusBadRequest, "upload_incomplete",
			"the upload ended before all %d bytes arrived", r.ContentLength))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	defer drop()

	release, ok := a.acquireWorld(w, r, name, maintenance.KindFileWrite, "stop the server before editing its files")
	if !ok {
		return
	}
	defer release()

	err = a.Files.Upload(r.Context(), name, path, fileedit.UploadSource{
		URL:    a.InternalBaseURL + "/api/v1/internal/file-uploads/" + staged.ID,
		Token:  staged.Token,
		Size:   staged.Size,
		SHA256: staged.SHA256,
	}, overwrite)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	a.auditFile(r, "file.upload", name, path, map[string]any{
		"size_bytes": staged.Size, "sha256": staged.SHA256, "overwrite": overwrite,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"path": path, "status": "uploaded", "sha256": staged.SHA256, "size": staged.Size,
	})
}

// handleInternalFileUpload serves GET /api/v1/internal/file-uploads/{id} — the
// staged bytes of one upload, to the one Job created to land them. It is Public on
// the internal face: the Job holds no service token (it holds no credential at
// all), so the bearer token minted with the upload is the whole check, and it
// opens that upload once. An unknown id, a wrong token and a spent one are the
// same 404, so the route answers nothing about which uploads exist.
func (a *API) handleInternalFileUpload(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if a.FileStage == nil || !ok {
		writeError(w, r, newError(http.StatusNotFound, "not_found", "no such upload"))
		return
	}
	f, size, err := a.FileStage.Open(r.PathValue("id"), token)
	if errors.Is(err, fileedit.ErrNotStaged) {
		writeError(w, r, newError(http.StatusNotFound, "not_found", "no such upload"))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// auditFile records a file change. The target is "<server>:<path>", as file.write
// has always recorded it; extra, when set, is the payload.
func (a *API) auditFile(r *http.Request, action, server, path string, extra map[string]any) {
	p := principalFromContext(r.Context())
	e := AuditEntry{Actor: auditActor(p), Action: action, ServerName: server + ":" + path}
	if p != nil {
		e.ActorUserID = p.UserID
	}
	if extra != nil {
		e.Payload = auditPayload(extra)
	}
	a.auditEntry(r, e)
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// authorizeFileOp is the shared front half of every file handler — the gate
// that decides whether this caller may touch this server's world at all. It
// mirrors the backup/restore gate step for step, because it is guarding the same
// resource under the same physical constraint:
//
//	① name validation — 400
//	② ServerByName — an unknown server is 404
//	③ owner-or-admin, else 403. An unowned (released) server fails for everyone
//	   but admin, which is the same "must re-claim first" rule restore enforces
//	④ stopped gate: the world PVC is RWO and held by a running server, so a file
//	   Job cannot mount it — refuse unless the server is fully stopped. Ready means
//	   it is up; any desiredState other than Stopped means it is up or coming up
//	   and still owns the volume. This yields a specific 409 instead of a Job that
//	   silently fails to mount
//	⑤ the FileEditor must be wired, else 503
//
// Single-sourcing it is what keeps the handlers from drifting: a read path that
// forgot the stopped gate would not merely fail, it would hang waiting for a Pod
// that can never be scheduled.
//
// It returns the validated server name and false if it has already written a
// response.
func (a *API) authorizeFileOp(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return "", false
	}

	p := principalFromContext(r.Context())
	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return "", false
	}
	if !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return "", false
	}

	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return "", false
	}
	if info.Ready || info.DesiredState != string(v1alpha1.DesiredStopped) {
		writeError(w, r, newError(http.StatusConflict, "not_stopped",
			"stop the server before editing its files"))
		return "", false
	}

	// World-volume gate, matching the backup/restore faces: the Job mounts the
	// world PVC by claim name, so a server that has never started (or was already
	// reaped) has no claim to mount and its Pod sits Pending until the executor's
	// wait expires — a knowably impossible request answered by a 90s hang and a
	// misleading 504. Refuse up front with the same specific 409.
	if exists, err := a.Cluster.WorldVolumeExists(r.Context(), name); err != nil {
		writeError(w, r, err)
		return "", false
	} else if !exists {
		writeError(w, r, errNoWorldVolume())
		return "", false
	}

	// Files is optional: when unwired the endpoints report 503 rather than
	// panicking, so the authorization boundary above is exercised even before the
	// file-Job executor is wired (see FileEditor).
	if a.Files == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "files_unavailable",
			"the file editor is not configured"))
		return "", false
	}
	return name, true
}

// writeFileEditError maps executor errors onto HTTP status codes. The
// sentinels are caller-fault and get precise answers; a timeout is reported as 504
// so the caller knows to retry rather than believing the edit was rejected; and
// anything else collapses to a 500 by writeError, so no cluster detail leaks.
//
// ErrBadPath is 400 rather than 403 on purpose: a path that escapes the world root
// is a malformed request, not a permission the caller might be granted. Answering
// 403 would imply some caller somewhere may read /etc/passwd through this endpoint,
// and none may.
func writeFileEditError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, fileedit.ErrNotFound):
		writeError(w, r, newError(http.StatusNotFound, "not_found", "%s", err.Error()))
	case errors.Is(err, fileedit.ErrBadPath):
		writeError(w, r, newError(http.StatusBadRequest, "bad_path", "%s", err.Error()))
	case errors.Is(err, fileedit.ErrTooLarge):
		writeError(w, r, newError(http.StatusRequestEntityTooLarge, "too_large", "%s", err.Error()))
	case errors.Is(err, fileedit.ErrConflict):
		writeError(w, r, newError(http.StatusConflict, "file_changed", "%s", err.Error()))
	case errors.Is(err, fileedit.ErrNoSpace):
		writeError(w, r, newError(http.StatusInsufficientStorage, "volume_full", "%s", err.Error()))
	case errors.Is(err, fileedit.ErrExists):
		writeError(w, r, newError(http.StatusConflict, "file_exists", "%s", err.Error()))
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, r, newError(http.StatusGatewayTimeout, "files_timeout",
			"the file operation did not finish in time; retry shortly"))
	default:
		writeError(w, r, err)
	}
}
