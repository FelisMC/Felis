package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"felis.lolicon.best/internal/submit"
)

// SubmissionService derives submitter and reviewer identity from authenticated principals, never request bodies.
type SubmissionService interface {
	// Create records pending_review without starting a build.
	Create(ctx context.Context, req submit.CreateRequest) (*submit.Submission, error)
	// UploadContext requires the submitter's identity and an owned pending submission.
	UploadContext(ctx context.Context, id, submittedBy string, r io.Reader) (*submit.Submission, error)
	// Chunked uploads resume at the staged length and use the same ownership checks as UploadContext.
	UploadStatus(ctx context.Context, id, submittedBy string) (submit.UploadProgress, error)
	UploadPart(ctx context.Context, id, submittedBy string, offset int64, r io.Reader) (submit.UploadProgress, error)
	CompleteUpload(ctx context.Context, id, submittedBy string) (*submit.Submission, error)
	// ListBy scopes results to submittedBy regardless of opts, newest first.
	ListBy(ctx context.Context, submittedBy string, opts submit.ListOpts) (submit.Page, error)
	List(ctx context.Context, opts submit.ListOpts) (submit.Page, error)
	// Approve starts one scan-gated build via CAS and rejects a context whose digest differs from expectedDigest.
	Approve(ctx context.Context, id, reviewedBy, expectedDigest string) (*submit.Submission, error)
	Reject(ctx context.Context, id, reviewedBy, reason string) (*submit.Submission, error)
	// Withdraw returns 404 for another user's submission and 409 after review.
	Withdraw(ctx context.Context, id, submittedBy string) (*submit.Submission, error)
	Delete(ctx context.Context, id string) (*submit.Submission, error)
	// OpenContext returns the blob and recorded SHA-256, or an empty digest for older uploads; the caller closes the stream.
	OpenContext(ctx context.Context, id string) (io.ReadCloser, string, error)
}

type createSubmissionRequest struct {
	DisplayName string `json:"display_name"`
}

// Keep create and upload cooldowns separate so a new submission can receive its context immediately.
const (
	submissionCreateKey = "create:"
	submissionUploadKey = "upload:"
)

type rejectSubmissionRequest struct {
	Reason string `json:"reason"`
}

type approveSubmissionRequest struct {
	ExpectedDigest string `json:"expected_digest"`
}

const contextDigestHeader = "X-Felis-Context-Sha256"

func (a *API) handleCreateSubmission(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	// Reserve before insertion to serialize concurrent creates; failed creates release the cooldown.
	lim := a.submitLimiter()
	reservedAt, ok := lim.reserve(submissionCreateKey+p.UserID, a.SubmitCreateCooldown)
	if !ok {
		writeError(w, r, newError(http.StatusTooManyRequests, "submission_cooldown",
			"a submission was created recently; wait a moment before creating another"))
		return
	}
	committed := false
	defer func() {
		if !committed {
			lim.release(submissionCreateKey+p.UserID, reservedAt)
		}
	}()
	var body createSubmissionRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	sub, err := a.Submissions.Create(r.Context(), submit.CreateRequest{
		DisplayName: body.DisplayName,
		SubmittedBy: p.UserID,
	})
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	committed = true
	a.audit(r, "submission.create", sub.ID)
	writeJSON(w, http.StatusCreated, sub)
}

// handleUploadSubmissionContext accepts raw gzip tarballs and returns 404 for submissions owned by another user.
func (a *API) handleUploadSubmissionContext(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	want, ok := contentDigest(w, r)
	if !ok {
		return
	}
	p := principalFromContext(r.Context())
	// Reserve before reading the expensive body to bound concurrent uploads; failed uploads release the cooldown.
	commit, release, ok := a.reserveUpload(w, r, p.UserID)
	if !ok {
		return
	}
	defer release()
	id := r.PathValue("id")
	sub, err := a.Submissions.UploadContext(r.Context(), id, p.UserID, submit.VerifyDigest(r.Body, want))
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	commit()
	a.audit(r, "submission.upload", sub.ID)
	writeJSON(w, http.StatusOK, sub)
}

// reserveUpload returns commit and deferred release callbacks; release rolls back unless committed.
func (a *API) reserveUpload(w http.ResponseWriter, r *http.Request, userID string) (commit, release func(), ok bool) {
	lim := a.submitLimiter()
	reservedAt, ok := lim.reserve(submissionUploadKey+userID, a.SubmitUploadCooldown)
	if !ok {
		writeError(w, r, newError(http.StatusTooManyRequests, "submission_cooldown",
			"an upload was accepted recently; wait a moment before uploading again"))
		return nil, nil, false
	}
	committed := false
	return func() { committed = true }, func() {
		if !committed {
			lim.release(submissionUploadKey+userID, reservedAt)
		}
	}, true
}

func (a *API) handleContextUploadStatus(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	prog, err := a.Submissions.UploadStatus(r.Context(), r.PathValue("id"), p.UserID)
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, prog)
}

// handleContextUploadPart resets at offset 0 or appends at the staged length; failed parts roll back.
func (a *API) handleContextUploadPart(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"offset must be the byte position the part starts at"))
		return
	}
	want, ok := contentDigest(w, r)
	if !ok {
		return
	}
	p := principalFromContext(r.Context())
	prog, err := a.Submissions.UploadPart(r.Context(), r.PathValue("id"), p.UserID, offset, submit.VerifyDigest(r.Body, want))
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, prog)
}

// handleContextUploadComplete applies the upload cooldown when the staged context is stored.
func (a *API) handleContextUploadComplete(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	commit, release, ok := a.reserveUpload(w, r, p.UserID)
	if !ok {
		return
	}
	defer release()
	sub, err := a.Submissions.CompleteUpload(r.Context(), r.PathValue("id"), p.UserID)
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	commit()
	a.audit(r, "submission.upload", sub.ID)
	writeJSON(w, http.StatusOK, sub)
}

type SubmissionLimits struct {
	MaxContextBytes int64 `json:"max_context_bytes"`
}

func (a *API) handleSubmissionLimits(w http.ResponseWriter, r *http.Request) {
	l, ok := a.Submissions.(interface{ ContextLimit() int64 })
	if !ok {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, SubmissionLimits{MaxContextBytes: l.ContextLimit()})
}

func (a *API) handleMySubmissions(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	page, err := a.Submissions.ListBy(r.Context(), p.UserID, submissionListOpts(r))
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	a.writeSubmissionPage(w, r, page)
}

func (a *API) handleListSubmissions(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	page, err := a.Submissions.List(r.Context(), submissionListOpts(r))
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	a.writeSubmissionPage(w, r, page)
}

func submissionListOpts(r *http.Request) submit.ListOpts {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	return submit.ListOpts{
		Status: submit.Status(q.Get("status")), Query: q.Get("query"),
		Limit: limit, Offset: offset,
	}
}

// writeSubmissionPage includes zero-valued status counts so every status key is present.
func (a *API) writeSubmissionPage(w http.ResponseWriter, r *http.Request, page submit.Page) {
	views, err := a.submissionViews(r.Context(), page.Submissions)
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	counts := map[string]int{}
	for _, st := range []submit.Status{submit.StatusPendingReview, submit.StatusApproved, submit.StatusRejected} {
		counts[string(st)] = page.Counts[st]
	}
	writeJSON(w, http.StatusOK, map[string]any{"submissions": views, "total": page.Total, "counts": counts})
}

type submissionView struct {
	submit.Submission
	BuildStatus string `json:"build_status,omitempty"`
	BuildError  string `json:"build_error,omitempty"`
}

// submissionViews reads builds without reconciliation; list rendering must not advance state or contact the cluster.
func (a *API) submissionViews(ctx context.Context, subs []submit.Submission) ([]submissionView, error) {
	views := make([]submissionView, len(subs))
	var ids []string
	for i, s := range subs {
		views[i] = submissionView{Submission: s}
		if s.BuildID != "" {
			ids = append(ids, s.BuildID)
		}
	}
	if a.Builder == nil || len(ids) == 0 {
		return views, nil
	}
	builds, err := a.Builder.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range views {
		if bld, ok := builds[views[i].BuildID]; ok {
			views[i].BuildStatus = string(bld.Status)
			views[i].BuildError = bld.Error
		}
	}
	return views, nil
}

func (a *API) handleApproveSubmission(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	id := r.PathValue("id")
	var body approveSubmissionRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &body); err != nil {
			writeError(w, r, err)
			return
		}
	}
	sub, err := a.Submissions.Approve(r.Context(), id, p.Email, body.ExpectedDigest)
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	a.audit(r, "submission.approve", sub.ID)
	writeJSON(w, http.StatusOK, sub)
}

func (a *API) handleRejectSubmission(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	id := r.PathValue("id")
	var body rejectSubmissionRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	sub, err := a.Submissions.Reject(r.Context(), id, p.Email, body.Reason)
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	a.audit(r, "submission.reject", sub.ID)
	writeJSON(w, http.StatusOK, sub)
}

// handleWithdrawSubmission returns 404 for another user's submission and 409 after review.
func (a *API) handleWithdrawSubmission(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	sub, err := a.Submissions.Withdraw(r.Context(), r.PathValue("id"), p.UserID)
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	a.audit(r, "submission.withdraw", sub.ID)
	writeJSON(w, http.StatusOK, sub)
}

// handleDeleteSubmission may interrupt an active build's context fetch.
func (a *API) handleDeleteSubmission(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	sub, err := a.Submissions.Delete(r.Context(), r.PathValue("id"))
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	a.audit(r, "submission.delete", sub.ID)
	writeJSON(w, http.StatusOK, sub)
}

const uploadsStoreRetry = 5 * time.Second

var errSubmissionsUnavailable = newError(http.StatusServiceUnavailable, "submissions_unavailable",
	"modpack submission subsystem is not configured")

// writeSubmitError treats platform-derived build validation and handoff failures as server faults, not invalid client input.
func writeSubmitError(w http.ResponseWriter, r *http.Request, err error) {
	var mismatch *submit.OffsetMismatchError
	switch {
	case errors.Is(err, submit.ErrInvalid):
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "%s", err.Error()))
	case errors.Is(err, submit.ErrNotFound):
		writeError(w, r, newError(http.StatusNotFound, "not_found", "submission not found"))
	case errors.Is(err, submit.ErrAlreadyReviewed):
		writeError(w, r, newError(http.StatusConflict, "already_reviewed",
			"submission has already been reviewed"))
	case errors.Is(err, submit.ErrContextChanged):
		writeError(w, r, newError(http.StatusConflict, "context_changed",
			"the build context was uploaded again after it was reviewed; review the new upload before approving"))
	case errors.Is(err, submit.ErrQuotaExceeded):
		writeError(w, r, newError(http.StatusForbidden, "submission_quota_exceeded",
			"submission quota reached"))
	case errors.Is(err, submit.ErrBlobNotFound):
		writeError(w, r, newError(http.StatusNotFound, "not_found", "no context uploaded for this submission"))
	case errors.Is(err, submit.ErrUploadsFull):
		writeError(w, r, newError(http.StatusInsufficientStorage, "uploads_full",
			"the uploads store is full; an admin has to delete reviewed submissions before new uploads fit"))
	case errors.Is(err, submit.ErrUploadsUnavailable):
		writeError(w, r, newError(http.StatusServiceUnavailable, "uploads_unavailable",
			"modpack upload transport is not configured"))
	case errors.Is(err, submit.ErrStoreUnavailable):
		// A failed budget check writes nothing, so the same upload request can retry.
		log.Printf("api: %s %s: %v", r.Method, r.URL.Path, err)
		writeError(w, r, newError(http.StatusServiceUnavailable, "uploads_store_unavailable",
			"the uploads store did not answer; send the request again").retryAfter(uploadsStoreRetry))
	case errors.Is(err, submit.ErrUploadBusy):
		writeError(w, r, newError(http.StatusConflict, "upload_busy",
			"another request is still writing this upload; read where it stands and continue from there"))
	case errors.Is(err, submit.ErrDigestMismatch):
		writeError(w, r, errDigestMismatch())
	case errors.As(err, &mismatch):
		writeError(w, r, newError(http.StatusConflict, "upload_offset_mismatch",
			"the upload holds %d bytes; send the part that starts there", mismatch.Received))
	case errors.Is(err, submit.ErrPartTooLarge):
		writeError(w, r, newError(http.StatusRequestEntityTooLarge, "part_too_large",
			"the part is larger than part_max_bytes in the upload status"))
	default:
		writeError(w, r, err)
	}
}

// handleInternalSubmissionContext serves build Jobs that cannot mount the cross-namespace uploads PVC.
func (a *API) handleInternalSubmissionContext(w http.ResponseWriter, r *http.Request) {
	rc, digest, ok := a.openSubmissionContext(w, r)
	if !ok {
		return
	}
	streamSubmissionContext(w, r, rc, digest)
}

// handleAdminSubmissionContext uses attachment disposition to prevent rendering the untrusted archive.
func (a *API) handleAdminSubmissionContext(w http.ResponseWriter, r *http.Request) {
	rc, digest, ok := a.openSubmissionContext(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="context.tar.gz"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	a.audit(r, "submission.context.download", r.PathValue("id"))
	streamSubmissionContext(w, r, rc, digest)
}

// openSubmissionContext writes the error response when opening the blob fails.
func (a *API) openSubmissionContext(w http.ResponseWriter, r *http.Request) (io.ReadCloser, string, bool) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return nil, "", false
	}
	rc, digest, err := a.Submissions.OpenContext(r.Context(), r.PathValue("id"))
	if err != nil {
		writeSubmitError(w, r, err)
		return nil, "", false
	}
	return rc, digest, true
}

// streamSubmissionContext holds the final chunk until the recorded digest matches; failure aborts the response.
func streamSubmissionContext(w http.ResponseWriter, r *http.Request, rc io.ReadCloser, digest string) {
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	if digest == "" {
		// Older contexts have no recorded digest to verify.
		_, _ = io.Copy(w, rc)
		return
	}
	w.Header().Set(contextDigestHeader, digest)
	h := sha256.New()
	buf := make([]byte, 32<<10)
	var held []byte
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if len(held) > 0 {
				if _, werr := w.Write(held); werr != nil {
					return
				}
			}
			held = append(held[:0], buf[:n]...)
			h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("api: submission %s context read failed mid-stream: %v", r.PathValue("id"), err)
			panic(http.ErrAbortHandler)
		}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		log.Printf("api: submission %s context hashes to %s, the row records %s; response aborted", r.PathValue("id"), got, digest)
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(held)
}

var _ SubmissionService = (*submit.Manager)(nil)
