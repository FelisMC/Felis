package api

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/maintenance"
)

// A file too big for the one-request upload (handleUploadFile) arrives as an
// upload session instead: POST …/files/uploads begins one for a path and a
// size, PUT …/files/uploads/{id}?offset= sends it in parts of at most
// fileedit.PartBytes (each part fits the Cloudflare edge's body limit), and POST
// …/files/uploads/{id}/commit lands it. The session lives on felis-api's staging
// disk (fileedit.Stage, session.go), bound to the account and the server it was
// begun for; the room for the whole file is reserved when it begins, so there is
// no product ceiling on the size, only the disk.
//
// Landing a file that size, like unzipping an archive, can outlast a request,
// so both answer 202 with the op, and GET …/files/ops reports how far it has got
// and how it ended (fileedit.Editor.StartUpload, StartUnzip, Ops). The Job holds
// the world volume while it runs, as any file write does, and the server cannot
// start until it ends.

// fileSessionView is where an upload session stands. Parts are the parts it
// took, in order, each with the SHA-256 it arrived with: a client resuming from
// a file on disk checks the file still holds those bytes before it sends the
// rest.
type fileSessionView struct {
	ID           string         `json:"id"`
	Path         string         `json:"path"`
	Size         int64          `json:"size"`
	Received     int64          `json:"received"`
	PartMaxBytes int64          `json:"part_max_bytes"`
	Parts        []filePartView `json:"parts"`
}

// filePartView is one part a session took.
type filePartView struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func sessionView(s fileedit.Session) fileSessionView {
	parts := make([]filePartView, 0, len(s.Parts))
	for _, p := range s.Parts {
		parts = append(parts, filePartView{Size: p.Size, SHA256: p.SHA256})
	}
	return fileSessionView{
		ID: s.ID, Path: s.Path, Size: s.Size, Received: s.Received,
		PartMaxBytes: fileedit.PartBytes, Parts: parts,
	}
}

// namesFit reports whether every name in path fits one folder entry.
func namesFit(path string) bool {
	for _, name := range strings.Split(path, "/") {
		if len(name) > fileedit.NameMax {
			return false
		}
	}
	return true
}

// errNameTooLong refuses a path namesFit rejects, before any byte is taken:
// the Job would only find out once it tried to create the file.
func errNameTooLong() *apiError {
	return newError(http.StatusBadRequest, "bad_path", "a name in the path is longer than %d bytes", fileedit.NameMax)
}

// beginFileUploadRequest is the POST …/files/uploads body.
type beginFileUploadRequest struct {
	Size *int64 `json:"size"`
}

// handleBeginFileUpload serves POST /api/v1/servers/{name}/files/uploads?path=…
// — begin an upload session for a file of body.size bytes that will land at
// path. The gate is the file manager's, so a server that is running is refused
// before any byte is sent; the parts that follow need only the account and the
// server, so starting the server midway costs the upload nothing but the
// commit's refusal until it is stopped again.
func (a *API) handleBeginFileUpload(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	user, ok := a.requireFileStage(w, r)
	if !ok {
		return
	}
	path, ok := requirePath(w, r)
	if !ok {
		return
	}
	// The Job checks the path against the volume when the file lands, hours of
	// upload later for a big one; one that could never land is refused now.
	if !filepath.IsLocal(path) || filepath.Clean(path) == "." {
		writeError(w, r, newError(http.StatusBadRequest, "bad_path",
			"the path must name a file inside the world folder"))
		return
	}
	if !namesFit(path) {
		writeError(w, r, errNameTooLong())
		return
	}
	var body beginFileUploadRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Size == nil || *body.Size < 0 {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"size must be the file's length in bytes"))
		return
	}
	s, err := a.FileStage.Begin(user, name, path, *body.Size)
	if err != nil {
		writeFileSessionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sessionView(s))
}

// handleFileUploadStatus serves GET /api/v1/servers/{name}/files/uploads/{id}
// — where the caller's session stands, so a client that lost a part's answer
// resumes from received.
func (a *API) handleFileUploadStatus(w http.ResponseWriter, r *http.Request) {
	name, user, ok := a.authorizeFileSession(w, r)
	if !ok {
		return
	}
	s, err := a.FileStage.Status(user, name, r.PathValue("id"))
	if err != nil {
		writeFileSessionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionView(s))
}

// handleFileUploadPart serves PUT
// /api/v1/servers/{name}/files/uploads/{id}?offset=… — append the raw body to
// the caller's session. offset must be where the session ends (409
// upload_offset_mismatch otherwise; the status says where), and Content-Length
// and Content-Digest are required, as for the one-request upload: the part is
// taken whole or not at all, and a part that breaks midway, or whose bytes do
// not hash to its digest (400 digest_mismatch), leaves the session where it
// was. A part over fileedit.PartBytes is refused before a byte of it is read
// (Append).
func (a *API) handleFileUploadPart(w http.ResponseWriter, r *http.Request) {
	name, user, ok := a.authorizeFileSession(w, r)
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"offset must be the byte position the part starts at"))
		return
	}
	if r.ContentLength < 0 {
		writeError(w, r, newError(http.StatusLengthRequired, "length_required",
			"a part needs a Content-Length"))
		return
	}
	want, ok := contentDigest(w, r)
	if !ok {
		return
	}
	s, err := a.FileStage.Append(user, name, r.PathValue("id"), offset, r.Body, r.ContentLength, want)
	if err != nil {
		writeFileSessionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionView(s))
}

// handleDropFileUpload serves DELETE /api/v1/servers/{name}/files/uploads/{id}
// — cancel the caller's session and free the room it holds.
func (a *API) handleDropFileUpload(w http.ResponseWriter, r *http.Request) {
	name, user, ok := a.authorizeFileSession(w, r)
	if !ok {
		return
	}
	if err := a.FileStage.Drop(user, name, r.PathValue("id")); err != nil {
		writeFileSessionError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// startFileOpRequest is the body of a commit or an unzip.
type startFileOpRequest struct {
	Overwrite bool `json:"overwrite"`
}

// handleCommitFileUpload serves POST
// /api/v1/servers/{name}/files/uploads/{id}/commit — land the caller's
// finished session at its path, replacing a file there only with
// body.overwrite (the op ends file_exists otherwise). It answers 202 with the
// op; GET …/files/ops reports how it ends.
//
// The world lock is taken BEFORE the session is sealed: a commit made while an
// earlier commit's Job is still fetching the file is refused by that Job's hold
// on the volume, so it never mints the fresh token that would lock the running
// Job out. The session outlives a Job that fails for any reason, so such a
// commit is simply made again; the Job whose file landed deletes it
// (handleInternalFileUploadLanded).
func (a *API) handleCommitFileUpload(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	user, ok := a.requireFileStage(w, r)
	if !ok {
		return
	}
	var body startFileOpRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	s, err := a.FileStage.Status(user, name, id)
	// Refused before the world lock is asked for; Seal checks again under the
	// stage's own lock, for a part that arrives in between.
	if err == nil && s.Received != s.Size {
		err = fmt.Errorf("%w: %d of %d bytes are here", fileedit.ErrUploadIncomplete, s.Received, s.Size)
	}
	if err != nil {
		writeFileSessionError(w, r, err)
		return
	}
	release, ok := a.acquireWorld(w, r, name, maintenance.KindFileWrite, "stop the server before editing its files")
	if !ok {
		return
	}
	defer release()
	staged, err := a.FileStage.Seal(user, name, id)
	if err != nil {
		writeFileSessionError(w, r, err)
		return
	}
	op, err := a.Files.StartUpload(r.Context(), name, s.Path, fileedit.UploadSource{
		URL:    a.InternalBaseURL + "/api/v1/internal/file-uploads/" + id,
		Token:  staged.Token,
		Size:   staged.Size,
		SHA256: staged.SHA256,
	}, body.Overwrite)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	a.auditFile(r, "file.upload", name, s.Path, map[string]any{
		"size_bytes": staged.Size, "sha256": staged.SHA256, "overwrite": body.Overwrite,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"op": opView(op)})
}

// handleUnzipFile serves POST /api/v1/servers/{name}/files/unzip?path=… —
// extract the .zip at path into the folder holding it. Without body.overwrite
// an archive that would replace any file changes nothing and the op ends
// file_exists with the list of them, for the caller to confirm and run again
// with overwrite. It answers 202 with the op, like a commit.
//
// Only the name is checked here, so a caller who picked the wrong file hears
// so at once; whether it is a zip, and whether every entry is safe to extract,
// is the Job's to decide (fileedit/unzip.go).
func (a *API) handleUnzipFile(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path, ok := requirePath(w, r)
	if !ok {
		return
	}
	if !strings.HasSuffix(strings.ToLower(path), ".zip") {
		writeError(w, r, newError(http.StatusBadRequest, "bad_path", "only a .zip file can be extracted"))
		return
	}
	var body startFileOpRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	release, ok := a.acquireWorld(w, r, name, maintenance.KindFileWrite, "stop the server before editing its files")
	if !ok {
		return
	}
	defer release()
	op, err := a.Files.StartUnzip(r.Context(), name, path, body.Overwrite)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	a.auditFile(r, "file.unzip", name, path, map[string]any{"overwrite": body.Overwrite})
	writeJSON(w, http.StatusAccepted, map[string]any{"op": opView(op)})
}

// handleListFileOps serves GET /api/v1/servers/{name}/files/ops — the server's
// background uploads and unzips, newest first: the one running, if any, and
// those that ended within the last half hour. It has no stopped gate: while
// one runs the server cannot start, and a finished one is still worth showing
// after it has.
func (a *API) handleListFileOps(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeServerFiles(w, r)
	if !ok {
		return
	}
	if a.Files == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "files_unavailable",
			"the file editor is not configured"))
		return
	}
	ops, err := a.Files.Ops(r.Context(), name)
	if err != nil {
		writeError(w, r, err)
		return
	}
	views := make([]fileOpView, 0, len(ops))
	for _, op := range ops {
		views = append(views, opView(op))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ops": views})
}

// fileOpView is one background file operation as the API shows it.
type fileOpView struct {
	ID         string       `json:"id"`
	Op         string       `json:"op"`
	Path       string       `json:"path"`
	State      string       `json:"state"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
	Done       int64        `json:"done"`
	Total      int64        `json:"total"`
	Files      int          `json:"files,omitempty"`
	Bytes      int64        `json:"bytes,omitempty"`
	Error      *fileOpError `json:"error,omitempty"`
}

// fileOpError is why an op failed. Code is the one the synchronous file routes
// answer with for the same refusal (writeFileEditError), or an unzip's own
// (archive_invalid, archive_unsafe, archive_symlink, type_conflict), or
// job_failed for a Job that ended without saying why; the rest is what the Job
// reported about it.
type fileOpError struct {
	Code          string   `json:"code"`
	Message       string   `json:"message"`
	Entry         string   `json:"entry,omitempty"`
	Conflicts     []string `json:"conflicts,omitempty"`
	ConflictCount int      `json:"conflict_count,omitempty"`
	Need          int64    `json:"need,omitempty"`
	Avail         int64    `json:"avail,omitempty"`
}

func opView(op fileedit.OpState) fileOpView {
	v := fileOpView{
		ID: op.ID, Op: op.Op, Path: op.Path, State: op.State, StartedAt: op.Started,
		Done: op.Done, Total: op.Total,
	}
	if !op.Finished.IsZero() {
		v.FinishedAt = &op.Finished
	}
	if op.Result != nil && op.Result.Code == "" {
		v.Files, v.Bytes = op.Result.Files, op.Result.Bytes
	}
	if op.State == fileedit.OpFailed {
		v.Error = opError(op)
	}
	return v
}

// opError maps a failed op onto the API's codes. A Job that printed no result
// carries only its reason (DeadlineExceeded, BackoffLimitExceeded, OOMKilled):
// the log it left is the world's content and the runtime's, and none of it is
// the caller's to read.
func opError(op fileedit.OpState) *fileOpError {
	res := op.Result
	if res == nil {
		msg := "the file operation stopped before it could report how it went (%s); run it again"
		switch op.Reason {
		case "DeadlineExceeded":
			msg = "the file operation ran out of time (%s); run it again"
		case fileedit.ReasonOOMKilled:
			msg = "the file operation ran out of memory (%s); an archive of this many files has to be split into smaller ones"
		}
		return &fileOpError{Code: "job_failed", Message: fmt.Sprintf(msg, op.Reason)}
	}
	code := res.Code
	switch res.Code {
	case fileedit.CodeExists:
		code = "file_exists"
	case fileedit.CodeNoSpace:
		code = "volume_full"
	case fileedit.CodeConflict:
		code = "file_changed"
	}
	return &fileOpError{
		Code: code, Message: res.Error, Entry: res.Entry,
		Conflicts: res.Conflicts, ConflictCount: res.ConflictCount, Need: res.Need, Avail: res.Avail,
	}
}

// requireFileStage checks the caller has an account to bind an upload session
// to and that sessions are configured, and returns the account.
func (a *API) requireFileStage(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := principalFromContext(r.Context())
	if p == nil || p.UserID == "" {
		writeError(w, r, errForbidden)
		return "", false
	}
	if a.FileStage == nil || a.InternalBaseURL == "" {
		writeError(w, r, newError(http.StatusServiceUnavailable, "files_unavailable",
			"uploads are not configured"))
		return "", false
	}
	return p.UserID, true
}

// authorizeServerFiles is authorizeFileOp without the stopped and world-volume
// gates: the name is valid, the server exists, and the caller owns it or is
// staff. It returns the server name.
func (a *API) authorizeServerFiles(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return "", false
	}
	rec, err := a.managedServerRecord(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return "", false
	}
	if !a.isOwnerOrAdmin(principalFromContext(r.Context()), rec) {
		writeError(w, r, errForbidden)
		return "", false
	}
	return name, true
}

// authorizeFileSession is the gate of a session's parts, status and cancel:
// the caller still owns the server (or is staff) and has the account the
// session was begun under. A session answers only that account on that server
// (fileedit.Stage), so it returns both.
func (a *API) authorizeFileSession(w http.ResponseWriter, r *http.Request) (name, user string, ok bool) {
	name, ok = a.authorizeServerFiles(w, r)
	if !ok {
		return "", "", false
	}
	user, ok = a.requireFileStage(w, r)
	return name, user, ok
}

// writeFileSessionError maps the upload session's errors onto HTTP statuses.
func writeFileSessionError(w http.ResponseWriter, r *http.Request, err error) {
	var offset *fileedit.OffsetError
	switch {
	case errors.Is(err, fileedit.ErrNotStaged):
		writeError(w, r, newError(http.StatusNotFound, "upload_not_found",
			"no such upload; it was cancelled, landed, or left idle too long, so start it again"))
	case errors.Is(err, fileedit.ErrStageFull):
		writeError(w, r, newError(http.StatusInsufficientStorage, "upload_staging_full",
			"felis has no room to take this upload right now; try again later or ask an admin"))
	case errors.Is(err, fileedit.ErrTooManySessions):
		writeError(w, r, newError(http.StatusTooManyRequests, "too_many_uploads",
			"you have %d uploads in progress; finish or cancel one first", fileedit.MaxSessionsPerUser))
	case errors.Is(err, fileedit.ErrUploadBusy):
		writeError(w, r, newError(http.StatusConflict, "upload_busy",
			"another request is still writing this upload; read where it stands and continue from there"))
	case errors.As(err, &offset):
		writeError(w, r, newError(http.StatusConflict, "upload_offset_mismatch",
			"the upload holds %d bytes; send the part that starts there", offset.Received))
	case errors.Is(err, fileedit.ErrPartTooLarge):
		writeError(w, r, newError(http.StatusRequestEntityTooLarge, "part_too_large",
			"the part is larger than part_max_bytes, or runs past the size the upload began with"))
	case errors.Is(err, fileedit.ErrDigestMismatch):
		writeError(w, r, errDigestMismatch())
	case errors.Is(err, fileedit.ErrShortUpload):
		writeError(w, r, newError(http.StatusBadRequest, "upload_incomplete",
			"the part ended before its Content-Length; read where the upload stands and send it again"))
	case errors.Is(err, fileedit.ErrUploadIncomplete):
		writeError(w, r, newError(http.StatusConflict, "upload_incomplete",
			"the upload has not finished arriving; read where it stands and send the rest"))
	default:
		writeError(w, r, err)
	}
}
