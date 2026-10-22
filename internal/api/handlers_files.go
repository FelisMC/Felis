package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// FileEditor edits world-volume files; StartUpload and StartUnzip return before their Jobs finish.
type FileEditor interface {
	List(ctx context.Context, server, path string) (fileedit.Listing, error)
	// Read returns the file hash before redaction.
	Read(ctx context.Context, server, path string) (content []byte, sha256 string, err error)
	// Write rejects a changed expect hash or an existing path with createOnly; it returns lowercase SHA-256 hex.
	Write(ctx context.Context, server, path string, content []byte, expect string, createOnly bool) (sha256 string, err error)
	Mkdir(ctx context.Context, server, path string) error
	Delete(ctx context.Context, server, path string) error
	Rename(ctx context.Context, server, path, to string) error
	// Upload writes exactly src or fails; overwrite=false rejects an existing path.
	Upload(ctx context.Context, server, path string, src fileedit.UploadSource, overwrite bool) error
	StartUpload(ctx context.Context, server, path string, src fileedit.UploadSource, overwrite bool) (fileedit.OpState, error)
	StartUnzip(ctx context.Context, server, path string, overwrite bool) (fileedit.OpState, error)
	Ops(ctx context.Context, server string) ([]fileedit.OpState, error)
}

func (a *API) handleInternalFileBrowser(w http.ResponseWriter, r *http.Request) {
	if a.FileBrowser == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "files_unavailable", "file browser is not configured"))
		return
	}
	a.FileBrowser.ServeHTTP(w, r)
}

// writeFileRequest distinguishes omitted content from an explicit empty file; content is base64-encoded bytes.
type writeFileRequest struct {
	Content       *[]byte `json:"content"`
	ContentSHA256 string  `json:"content_sha256"`
	ExpectSHA256  string  `json:"expect_sha256,omitempty"`
	CreateOnly    bool    `json:"create_only,omitempty"`
}

// handleListFiles delegates path containment to os.Root inside the Job at file-open time.
func (a *API) handleListFiles(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")

	ls, err := a.Files.List(r.Context(), name, path)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	if ls.Entries == nil {
		ls.Entries = []fileedit.Entry{} // encode empty directories as []
	}
	// Unavailable free space is null; zero means the volume is full.
	var free any = ls.Free
	if ls.Free < 0 {
		free = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": path, "entries": ls.Entries, "truncated": ls.Truncated, "free_bytes": free,
	})
}

// handleReadFile rejects oversized files rather than returning a partial read that a later save could truncate.
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
		content = []byte{} // encode empty files as an empty base64 string
	}
	// content_sha256 covers transmitted bytes; sha256 covers the file before redaction.
	got := sha256.Sum256(content)
	writeJSON(w, http.StatusOK, map[string]any{
		"path": path, "content": content, "sha256": sum, "content_sha256": hex.EncodeToString(got[:]),
	})
}

// handleWriteFile caps content before Job creation to stay below etcd's object-size limit.
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
	// DisallowUnknownFields rejects unknown fields, but does not require content to be present.
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
	// The hash is passed in Job arguments, so accept only canonical lowercase SHA-256 hex.
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
	switch sum := sha256.Sum256(*body.Content); {
	case body.ContentSHA256 == "":
		writeError(w, r, newError(http.StatusBadRequest, "digest_required",
			"send the SHA-256 of the content as content_sha256"))
		return
	case !sha256Hex.MatchString(body.ContentSHA256):
		writeError(w, r, newError(http.StatusBadRequest, "bad_digest",
			"content_sha256 must be the 64-digit lowercase hex SHA-256 of the content"))
		return
	case hex.EncodeToString(sum[:]) != body.ContentSHA256:
		writeError(w, r, newError(http.StatusBadRequest, "digest_mismatch",
			"the content that arrived does not hash to content_sha256, so it was changed on the way; send it again"))
		return
	}

	// Writes hold the world-volume lock; overlapping reads rely on the expected hash to prevent stale saves.
	kind := maintenance.KindFileWrite
	if liveExperienceFile(r, name) {
		kind = maintenance.KindConfigWrite
	}
	release, ok := a.acquireFileWorld(w, r, name, kind, "wait for the server to stop or finish starting before editing its files")
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

// requirePath writes a 400 response when returning false.
func requirePath(w http.ResponseWriter, r *http.Request) (string, bool) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"the ?path= query parameter is required"))
		return "", false
	}
	return path, true
}

func (a *API) handleMkdir(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path, ok := requirePath(w, r)
	if !ok {
		return
	}
	release, ok := a.acquireFileWorld(w, r, name, maintenance.KindFileWrite, "wait for the server to stop or finish starting before editing its files")
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

// handleDeleteFile removes symlinks without following them and refuses the world root.
func (a *API) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path, ok := requirePath(w, r)
	if !ok {
		return
	}
	release, ok := a.acquireFileWorld(w, r, name, maintenance.KindFileWrite, "wait for the server to stop or finish starting before editing its files")
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

type renameFileRequest struct {
	To string `json:"to"`
}

// handleRenameFile protects secret-bearing paths because renaming them could bypass read redaction.
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
	release, ok := a.acquireFileWorld(w, r, name, maintenance.KindFileWrite, "wait for the server to stop or finish starting before editing its files")
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

// handleUploadFile stages and verifies the declared length and digest before locking the world volume.
func (a *API) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	path, ok := requirePath(w, r)
	if !ok {
		return
	}
	if !namesFit(path) {
		writeError(w, r, errNameTooLong())
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
			"the file is %d bytes; one request carries at most %d, so send it as an upload session (POST files/uploads)",
			r.ContentLength, fileedit.MaxUploadBytes))
		return
	}
	want, ok := contentDigest(w, r)
	if !ok {
		return
	}
	overwrite := r.URL.Query().Get("overwrite") == "true"

	staged, drop, err := a.FileStage.Put(r.Body, r.ContentLength, want)
	switch {
	case errors.Is(err, fileedit.ErrDigestMismatch):
		writeError(w, r, errDigestMismatch())
		return
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

	release, ok := a.acquireFileWorld(w, r, name, maintenance.KindFileWrite, "wait for the server to stop or finish starting before editing its files")
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

// handleInternalFileUpload uses a single-use upload token; unknown IDs and invalid or spent tokens all return 404.
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

// handleInternalFileUploadLanded deletes staged bytes only after success, allowing failed Jobs to retry without reuploading.
func (a *API) handleInternalFileUploadLanded(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if a.FileStage == nil || !ok || a.FileStage.Landed(r.PathValue("id"), token) != nil {
		writeError(w, r, newError(http.StatusNotFound, "not_found", "no such upload"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// contentDigest requires a SHA-256 Content-Digest and writes the error response when returning false.
func contentDigest(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	sum, err := parseContentDigest(r.Header.Values("Content-Digest"))
	switch {
	case errors.Is(err, errNoContentDigest):
		writeError(w, r, newError(http.StatusBadRequest, "digest_required",
			"send the SHA-256 of the body as Content-Digest: sha-256=:<base64>:"))
		return nil, false
	case err != nil:
		writeError(w, r, newError(http.StatusBadRequest, "bad_digest",
			"Content-Digest must carry sha-256=:<base64 of the 32-byte SHA-256>:"))
		return nil, false
	}
	return sum, true
}

var (
	errNoContentDigest  = errors.New("no sha-256 Content-Digest")
	errBadContentDigest = errors.New("a malformed sha-256 Content-Digest")
)

// parseContentDigest reads RFC 9530 sha-256=:<base64>:, ignoring other algorithms.
func parseContentDigest(values []string) ([]byte, error) {
	var found []byte
	for _, v := range values {
		for _, member := range strings.Split(v, ",") {
			key, value, _ := strings.Cut(member, "=")
			if !strings.EqualFold(strings.TrimSpace(key), "sha-256") {
				continue
			}
			value, _, _ = strings.Cut(value, ";")
			value = strings.TrimSpace(value)
			inner, pre := strings.CutPrefix(value, ":")
			inner, post := strings.CutSuffix(inner, ":")
			sum, err := base64.StdEncoding.DecodeString(inner)
			if !pre || !post || err != nil || len(sum) != sha256.Size {
				return nil, errBadContentDigest
			}
			found = sum
		}
	}
	if found == nil {
		return nil, errNoContentDigest
	}
	return found, nil
}

func errDigestMismatch() error {
	return newError(http.StatusBadRequest, "digest_mismatch",
		"the bytes that arrived do not match their Content-Digest, so they were changed on the way; send them again")
}

// auditFile uses <server>:<path> as the target and extra as the optional payload.
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

// File access permits stable running servers while backup, restore and migration still hold the world.
func (a *API) authorizeFileOp(w http.ResponseWriter, r *http.Request) (string, bool) {
	name, ok := a.authorizeServerFiles(w, r)
	if !ok {
		return "", false
	}

	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return "", false
	}
	running := info.Phase == string(v1alpha1.PhaseRunning) && info.DesiredState == string(v1alpha1.DesiredRunning)
	if !running && !liveExperienceFile(r, name) && (info.Ready || info.DesiredState != string(v1alpha1.DesiredStopped) || info.Phase == string(v1alpha1.PhaseStarting) || info.Phase == string(v1alpha1.PhaseStopping)) {
		writeError(w, r, newError(http.StatusConflict, "server_transitioning",
			"wait for the server to finish starting or stopping before working with its files"))
		return "", false
	}
	if err := a.Cluster.CheckFileAccess(r.Context(), name); err != nil {
		a.writeLookupError(w, r, maintenanceError(err, ""))
		return "", false
	}

	if a.Files == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "files_unavailable",
			"the file editor is not configured"))
		return "", false
	}

	// Create the PVC before first start or after archival; the StatefulSet adopts it on start.
	if exists, err := a.Cluster.WorldVolumeExists(r.Context(), name); err != nil {
		writeError(w, r, err)
		return "", false
	} else if !exists {
		if err := a.Cluster.EnsureWorldVolume(r.Context(), name); err != nil {
			a.writeLookupError(w, r, err)
			return "", false
		}
	}
	return name, true
}

// Startup-only system-plugin settings remain accessible during server transitions.
func liveExperienceFile(r *http.Request, name string) bool {
	return naming.IsSystemServer(name) && strings.HasSuffix(r.URL.Path, "/file") &&
		(r.Method == http.MethodGet || r.Method == http.MethodPut) &&
		r.URL.Query().Get("path") == naming.ExperienceConfigFile
}

// writeFileEditError maps timeouts to 504 and hides unrecognized executor errors behind 500.
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
	case errors.Is(err, fileedit.ErrDigestMismatch):
		writeError(w, r, newError(http.StatusBadRequest, "digest_mismatch", "%s", err.Error()))
	case errors.Is(err, fileedit.ErrReadDamaged):
		writeError(w, r, newError(http.StatusBadGateway, "read_damaged",
			"the file's bytes changed on their way from the file Job; read it again"))
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, r, newError(http.StatusGatewayTimeout, "files_timeout",
			"the file operation did not finish in time; retry shortly"))
	default:
		writeError(w, r, err)
	}
}
