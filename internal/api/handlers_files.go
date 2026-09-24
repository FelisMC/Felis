package api

import (
	"context"
	"errors"
	"net/http"

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
// It returns fileedit.ErrNotFound / ErrBadPath / ErrTooLarge for caller-fault
// failures, which writeFileEditError maps to 404 / 400 / 413.
type FileEditor interface {
	List(ctx context.Context, server, path string) (entries []fileedit.Entry, truncated bool, err error)
	Read(ctx context.Context, server, path string) ([]byte, error)
	Write(ctx context.Context, server, path string, content []byte) error
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
type writeFileRequest struct {
	Content *[]byte `json:"content"`
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
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"the ?path= query parameter is required"))
		return
	}

	content, err := a.Files.Read(r.Context(), name, path)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "content": content})
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
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"the ?path= query parameter is required"))
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

	// A write holds the world volume for its Job's lifetime (internal/maintenance);
	// reads and listings do not, since a read-only mount cannot hurt a server
	// starting beside it.
	release, ok := a.acquireWorld(w, r, name, maintenance.KindFileWrite, "stop the server before editing its files")
	if !ok {
		return
	}
	defer release()

	if err := a.Files.Write(r.Context(), name, path, *body.Content); err != nil {
		writeFileEditError(w, r, err)
		return
	}

	a.audit(r, "file.write", name+":"+path)
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "status": "written"})
}

// authorizeFileOp is the shared front half of all three file handlers — the gate
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
// Single-sourcing it is what keeps the three faces from drifting: a read path that
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

// writeFileEditError maps executor errors onto HTTP status codes. The three
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
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, r, newError(http.StatusGatewayTimeout, "files_timeout",
			"the file operation did not finish in time; retry shortly"))
	default:
		writeError(w, r, err)
	}
}
