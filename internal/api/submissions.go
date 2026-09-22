package api

import (
	"context"
	"errors"
	"io"
	"net/http"

	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/submit"
)

// SubmissionService is the user-modpack approval lane the API depends on (a
// user-directed extension over the §16 build subsystem — see the internal/submit
// package doc for its provenance). It is an interface so the submission handlers
// are unit-tested against a fake; the production implementation is
// *submit.Manager. The split mirrors
// ImageBuilder: an admin builds an image directly (POST /images/build), whereas
// an ordinary user may only SUBMIT a modpack here and an admin must approve it
// before anything is built. The approval routes are admin-tier (adminOnly runs
// before the handler); the /me/submissions routes are app-tier and scope to the
// authenticated principal.
//
// The identity that matters is never taken from the body: Create stamps
// SubmittedBy from the principal and the handler ignores any submitted_by a
// client tries to send (decodeJSON rejects it as an unknown field), and Approve/
// Reject record the reviewer from the admin principal's email.
type SubmissionService interface {
	// Create records a pending_review submission. It starts NO build (the whole
	// point of the lane — nothing is built until an admin approves).
	Create(ctx context.Context, req submit.CreateRequest) (*submit.Submission, error)
	// UploadContext stores the modpack blob for the caller's own pending
	// submission at the platform-derived context ref. submittedBy is the principal,
	// never the body, so a user can only upload to a submission they own.
	UploadContext(ctx context.Context, id, submittedBy string, r io.Reader) (*submit.Submission, error)
	// ListBy returns one user's submissions, newest first (the "my uploads" view).
	ListBy(ctx context.Context, submittedBy string) ([]submit.Submission, error)
	// List returns every submission, newest first (the admin review queue).
	List(ctx context.Context) ([]submit.Submission, error)
	// Approve is the admin gate: it claims pending_review -> approved (CAS) and the
	// winner starts the SAME Trivy-gated build as an admin's direct build.
	Approve(ctx context.Context, id, reviewedBy string) (*submit.Submission, error)
	// Reject is the admin's other verdict: pending_review -> rejected with a
	// required reason; it starts no build.
	Reject(ctx context.Context, id, reviewedBy, reason string) (*submit.Submission, error)
	// OpenContext returns the stored build-context blob for the internal
	// context-fetch route: the build Pod's initContainer cannot mount the uploads
	// PVC across namespaces and holds no object-store credentials, so it streams
	// the blob from the API over the service-token-gated internal face instead.
	OpenContext(ctx context.Context, id string) (io.ReadCloser, error)
}

// createSubmissionRequest is the POST /me/submissions body. The user
// supplies ONLY a human-friendly label; identity comes from the Access principal
// and the build inputs (image/context refs) are platform-derived, never from the
// body. decodeJSON rejects unknown fields, so a client cannot smuggle a
// submitted_by or a context_ref through this endpoint.
type createSubmissionRequest struct {
	DisplayName string `json:"display_name"`
}

// rejectSubmissionRequest is the POST /submissions/{id}/reject body. A reason is
// required (the submit layer rejects an empty one with 400).
type rejectSubmissionRequest struct {
	Reason string `json:"reason"`
}

// handleCreateSubmission records a new pending_review submission (app-tier). The
// submitter is the authenticated principal's id — never the body — so a user can
// only ever file an upload under their own identity.
func (a *API) handleCreateSubmission(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	p := principalFromContext(r.Context())
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
	a.audit(r, p.Email, "submission.create", sub.ID)
	writeJSON(w, http.StatusCreated, sub)
}

// handleUploadSubmissionContext stores the caller's modpack blob as the build
// context for their own pending submission (app-tier). The request body IS the
// raw gzip tarball (context.tar.gz) — not JSON, not multipart — streamed straight
// to the transport; the submit layer sniffs the gzip magic and caps the size. The
// submitter is the authenticated principal, never the body, and a submission the
// caller does not own is reported as 404, so this endpoint cannot upload to — or
// probe the existence of — another user's submission.
//
// Uploading does not change the submission row (there is no "uploaded" column):
// the blob store is the presence source of truth, which admin approval consults.
func (a *API) handleUploadSubmissionContext(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	id := r.PathValue("id")
	sub, err := a.Submissions.UploadContext(r.Context(), id, p.UserID, r.Body)
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	a.audit(r, p.Email, "submission.upload", sub.ID)
	writeJSON(w, http.StatusOK, sub)
}

// handleMySubmissions lists the caller's own submissions (app-tier). It scopes
// strictly to the principal's id; there is no parameter that could widen the
// query to another user's uploads. Each row is enriched with its linked build's
// outcome — this list is the only player-visible outlet for a build result, so
// a failed build is not invisible to the person who submitted it.
func (a *API) handleMySubmissions(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	subs, err := a.Submissions.ListBy(r.Context(), p.UserID)
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	views, err := a.submissionViews(r.Context(), subs)
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"submissions": views})
}

// handleListSubmissions is the admin review queue: every submission across all
// users, newest first (admin-tier — it reads other users' uploads, so it gates
// on the admin Zero-Trust path via adminOnly). Rows carry the same build
// outcome enrichment as /me/submissions.
func (a *API) handleListSubmissions(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	subs, err := a.Submissions.List(r.Context())
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	views, err := a.submissionViews(r.Context(), subs)
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"submissions": views})
}

// submissionView is one submission row enriched with its linked build's
// outcome. The row itself is embedded unchanged, so the wire shape only gains
// the two optional fields; they appear solely once a build has been linked
// (BuildID set) and its record is still readable.
type submissionView struct {
	submit.Submission
	BuildStatus string `json:"build_status,omitempty"`
	BuildError  string `json:"build_error,omitempty"`
}

// submissionViews enriches each submission with its linked build's status via a
// read-only Builder.Get — deliberately never Sync, because the 15s reconcile
// loop owns state advance and rendering a list must not touch the cluster. A
// submission with no linked build (never approved, or approved before the
// hand-off could record the id), no Builder wired, or a build row that is gone
// (ErrNotFound) renders without the extra fields; any other store failure is
// returned so the handler reports it rather than silently dropping the outcome.
func (a *API) submissionViews(ctx context.Context, subs []submit.Submission) ([]submissionView, error) {
	views := make([]submissionView, len(subs))
	for i, s := range subs {
		views[i] = submissionView{Submission: s}
		if a.Builder == nil || s.BuildID == "" {
			continue
		}
		bld, err := a.Builder.Get(ctx, s.BuildID)
		if errors.Is(err, build.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		views[i].BuildStatus = string(bld.Status)
		views[i].BuildError = bld.Error
	}
	return views, nil
}

// handleApproveSubmission is the admin approve gate (admin-tier). The reviewer is
// the admin principal's email; the build inputs are derived inside the submit
// layer, so this handler forwards no client-controlled build parameter.
func (a *API) handleApproveSubmission(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	id := r.PathValue("id")
	sub, err := a.Submissions.Approve(r.Context(), id, p.Email)
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	a.audit(r, p.Email, "submission.approve", sub.ID)
	writeJSON(w, http.StatusOK, sub)
}

// handleRejectSubmission records an admin rejection with a required reason
// (admin-tier). It starts no build.
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
	a.audit(r, p.Email, "submission.reject", sub.ID)
	writeJSON(w, http.StatusOK, sub)
}

// errSubmissionsUnavailable is returned when the approval lane is not configured
// on this api instance (a nil Submissions service), so the admin/app boundary is
// still exercised before the subsystem is wired in.
var errSubmissionsUnavailable = newError(http.StatusServiceUnavailable, "submissions_unavailable",
	"modpack submission subsystem is not configured")

// writeSubmitError maps submit-package errors onto HTTP status codes. Only the
// business sentinels are client-facing: a validation failure is 400, a missing
// submission is 404, an already-reviewed submission is 409, and an unconfigured
// upload transport is 503 (the store this deployment set has no implemented
// transport — an honest "not available here", not a client error). Everything
// else — including a build.ErrInvalid raised by the pre-CAS build.Validate (a
// platform registry/context MISCONFIGURATION, never client input, since every
// build input is platform-derived) and a post-CAS Submit hand-off failure — is a
// server-side fault that collapses to 500 via writeError. The lane deliberately
// does not surface those as 4xx: the client did nothing wrong.
func writeSubmitError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, submit.ErrInvalid):
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "%s", err.Error()))
	case errors.Is(err, submit.ErrNotFound):
		writeError(w, r, newError(http.StatusNotFound, "not_found", "submission not found"))
	case errors.Is(err, submit.ErrAlreadyReviewed):
		writeError(w, r, newError(http.StatusConflict, "already_reviewed",
			"submission has already been reviewed"))
	case errors.Is(err, submit.ErrBlobNotFound):
		writeError(w, r, newError(http.StatusNotFound, "not_found", "no context uploaded for this submission"))
	case errors.Is(err, submit.ErrUploadsUnavailable):
		writeError(w, r, newError(http.StatusServiceUnavailable, "uploads_unavailable",
			"modpack upload transport is not configured"))
	default:
		writeError(w, r, err)
	}
}

// handleInternalSubmissionContext streams a submission's stored build-context
// tarball to the build Pod's `felis fetch-context` initContainer. It lives on the
// internal face (service-token, no Zero Trust) because its only caller is
// in-cluster infrastructure: the build Job runs in the build namespace, where it
// can neither mount the uploads PVC nor hold object-store credentials, so the API
// — which wrote the blob — is the transport. The blob is served verbatim; the
// fetcher extracts it under a zip-slip guard, and Kaniko treats the result as
// hostile regardless (spec §16).
func (a *API) handleInternalSubmissionContext(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	rc, err := a.Submissions.OpenContext(r.Context(), r.PathValue("id"))
	if err != nil {
		writeSubmitError(w, r, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	if _, err := io.Copy(w, rc); err != nil {
		// The status is already committed; the client sees a truncated stream and
		// the fetch fails on size/extract, so there is nothing left to write here.
		return
	}
}

// Compile-time proof that the production Manager satisfies the API interface.
var _ SubmissionService = (*submit.Manager)(nil)
