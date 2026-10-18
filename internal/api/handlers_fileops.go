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

// Upload sessions reserve the full size on staging disk; background landing and unzip Jobs hold the world-volume lock.

// fileSessionView includes ordered part digests so a resumed upload can verify the local file has not changed.
type fileSessionView struct {
	ID           string         `json:"id"`
	Path         string         `json:"path"`
	Size         int64          `json:"size"`
	Received     int64          `json:"received"`
	PartMaxBytes int64          `json:"part_max_bytes"`
	Parts        []filePartView `json:"parts"`
}

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

func namesFit(path string) bool {
	for _, name := range strings.Split(path, "/") {
		if len(name) > fileedit.NameMax {
			return false
		}
	}
	return true
}

func errNameTooLong() *apiError {
	return newError(http.StatusBadRequest, "bad_path", "a name in the path is longer than %d bytes", fileedit.NameMax)
}

type beginFileUploadRequest struct {
	Size *int64 `json:"size"`
}

// handleBeginFileUpload requires a stopped server; later parts may arrive while running, but commit must wait.
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
	// Reject unusable paths before staging; the Job rechecks containment when landing the file.
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

// handleFileUploadPart requires the current offset; interrupted or digest-mismatched parts leave the session unchanged.
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

type startFileOpRequest struct {
	Overwrite bool `json:"overwrite"`
}

// handleCommitFileUpload locks the world before Seal so a concurrent commit cannot rotate a running Job's fetch token.
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
	// Seal rechecks under the staging lock in case another part arrives after this check.
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

// handleUnzipFile leaves existing files unchanged without overwrite; the Job validates archive entries.
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

// handleListFileOps has no stopped-server gate so completed operations remain visible after startup.
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

// opError exposes only the Job's reason when no result was printed; raw logs may contain private world data.
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

// authorizeServerFiles omits the stopped-server and world-volume checks.
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

// authorizeFileSession requires server access and the same account that began the session.
func (a *API) authorizeFileSession(w http.ResponseWriter, r *http.Request) (name, user string, ok bool) {
	name, ok = a.authorizeServerFiles(w, r)
	if !ok {
		return "", "", false
	}
	user, ok = a.requireFileStage(w, r)
	return name, user, ok
}

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
