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
	// UploadStatus, UploadPart and CompleteUpload are the chunked form of
	// UploadContext, for a context larger than one request carries through the
	// edge (Cloudflare refuses bodies over 100 MB): parts are staged in order, the
	// staged length is the resume point, and completion stores the whole through
	// UploadContext's checks. Same owner scoping.
	UploadStatus(ctx context.Context, id, submittedBy string) (submit.UploadProgress, error)
	UploadPart(ctx context.Context, id, submittedBy string, offset int64, r io.Reader) (submit.UploadProgress, error)
	CompleteUpload(ctx context.Context, id, submittedBy string) (*submit.Submission, error)
	// ListBy returns one page of one user's submissions, newest first (the "my
	// uploads" view). The scope is submittedBy, whatever opts says.
	ListBy(ctx context.Context, submittedBy string, opts submit.ListOpts) (submit.Page, error)
	// List returns one page of every submission, newest first (the admin review
	// queue).
	List(ctx context.Context, opts submit.ListOpts) (submit.Page, error)
	// Approve is the admin gate: it claims pending_review -> approved (CAS) and the
	// winner starts the SAME Trivy-gated build as an admin's direct build.
	// expectedDigest is the context sha256 the reviewer inspected; a row whose
	// context has since been replaced refuses with ErrContextChanged.
	Approve(ctx context.Context, id, reviewedBy, expectedDigest string) (*submit.Submission, error)
	// Reject is the admin's other verdict: pending_review -> rejected with a
	// required reason; it starts no build.
	Reject(ctx context.Context, id, reviewedBy, reason string) (*submit.Submission, error)
	// Withdraw retracts the caller's OWN pending submission: the row and its
	// uploaded context are deleted. A reviewed submission is frozen (409) and a
	// submission the caller does not own reads back as 404, like the upload route.
	Withdraw(ctx context.Context, id, submittedBy string) (*submit.Submission, error)
	// Delete retires any submission outright (the admin lifecycle valve): the row
	// and its uploaded context are removed, any status.
	Delete(ctx context.Context, id string) (*submit.Submission, error)
	// OpenContext returns the stored build-context blob for the internal
	// context-fetch route: the build Pod's initContainer cannot mount the uploads
	// PVC across namespaces and holds no object-store credentials, so it streams
	// the blob from the API over the service-token-gated internal face instead.
	// The string is the sha256 the row records for the blob ("" when none was
	// recorded); the routes refuse to finish a stream that does not match it.
	OpenContext(ctx context.Context, id string) (io.ReadCloser, string, error)
}

// createSubmissionRequest is the POST /me/submissions body. The user
// supplies ONLY a human-friendly label; identity comes from the Access principal
// and the build inputs (image/context refs) are platform-derived, never from the
// body. decodeJSON rejects unknown fields, so a client cannot smuggle a
// submitted_by or a context_ref through this endpoint.
type createSubmissionRequest struct {
	DisplayName string `json:"display_name"`
}

// The submission lane's two cooldown keys, prefixed into the shared submit
// limiter's per-user keys. Create and upload are separate levers on purpose:
// creating a submission and then immediately uploading its context is the lane's
// normal shape, so one must never consume the other's window.
const (
	submissionCreateKey = "create:"
	submissionUploadKey = "upload:"
)

// rejectSubmissionRequest is the POST /submissions/{id}/reject body. A reason is
// required (the submit layer rejects an empty one with 400).
type rejectSubmissionRequest struct {
	Reason string `json:"reason"`
}

// approveSubmissionRequest is the POST /submissions/{id}/approve body: the
// context digest the admin reviewed. The panel sends the digest of the bytes it
// downloaded (the X-Felis-Context-Sha256 header), or the listed one.
type approveSubmissionRequest struct {
	ExpectedDigest string `json:"expected_digest"`
}

// contextDigestHeader carries the recorded sha256 on both context routes, so a
// reviewer can compare it with `sha256sum` and the panel can approve exactly the
// bytes it fetched.
const contextDigestHeader = "X-Felis-Context-Sha256"

// handleCreateSubmission records a new pending_review submission (app-tier). The
// submitter is the authenticated principal's id — never the body — so a user can
// only ever file an upload under their own identity.
func (a *API) handleCreateSubmission(w http.ResponseWriter, r *http.Request) {
	if a.Submissions == nil {
		writeError(w, r, errSubmissionsUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	// Reserve the per-user create cooldown BEFORE the store write. Unlike the
	// wake lever's allowed→record (whose real gate is the running cap and whose
	// effect is idempotent), a create is a non-idempotent row insertion with no
	// other bound on its rate, so a burst of truly concurrent creates must yield
	// exactly one winner per window. The deferred rollback frees the window
	// whenever the create fails — a 400 typo, a spent quota, a store error — so
	// only a row that was actually recorded consumes it.
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

// handleUploadSubmissionContext stores the caller's modpack blob as the build
// context for their own pending submission (app-tier). The request body IS the
// raw gzip tarball (context.tar.gz) — not JSON, not multipart — streamed straight
// to the transport; the submit layer sniffs the gzip magic and caps the size. The
// submitter is the authenticated principal, never the body, and a submission the
// caller does not own is reported as 404, so this endpoint cannot upload to — or
// probe the existence of — another user's submission.
//
// The body carries its SHA-256 as Content-Digest (contentDigest); bytes that hash
// to anything else were changed on the way, and none of them are kept
// (submit.VerifyDigest).
//
// Uploading does not change the submission row (there is no "uploaded" column):
// the blob store is the presence source of truth, which admin approval consults.
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
	// Reserve the per-user upload cooldown BEFORE streaming. The body is the
	// expensive part (up to the 1 GiB blob cap), so without a reservation the
	// throttle would never bound the resource it exists for: a caller could
	// repeatedly start long uploads and abort them. Reserving also collapses the
	// lane's parallel overshoot — a burst of concurrent uploads from one user
	// yields exactly one admitted stream per replica. The rollback keeps a failed
	// upload (aborted transfer, wrong format, spent quota) from burning the
	// window, so a legit retry after a genuine failure is not punished.
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

// reserveUpload claims the per-user upload cooldown for userID, answering 429
// when an upload landed within it. commit keeps the reservation; release, run
// deferred, gives it back unless commit ran.
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

// handleContextUploadStatus reports how far the caller's chunked upload of a
// submission has come (app-tier, owner-scoped like the single upload). The
// panel reads it before the first part and again after a failed one, and sends
// the next part from received.
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

// handleContextUploadPart appends one part of the caller's chunked upload. The
// body is the raw bytes; ?offset= is where they start: 0 starts over, anything
// else must equal the staged length (409 upload_offset_mismatch otherwise). A
// part is small enough for any edge, so no cooldown applies here: the staged
// total is bounded by the context cap and the storage budget, and completion
// holds the cooldown. Each part carries its own Content-Digest, and one that
// does not hash to it is cut back off, as a part that breaks off is.
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

// handleContextUploadComplete stores the caller's staged upload as the
// submission's context. It holds the per-user upload cooldown and is audited
// like the single upload, since this is where a context lands.
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

// handleMySubmissions lists the caller's own submissions (app-tier). It scopes
// strictly to the principal's id; there is no parameter that could widen the
// query to another user's uploads. Each row is enriched with its linked build's
// outcome — this list is the only player-visible outlet for a build result, so
// a failed build is not invisible to the person who submitted it.
// SubmissionLimits is what one upload may carry, read before sending it.
type SubmissionLimits struct {
	MaxContextBytes int64 `json:"max_context_bytes"`
}

// handleSubmissionLimits reports the effective per-upload context cap
// ([registry] context_max_bytes), so the panel can refuse an oversized file
// before streaming it into the edge's own body limit.
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

// handleListSubmissions is the admin review queue: every submission across all
// users, newest first (admin-tier — it reads other users' uploads, so it gates
// on the admin Zero-Trust path via adminOnly). Rows carry the same build
// outcome enrichment as /me/submissions.
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

// submissionListOpts reads the page a submission list asks for: ?status= (exact),
// ?query= (id, submitter or name), ?limit= and ?offset=. An unparsable number
// reads as absent and the submit layer settles the bounds. Scope is never read
// from the request: each handler supplies it.
func submissionListOpts(r *http.Request) submit.ListOpts {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	return submit.ListOpts{
		Status: submit.Status(q.Get("status")), Query: q.Get("query"),
		Limit: limit, Offset: offset,
	}
}

// writeSubmissionPage renders one page: the enriched rows, how many match in all,
// and the scope's count in each status (every status present, zero or not, so the
// panel's summary cards never read a missing key).
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

// submissionView is one submission row enriched with its linked build's
// outcome. The row itself is embedded unchanged, so the wire shape only gains
// the two optional fields; they appear solely once a build has been linked
// (BuildID set) and its record is still readable.
type submissionView struct {
	submit.Submission
	BuildStatus string `json:"build_status,omitempty"`
	BuildError  string `json:"build_error,omitempty"`
}

// submissionViews enriches each submission with its linked build's status via
// one read-only Builder.GetMany for the whole page — deliberately never Sync,
// because the 15s reconcile loop owns state advance and rendering a list must not
// touch the cluster. A submission with no linked build (never approved, or
// approved before the hand-off could record the id), no Builder wired, or a build
// row that is gone renders without the extra fields; a store failure is returned
// so the handler reports it rather than silently dropping the outcome.
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
	a.audit(r, "submission.reject", sub.ID)
	writeJSON(w, http.StatusOK, sub)
}

// handleWithdrawSubmission retracts the caller's own pending submission
// (app-tier): the row and its uploaded context are deleted, freeing the pending
// slot and the storage budget for a fresh submission. The submitter is the
// principal, never the body; a submission the caller does not own is reported as
// 404, so this endpoint cannot probe or clear another user's uploads, and a
// reviewed submission is 409 (its build may already be consuming the context).
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

// handleDeleteSubmission retires any submission outright (admin-tier): the row
// and its uploaded context are removed, any status. This is the lane's lifecycle
// valve — the only path that reclaims a rejected or consumed upload from the
// uploads PVC. The reviewer identity goes to the audit event, not the (now
// nonexistent) row. Deleting an approved submission whose build is still running
// fails that build's context fetch; the admin has explicitly chosen to retire it.
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

// errSubmissionsUnavailable is returned when the approval lane is not configured
// on this api instance (a nil Submissions service), so the admin/app boundary is
// still exercised before the subsystem is wired in.
// uploadsStoreRetry is the Retry-After on uploads_store_unavailable.
const uploadsStoreRetry = 5 * time.Second

var errSubmissionsUnavailable = newError(http.StatusServiceUnavailable, "submissions_unavailable",
	"modpack submission subsystem is not configured")

// writeSubmitError maps submit-package errors onto HTTP status codes. Only the
// business sentinels are client-facing: a validation failure is 400, a missing
// submission is 404, an already-reviewed submission is 409, a spent per-user
// allowance is 403 (the same status the server-resource quota answers with), a
// full uploads store (every user's uploads together at their cap, or the volume
// short of free space) is 507, and an unconfigured upload transport is 503 (the store this deployment set has no
// implemented transport — an honest "not available here", not a client error). A
// blob store that did not answer the budget check is 503 uploads_store_unavailable
// with Retry-After: nothing was written and the same request can be sent again.
// Everything
// else — including a build.ErrInvalid raised by the pre-CAS build.Validate (a
// platform registry/context MISCONFIGURATION, never client input, since every
// build input is platform-derived) and a post-CAS Submit hand-off failure — is a
// server-side fault that collapses to 500 via writeError. The lane deliberately
// does not surface those as 4xx: the client did nothing wrong.
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
		// The budget could not be checked; nothing was written. The panel's upload
		// loop sends the same request again.
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

// handleInternalSubmissionContext streams a submission's stored build-context
// tarball to the build Pod's `felis fetch-context` initContainer. It lives on the
// internal face (service-token, no Zero Trust) because its only caller is
// in-cluster infrastructure: the build Job runs in the build namespace, where it
// can neither mount the uploads PVC nor hold object-store credentials, so the API
// — which wrote the blob — is the transport. The blob is served verbatim; the
// fetcher extracts it under a zip-slip guard, and Kaniko treats the result as
// hostile regardless (spec §16).
func (a *API) handleInternalSubmissionContext(w http.ResponseWriter, r *http.Request) {
	rc, digest, ok := a.openSubmissionContext(w, r)
	if !ok {
		return
	}
	streamSubmissionContext(w, r, rc, digest)
}

// handleAdminSubmissionContext streams a submission's stored build-context
// tarball to a reviewing admin (admin-tier). Review is only a real gate if the
// reviewer can inspect what they approve: the executed Dockerfile lives INSIDE
// this tarball (build/jobspec.go pins --dockerfile=Dockerfile), so without this
// route the human gate could not see the recipe at all. The bytes are the same
// ones the build Pod fetches over the internal face; the attachment disposition
// makes the browser download the attacker-supplied archive, never render it.
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

// openSubmissionContext resolves the build-context blob named in the request
// path, mapping the submit-layer errors onto the shared submission statuses (a
// missing blob is 404, an unwired transport 503). On failure the error response
// is already written and the caller must return.
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

// streamSubmissionContext copies the blob to w and closes it. The caller must
// have set every header already: the copy commits the response.
//
// With a recorded digest the final chunk is held back until the whole blob has
// been hashed. Bytes that do not match the row (a re-upload landed between the
// row read and the open) or a failed read abort the response instead of ending
// it cleanly, so neither the reviewer's download nor the build's fetch can take
// a prefix or a different blob for the recorded one.
func streamSubmissionContext(w http.ResponseWriter, r *http.Request, rc io.ReadCloser, digest string) {
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	if digest == "" {
		// A context uploaded before digests were kept: nothing to check against.
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

// Compile-time proof that the production Manager satisfies the API interface.
var _ SubmissionService = (*submit.Manager)(nil)
