package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/submit"
)

// fakeSubmissions is an in-memory SubmissionService for the handler tests. Each
// *Err field injects a canned outcome; the recorders let a test assert exactly
// what the handler forwarded (the point of the owner-scoping checks: the
// submitter and reviewer must come from the principal, never the body).
type fakeSubmissions struct {
	created     *submit.CreateRequest
	createErr   error
	uploadedID  string
	uploadedBy  string
	uploadedN   int64
	uploadErr   error
	listedBy    string
	byResult    []submit.Submission
	byErr       error
	listed      []submit.Submission
	listErr     error
	listOpts    submit.ListOpts // the last List or ListBy options
	pageTotal   int
	pageCounts  map[submit.Status]int
	approvedID  string
	approvedBy  string
	approveErr  error
	rejectedID  string
	rejectedBy  string
	rejectReas  string
	rejectErr   error
	withdrawnID string
	withdrawBy  string
	withdrawErr error
	deletedID   string
	deleteErr   error
	openedID    string
	openBody    string
	openErr     error

	approvedDigest string
	openDigest     string
}

func (f *fakeSubmissions) Create(_ context.Context, req submit.CreateRequest) (*submit.Submission, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	cp := req
	f.created = &cp
	return &submit.Submission{ID: "sub-1", SubmittedBy: req.SubmittedBy,
		DisplayName: req.DisplayName, Status: submit.StatusPendingReview}, nil
}

func (f *fakeSubmissions) UploadContext(_ context.Context, id, submittedBy string, r io.Reader) (*submit.Submission, error) {
	f.uploadedID, f.uploadedBy = id, submittedBy
	if f.uploadErr != nil {
		return nil, f.uploadErr
	}
	n, _ := io.Copy(io.Discard, r)
	f.uploadedN = n
	return &submit.Submission{ID: id, SubmittedBy: submittedBy, Status: submit.StatusPendingReview}, nil
}

func (f *fakeSubmissions) ListBy(_ context.Context, submittedBy string, opts submit.ListOpts) (submit.Page, error) {
	f.listedBy, f.listOpts = submittedBy, opts
	return submit.Page{Submissions: f.byResult, Total: f.pageTotal, Counts: f.pageCounts}, f.byErr
}

func (f *fakeSubmissions) List(_ context.Context, opts submit.ListOpts) (submit.Page, error) {
	f.listOpts = opts
	return submit.Page{Submissions: f.listed, Total: f.pageTotal, Counts: f.pageCounts}, f.listErr
}

func (f *fakeSubmissions) Approve(_ context.Context, id, reviewedBy, expectedDigest string) (*submit.Submission, error) {
	f.approvedID, f.approvedBy, f.approvedDigest = id, reviewedBy, expectedDigest
	if f.approveErr != nil {
		return nil, f.approveErr
	}
	return &submit.Submission{ID: id, Status: submit.StatusApproved, ReviewedBy: reviewedBy, BuildID: "bld-1"}, nil
}

func (f *fakeSubmissions) Reject(_ context.Context, id, reviewedBy, reason string) (*submit.Submission, error) {
	f.rejectedID, f.rejectedBy, f.rejectReas = id, reviewedBy, reason
	if f.rejectErr != nil {
		return nil, f.rejectErr
	}
	return &submit.Submission{ID: id, Status: submit.StatusRejected, ReviewedBy: reviewedBy, RejectReason: reason}, nil
}

func (f *fakeSubmissions) Withdraw(_ context.Context, id, submittedBy string) (*submit.Submission, error) {
	f.withdrawnID, f.withdrawBy = id, submittedBy
	if f.withdrawErr != nil {
		return nil, f.withdrawErr
	}
	return &submit.Submission{ID: id, SubmittedBy: submittedBy, Status: submit.StatusPendingReview}, nil
}

func (f *fakeSubmissions) Delete(_ context.Context, id string) (*submit.Submission, error) {
	f.deletedID = id
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &submit.Submission{ID: id, Status: submit.StatusRejected}, nil
}

// openErr injects the OpenContext outcome; the body recorder lets the internal
// route test assert byte-exact streaming and the 404 mapping.
func (f *fakeSubmissions) OpenContext(_ context.Context, id string) (io.ReadCloser, string, error) {
	f.openedID = id
	if f.openErr != nil {
		return nil, "", f.openErr
	}
	return io.NopCloser(strings.NewReader(f.openBody)), f.openDigest, nil
}

// appSubAPI wires a submissions service behind an ordinary user principal (the
// app tier — /me/submissions). user@example.net / .test are deliberately not the
// deployment domain.
func appSubAPI(s SubmissionService) *API {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.Submissions = s
	api.External = staticExternal{p: &Principal{UserID: "user-7", Email: "user@example.net", Role: "user"}}
	return api
}

// adminSubAPI wires a submissions service behind an admin principal (the admin
// tier — /submissions approve/reject/list).
func adminSubAPI(s SubmissionService) *API {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.Submissions = s
	api.External = staticExternal{p: &Principal{UserID: "admin-1", Email: "admin@example.net",
		Role: "admin", ViaAdminAccess: true}}
	return api
}

// The submitter is the principal, never the body: a valid create stamps the
// authenticated user's id onto the submission.
func TestCreateSubmissionStampsPrincipal(t *testing.T) {
	fs := &fakeSubmissions{}
	api := appSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/me/submissions", `{"display_name":"My Pack"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	if fs.created == nil {
		t.Fatal("Create was not called")
	}
	if fs.created.SubmittedBy != "user-7" {
		t.Errorf("submitted_by = %q, want the principal id user-7", fs.created.SubmittedBy)
	}
	if fs.created.DisplayName != "My Pack" {
		t.Errorf("display_name = %q", fs.created.DisplayName)
	}
}

// A client cannot smuggle a submitted_by through the body: decodeJSON rejects the
// unknown field with 400 and the handler never reaches Create.
func TestCreateSubmissionRejectsBodySubmittedBy(t *testing.T) {
	fs := &fakeSubmissions{}
	api := appSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/me/submissions",
		`{"display_name":"X","submitted_by":"someone-else"}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if fs.created != nil {
		t.Errorf("Create must not be called for an unknown-field body; got %+v", fs.created)
	}
}

// A validation failure from the submit layer surfaces as 400 bad_request.
func TestCreateSubmissionValidationIs400(t *testing.T) {
	fs := &fakeSubmissions{createErr: fmt.Errorf("%w: display name is required", submit.ErrInvalid)}
	api := appSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/me/submissions", `{"display_name":""}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if got := decodeErr(t, w); got != "bad_request" {
		t.Errorf("error code = %q, want bad_request", got)
	}
}

// The upload endpoint forwards the raw body to the transport and stamps the
// submitter from the principal, never the body — a user can only upload to a
// submission under their own identity.
func TestUploadSubmissionContextStreamsBody(t *testing.T) {
	fs := &fakeSubmissions{}
	api := appSubAPI(fs)
	// A tiny gzip-magic-prefixed body stands in for a real context.tar.gz.
	body := "\x1f\x8b\x08\x00 the modpack bytes"
	w := do(api.ExternalHandler(), "POST", "/api/v1/me/submissions/sub-9/context", body,
		map[string]string{"Content-Type": "application/gzip"})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if fs.uploadedID != "sub-9" {
		t.Errorf("uploaded id = %q, want sub-9", fs.uploadedID)
	}
	if fs.uploadedBy != "user-7" {
		t.Errorf("submitter = %q, want the principal id user-7", fs.uploadedBy)
	}
	if fs.uploadedN != int64(len(body)) {
		t.Errorf("streamed %d bytes, want %d", fs.uploadedN, len(body))
	}
}

// A submission the caller does not own reads back as 404 (the transport reports
// ErrNotFound), so the endpoint cannot probe another user's submission.
func TestUploadSubmissionContextNotOwnedIs404(t *testing.T) {
	fs := &fakeSubmissions{uploadErr: submit.ErrNotFound}
	api := appSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/me/submissions/sub-x/context", "\x1f\x8bdata", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

// Uploading to an already-reviewed submission is a 409.
func TestUploadSubmissionContextAlreadyReviewedIs409(t *testing.T) {
	fs := &fakeSubmissions{uploadErr: submit.ErrAlreadyReviewed}
	api := appSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/me/submissions/sub-9/context", "\x1f\x8bdata", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 (%s)", w.Code, w.Body.String())
	}
}

// A wrong-format / oversize body surfaces as 400 (the transport wraps ErrInvalid).
func TestUploadSubmissionContextBadFormatIs400(t *testing.T) {
	fs := &fakeSubmissions{uploadErr: fmt.Errorf("%w: build context must be a gzip-compressed tarball (.tar.gz)", submit.ErrInvalid)}
	api := appSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/me/submissions/sub-9/context", "not gzip", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if got := decodeErr(t, w); got != "bad_request" {
		t.Errorf("error code = %q, want bad_request", got)
	}
}

// When the deployment's store has no upload transport, the endpoint reports 503
// (ErrUploadsUnavailable) — an honest "not available here", not a 500.
func TestUploadSubmissionContextNoTransportIs503(t *testing.T) {
	fs := &fakeSubmissions{uploadErr: submit.ErrUploadsUnavailable}
	api := appSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/me/submissions/sub-9/context", "\x1f\x8bdata", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (%s)", w.Code, w.Body.String())
	}
	if got := decodeErr(t, w); got != "uploads_unavailable" {
		t.Errorf("error code = %q, want uploads_unavailable", got)
	}
}

// The upload route is app-tier: with no service configured it is 503, exactly
// like the other /me/submissions routes.
func TestUploadSubmissionContextWithoutServiceIs503(t *testing.T) {
	app := appSubAPI(nil)
	app.Submissions = nil
	w := do(app.ExternalHandler(), "POST", "/api/v1/me/submissions/sub-9/context", "\x1f\x8bdata", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (%s)", w.Code, w.Body.String())
	}
}

// Withdraw retracts the caller's OWN pending submission: the submitter is the
// principal (never the body), a reviewed submission is 409, and a foreign id is
// 404 — the same posture as the upload route.
func TestWithdrawSubmission(t *testing.T) {
	t.Run("withdraws as the principal", func(t *testing.T) {
		fs := &fakeSubmissions{}
		w := do(appSubAPI(fs).ExternalHandler(), "DELETE", "/api/v1/me/submissions/sub-3", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		if fs.withdrawnID != "sub-3" || fs.withdrawBy != "user-7" {
			t.Fatalf("withdraw forwarded (%q, %q), want (sub-3, user-7)", fs.withdrawnID, fs.withdrawBy)
		}
	})
	t.Run("reviewed submission is 409", func(t *testing.T) {
		fs := &fakeSubmissions{withdrawErr: submit.ErrAlreadyReviewed}
		w := do(appSubAPI(fs).ExternalHandler(), "DELETE", "/api/v1/me/submissions/sub-3", "", nil)
		if w.Code != http.StatusConflict {
			t.Fatalf("code = %d, want 409 (%s)", w.Code, w.Body.String())
		}
		if got := decodeErr(t, w); got != "already_reviewed" {
			t.Errorf("error code = %q, want already_reviewed", got)
		}
	})
	t.Run("foreign or unknown id is 404", func(t *testing.T) {
		fs := &fakeSubmissions{withdrawErr: submit.ErrNotFound}
		w := do(appSubAPI(fs).ExternalHandler(), "DELETE", "/api/v1/me/submissions/sub-x", "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404 (%s)", w.Code, w.Body.String())
		}
	})
	t.Run("no service is 503", func(t *testing.T) {
		app := appSubAPI(nil)
		app.Submissions = nil
		w := do(app.ExternalHandler(), "DELETE", "/api/v1/me/submissions/sub-3", "", nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("code = %d, want 503 (%s)", w.Code, w.Body.String())
		}
	})
}

// The admin delete retires any submission and maps the lane's 404; the route's
// admin gate itself is pinned by TestSubmissionAdminRoutesAreAdminOnly.
func TestDeleteSubmissionAdmin(t *testing.T) {
	t.Run("deletes the named row", func(t *testing.T) {
		fs := &fakeSubmissions{}
		w := do(adminSubAPI(fs).ExternalHandler(), "DELETE", "/api/v1/submissions/sub-8", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		if fs.deletedID != "sub-8" {
			t.Fatalf("delete forwarded id %q, want sub-8", fs.deletedID)
		}
	})
	t.Run("unknown is 404", func(t *testing.T) {
		fs := &fakeSubmissions{deleteErr: submit.ErrNotFound}
		w := do(adminSubAPI(fs).ExternalHandler(), "DELETE", "/api/v1/submissions/sub-x", "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404 (%s)", w.Code, w.Body.String())
		}
	})
}

// The "my uploads" list scopes strictly to the principal's id — there is no
// parameter that could widen it to another user's submissions.
func TestMySubmissionsScopesToPrincipal(t *testing.T) {
	fs := &fakeSubmissions{byResult: []submit.Submission{
		{ID: "sub-1", SubmittedBy: "user-7", DisplayName: "p", Status: submit.StatusPendingReview},
	}}
	api := appSubAPI(fs)
	w := do(api.ExternalHandler(), "GET", "/api/v1/me/submissions", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if fs.listedBy != "user-7" {
		t.Errorf("ListBy scoped to %q, want the principal id user-7", fs.listedBy)
	}
	var got struct {
		Submissions []submit.Submission `json:"submissions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if len(got.Submissions) != 1 {
		t.Fatalf("submissions = %d, want 1", len(got.Submissions))
	}
}

// The "my uploads" list carries the linked build's outcome — for a submitter it
// is the only visible outlet for a failed build (the /images/build routes are
// admin-tier). A row with no linked build gains no build fields.
func TestMySubmissionsCarriesBuildOutcome(t *testing.T) {
	fs := &fakeSubmissions{byResult: []submit.Submission{
		{ID: "sub-1", SubmittedBy: "user-7", Status: submit.StatusApproved, BuildID: "bld-9"},
		{ID: "sub-2", SubmittedBy: "user-7", Status: submit.StatusPendingReview},
	}}
	api := appSubAPI(fs)
	api.Builder = &fakeBuilder{getBuilds: map[string]*build.Build{
		"bld-9": {ID: "bld-9", Status: build.StatusFailed, Error: "build job failed or scan found a CRITICAL CVE"},
	}}
	w := do(api.ExternalHandler(), "GET", "/api/v1/me/submissions", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var got struct {
		Submissions []struct {
			ID          string `json:"id"`
			BuildStatus string `json:"build_status"`
			BuildError  string `json:"build_error"`
		} `json:"submissions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if len(got.Submissions) != 2 {
		t.Fatalf("submissions = %d, want 2", len(got.Submissions))
	}
	if got.Submissions[0].BuildStatus != "failed" || got.Submissions[0].BuildError == "" {
		t.Errorf("sub-1 outcome = %+v, want failed with the error text", got.Submissions[0])
	}
	if got.Submissions[1].BuildStatus != "" || got.Submissions[1].BuildError != "" {
		t.Errorf("sub-2 outcome = %+v, want no build fields without a linked build", got.Submissions[1])
	}
}

// A linked build whose row is gone renders as "no outcome" rather than failing
// the whole list; any other lookup failure must surface, never be swallowed.
func TestMySubmissionsBuildLookupSemantics(t *testing.T) {
	// Missing row (ErrNotFound): 200 with no build fields.
	fs := &fakeSubmissions{byResult: []submit.Submission{{ID: "sub-1", Status: submit.StatusApproved, BuildID: "bld-gone"}}}
	api := appSubAPI(fs)
	api.Builder = &fakeBuilder{}
	w := do(api.ExternalHandler(), "GET", "/api/v1/me/submissions", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("missing build row: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "build_status") {
		t.Errorf("missing build row: body carries build fields: %s", w.Body.String())
	}

	// Store fault: the failure is reported, not hidden behind a 200.
	fs = &fakeSubmissions{byResult: []submit.Submission{{ID: "sub-1", BuildID: "bld-1"}}}
	api = appSubAPI(fs)
	api.Builder = &fakeBuilder{getErr: errors.New("db down")}
	w = do(api.ExternalHandler(), "GET", "/api/v1/me/submissions", "", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("store fault: code = %d, want 500 (%s)", w.Code, w.Body.String())
	}
}

// Both lists forward the page the panel asked for and answer with the match total
// and every status count — zero included, so a summary card never reads a missing
// key. A number that does not parse reads as absent (the submit layer then applies
// its default); the scope never comes from the query string.
func TestSubmissionListsForwardThePage(t *testing.T) {
	fs := &fakeSubmissions{
		listed:     []submit.Submission{{ID: "sub-1", Status: submit.StatusApproved}},
		pageTotal:  41,
		pageCounts: map[submit.Status]int{submit.StatusApproved: 30, submit.StatusPendingReview: 11},
	}
	w := do(adminSubAPI(fs).ExternalHandler(), "GET",
		"/api/v1/submissions?status=approved&query=pack&limit=5&offset=10", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	want := submit.ListOpts{Status: "approved", Query: "pack", Limit: 5, Offset: 10}
	if fs.listOpts != want {
		t.Fatalf("admin list forwarded %+v, want %+v", fs.listOpts, want)
	}
	var got struct {
		Submissions []submit.Submission `json:"submissions"`
		Total       int                 `json:"total"`
		Counts      map[string]int      `json:"counts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if len(got.Submissions) != 1 || got.Total != 41 {
		t.Fatalf("page = %d rows, total %d; want 1 row, total 41", len(got.Submissions), got.Total)
	}
	wantCounts := map[string]int{"pending_review": 11, "approved": 30, "rejected": 0}
	if len(got.Counts) != 3 || got.Counts["pending_review"] != 11 || got.Counts["approved"] != 30 {
		t.Fatalf("counts = %v, want %v", got.Counts, wantCounts)
	}
	if n, ok := got.Counts["rejected"]; !ok || n != 0 {
		t.Fatalf("counts = %v, want rejected present as 0", got.Counts)
	}

	fs = &fakeSubmissions{}
	w = do(appSubAPI(fs).ExternalHandler(), "GET", "/api/v1/me/submissions?limit=many&offset=3&status=rejected", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("mine: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	want = submit.ListOpts{Status: "rejected", Offset: 3}
	if fs.listOpts != want || fs.listedBy != "user-7" {
		t.Fatalf("mine forwarded %+v for %q, want %+v for user-7", fs.listOpts, fs.listedBy, want)
	}
	if !strings.Contains(w.Body.String(), `"submissions":[]`) {
		t.Fatalf("an empty page must render an empty array: %s", w.Body.String())
	}
}

// A page's build outcomes come from one lookup naming every linked build; a page
// with no linked build asks nothing.
func TestSubmissionPageLooksUpBuildsOnce(t *testing.T) {
	fs := &fakeSubmissions{listed: []submit.Submission{
		{ID: "sub-1", Status: submit.StatusApproved, BuildID: "bld-1"},
		{ID: "sub-2", Status: submit.StatusPendingReview},
		{ID: "sub-3", Status: submit.StatusApproved, BuildID: "bld-3"},
	}}
	fb := &fakeBuilder{getBuilds: map[string]*build.Build{
		"bld-1": {ID: "bld-1", Status: build.StatusSucceeded},
		"bld-3": {ID: "bld-3", Status: build.StatusFailed, Error: "scan found a CRITICAL CVE"},
	}}
	api := adminSubAPI(fs)
	api.Builder = fb
	w := do(api.ExternalHandler(), "GET", "/api/v1/submissions", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if len(fb.getManyIDs) != 1 || strings.Join(fb.getManyIDs[0], ",") != "bld-1,bld-3" {
		t.Fatalf("build lookups = %v, want one naming bld-1,bld-3", fb.getManyIDs)
	}
	var got struct {
		Submissions []struct {
			ID          string `json:"id"`
			BuildStatus string `json:"build_status"`
			BuildError  string `json:"build_error"`
		} `json:"submissions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got.Submissions[0].BuildStatus != "succeeded" || got.Submissions[1].BuildStatus != "" ||
		got.Submissions[2].BuildStatus != "failed" || got.Submissions[2].BuildError != "scan found a CRITICAL CVE" {
		t.Fatalf("outcomes = %+v", got.Submissions)
	}

	fs.listed = []submit.Submission{{ID: "sub-2", Status: submit.StatusPendingReview}}
	fb.getManyIDs = nil
	if w := do(api.ExternalHandler(), "GET", "/api/v1/submissions", "", nil); w.Code != http.StatusOK {
		t.Fatalf("unlinked page: code = %d", w.Code)
	}
	if len(fb.getManyIDs) != 0 {
		t.Fatalf("a page with no linked build looked up %v", fb.getManyIDs)
	}
}

// Every /submissions route is admin-tier: a plain user is rejected before the
// handler runs.
func TestSubmissionAdminRoutesAreAdminOnly(t *testing.T) {
	cases := []struct {
		method, target, body string
	}{
		{"GET", "/api/v1/submissions", ""},
		{"POST", "/api/v1/submissions/sub-1/approve", ""},
		{"POST", "/api/v1/submissions/sub-1/reject", `{"reason":"no"}`},
		{"DELETE", "/api/v1/submissions/sub-1", ""},
		{"GET", "/api/v1/submissions/sub-1/context", ""},
	}
	for _, c := range cases {
		api := adminSubAPI(&fakeSubmissions{})
		// downgrade to a plain user
		api.External = staticExternal{p: &Principal{UserID: "u", Role: "user"}}
		w := do(api.ExternalHandler(), c.method, c.target, c.body, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s: code = %d, want 403", c.method, c.target, w.Code)
		}
	}
}

func TestListSubmissionsAdmin(t *testing.T) {
	fs := &fakeSubmissions{listed: []submit.Submission{
		{ID: "sub-1", SubmittedBy: "user-7", Status: submit.StatusPendingReview},
		{ID: "sub-2", SubmittedBy: "user-9", Status: submit.StatusApproved, BuildID: "bld-2"},
	}}
	api := adminSubAPI(fs)
	api.Builder = &fakeBuilder{getBuilds: map[string]*build.Build{
		"bld-2": {ID: "bld-2", Status: build.StatusSucceeded},
	}}
	w := do(api.ExternalHandler(), "GET", "/api/v1/submissions", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var got struct {
		Submissions []submit.Submission `json:"submissions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if len(got.Submissions) != 2 {
		t.Fatalf("submissions = %d, want 2", len(got.Submissions))
	}
	// The admin queue carries the same build outcome enrichment.
	if !strings.Contains(w.Body.String(), `"build_status":"succeeded"`) {
		t.Errorf("admin queue lacks the linked build outcome: %s", w.Body.String())
	}
}

// The reviewer is the admin principal's email, never client input.
func TestApproveSubmission(t *testing.T) {
	fs := &fakeSubmissions{}
	api := adminSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/submissions/sub-9/approve", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if fs.approvedID != "sub-9" {
		t.Errorf("approved id = %q, want sub-9", fs.approvedID)
	}
	if fs.approvedBy != "admin@example.net" {
		t.Errorf("reviewer = %q, want the admin email", fs.approvedBy)
	}
}

// The digest the reviewer inspected travels in the body, and a context replaced
// since then is a 409 the panel can act on.
func TestApproveSubmissionForwardsTheReviewedDigest(t *testing.T) {
	fs := &fakeSubmissions{}
	digest := strings.Repeat("ab", 32)
	w := do(adminSubAPI(fs).ExternalHandler(), "POST", "/api/v1/submissions/sub-9/approve",
		`{"expected_digest":"`+digest+`"}`, nil)
	if w.Code != http.StatusOK || fs.approvedDigest != digest {
		t.Fatalf("code = %d, forwarded digest %q (%s)", w.Code, fs.approvedDigest, w.Body.String())
	}

	fs = &fakeSubmissions{approveErr: submit.ErrContextChanged}
	w = do(adminSubAPI(fs).ExternalHandler(), "POST", "/api/v1/submissions/sub-9/approve",
		`{"expected_digest":"`+digest+`"}`, nil)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "context_changed" {
		t.Fatalf("code = %d body %s, want 409 context_changed", w.Code, w.Body.String())
	}

	w = do(adminSubAPI(&fakeSubmissions{}).ExternalHandler(), "POST", "/api/v1/submissions/sub-9/approve",
		`{"digest":"x"}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field code = %d, want 400", w.Code)
	}
}

func TestApproveSubmissionAlreadyReviewedIs409(t *testing.T) {
	fs := &fakeSubmissions{approveErr: submit.ErrAlreadyReviewed}
	api := adminSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/submissions/sub-9/approve", "", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if got := decodeErr(t, w); got != "already_reviewed" {
		t.Errorf("error code = %q, want already_reviewed", got)
	}
}

func TestApproveSubmissionNotFoundIs404(t *testing.T) {
	fs := &fakeSubmissions{approveErr: submit.ErrNotFound}
	api := adminSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/submissions/missing/approve", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

// A build hand-off failure post-approval is a server-side fault (the client did
// nothing wrong), so it collapses to 500 — never a 4xx.
func TestApproveBuildHandoffFailureIs500(t *testing.T) {
	fs := &fakeSubmissions{approveErr: fmt.Errorf("submit: approved but build hand-off failed: %w",
		errors.New("cluster unreachable"))}
	api := adminSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/submissions/sub-9/approve", "", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if got := decodeErr(t, w); got != "internal" {
		t.Errorf("error code = %q, want internal", got)
	}
}

func TestRejectSubmission(t *testing.T) {
	fs := &fakeSubmissions{}
	api := adminSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/submissions/sub-3/reject",
		`{"reason":"contains malware"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if fs.rejectedID != "sub-3" {
		t.Errorf("rejected id = %q, want sub-3", fs.rejectedID)
	}
	if fs.rejectedBy != "admin@example.net" {
		t.Errorf("reviewer = %q, want the admin email", fs.rejectedBy)
	}
	if fs.rejectReas != "contains malware" {
		t.Errorf("reason = %q", fs.rejectReas)
	}
}

func TestRejectSubmissionValidationIs400(t *testing.T) {
	fs := &fakeSubmissions{rejectErr: fmt.Errorf("%w: a reject reason is required", submit.ErrInvalid)}
	api := adminSubAPI(fs)
	w := do(api.ExternalHandler(), "POST", "/api/v1/submissions/sub-3/reject", `{"reason":""}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if got := decodeErr(t, w); got != "bad_request" {
		t.Errorf("error code = %q, want bad_request", got)
	}
}

// When no Submissions service is configured, the routes report 503 — the app
// route directly, and the admin route only AFTER the admin gate, so the boundary
// is still enforced first.
func TestSubmissionRoutesWithoutServiceAre503(t *testing.T) {
	// app tier
	app := appSubAPI(nil)
	app.Submissions = nil
	if w := do(app.ExternalHandler(), "GET", "/api/v1/me/submissions", "", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("app route: code = %d, want 503", w.Code)
	}
	// admin tier
	adm := adminSubAPI(nil)
	adm.Submissions = nil
	if w := do(adm.ExternalHandler(), "GET", "/api/v1/submissions", "", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("admin route: code = %d, want 503", w.Code)
	}
}

// The internal context route is the build Pod's only read path to a submission's
// blob: it streams the bytes verbatim, and its error mapping distinguishes a
// missing blob (404) from an unwired transport (503).
func TestInternalSubmissionContextRoute(t *testing.T) {
	newAPI := func(s SubmissionService) *API {
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.Internal = okInternal{caller: CallerBuild}
		api.Submissions = s
		return api
	}

	t.Run("streams the blob", func(t *testing.T) {
		fs := &fakeSubmissions{openBody: "\x1f\x8b\x08\x00blob"}
		w := do(newAPI(fs).InternalHandler(), "GET", "/api/v1/internal/submissions/sub-7/context", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if w.Body.String() != fs.openBody {
			t.Fatalf("body = %q, want the stored blob %q", w.Body.String(), fs.openBody)
		}
		if fs.openedID != "sub-7" {
			t.Fatalf("opened id = %q, want the path id", fs.openedID)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/gzip" {
			t.Fatalf("content-type = %q, want application/gzip", ct)
		}
	})

	t.Run("missing blob is 404", func(t *testing.T) {
		fs := &fakeSubmissions{openErr: fmt.Errorf("%w: gone", submit.ErrBlobNotFound)}
		w := do(newAPI(fs).InternalHandler(), "GET", "/api/v1/internal/submissions/sub-7/context", "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})

	t.Run("unwired transport is 503", func(t *testing.T) {
		w := do(newAPI(nil).InternalHandler(), "GET", "/api/v1/internal/submissions/sub-7/context", "", nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("code = %d, want 503", w.Code)
		}
	})
}

// The admin context route is the reviewer's read path to the blob they are
// approving: an admin streams the stored bytes with a download disposition,
// and the error mapping matches the internal route (404 missing, 503 unwired).
// The admin-only gate itself is pinned by TestSubmissionAdminRoutesAreAdminOnly.
func TestAdminSubmissionContextRoute(t *testing.T) {
	t.Run("streams the blob with a download disposition", func(t *testing.T) {
		fs := &fakeSubmissions{openBody: "\x1f\x8b\x08\x00blob"}
		w := do(adminSubAPI(fs).ExternalHandler(), "GET", "/api/v1/submissions/sub-7/context", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if w.Body.String() != fs.openBody {
			t.Fatalf("body = %q, want the stored blob %q", w.Body.String(), fs.openBody)
		}
		if fs.openedID != "sub-7" {
			t.Fatalf("opened id = %q, want the path id", fs.openedID)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/gzip" {
			t.Fatalf("content-type = %q, want application/gzip", ct)
		}
		if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="context.tar.gz"` {
			t.Fatalf("content-disposition = %q", cd)
		}
	})

	t.Run("names the recorded digest", func(t *testing.T) {
		body := "\x1f\x8b\x08\x00blob"
		sum := sha256.Sum256([]byte(body))
		fs := &fakeSubmissions{openBody: body, openDigest: hex.EncodeToString(sum[:])}
		w := do(adminSubAPI(fs).ExternalHandler(), "GET", "/api/v1/submissions/sub-7/context", "", nil)
		if w.Code != http.StatusOK || w.Body.String() != body {
			t.Fatalf("code = %d body %q", w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-Felis-Context-Sha256"); got != fs.openDigest {
			t.Fatalf("digest header = %q, want %q", got, fs.openDigest)
		}
	})

	// Bytes that are not the recorded ones must not arrive as a complete download:
	// the reviewer would inspect a blob the approval does not name.
	t.Run("aborts a blob that does not match", func(t *testing.T) {
		big := "\x1f\x8b" + strings.Repeat("x", 100<<10)
		fs := &fakeSubmissions{openBody: big, openDigest: strings.Repeat("0", 64)}
		srv := httptest.NewServer(adminSubAPI(fs).ExternalHandler())
		defer srv.Close()
		resp, err := http.Get(srv.URL + "/api/v1/submissions/sub-7/context")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		if err == nil {
			t.Fatalf("read %d bytes cleanly; want the response aborted", len(got))
		}
		if len(got) >= len(big) {
			t.Fatalf("received all %d bytes before the abort", len(got))
		}
	})

	t.Run("missing blob is 404", func(t *testing.T) {
		fs := &fakeSubmissions{openErr: fmt.Errorf("%w: gone", submit.ErrBlobNotFound)}
		w := do(adminSubAPI(fs).ExternalHandler(), "GET", "/api/v1/submissions/sub-7/context", "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})

	t.Run("unwired transport is 503", func(t *testing.T) {
		api := adminSubAPI(nil)
		api.Submissions = nil
		w := do(api.ExternalHandler(), "GET", "/api/v1/submissions/sub-7/context", "", nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("code = %d, want 503", w.Code)
		}
	})
}

// A spent per-user allowance is 403 submission_quota_exceeded on both the create
// and the upload path — distinctly NOT the 400 a malformed request gets, and not
// the 429 the cooldown answers with.
func TestSubmissionQuotaIs403(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		fs := &fakeSubmissions{createErr: fmt.Errorf("%w: 5 submissions are already awaiting review", submit.ErrQuotaExceeded)}
		w := do(appSubAPI(fs).ExternalHandler(), "POST", "/api/v1/me/submissions", `{"display_name":"Pack"}`, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403 (%s)", w.Code, w.Body.String())
		}
		if got := decodeErr(t, w); got != "submission_quota_exceeded" {
			t.Errorf("error code = %q, want submission_quota_exceeded", got)
		}
	})
	t.Run("upload", func(t *testing.T) {
		fs := &fakeSubmissions{uploadErr: fmt.Errorf("%w: exceeds your remaining storage allowance", submit.ErrQuotaExceeded)}
		w := do(appSubAPI(fs).ExternalHandler(), "POST", "/api/v1/me/submissions/sub-9/context", "\x1f\x8bdata", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403 (%s)", w.Code, w.Body.String())
		}
		if got := decodeErr(t, w); got != "submission_quota_exceeded" {
			t.Errorf("error code = %q, want submission_quota_exceeded", got)
		}
	})
}

// The per-user create cooldown bounds review-queue growth: a second create in
// the same window is 429 submission_cooldown and never reaches the service; the
// window recovers afterwards.
func TestCreateSubmissionRateLimited(t *testing.T) {
	fs := &fakeSubmissions{}
	api := appSubAPI(fs)
	clock := time.Unix(1_700_000_000, 0)
	api.Now = func() time.Time { return clock }
	api.SubmitCreateCooldown = time.Minute
	eh := api.ExternalHandler()

	if w := do(eh, "POST", "/api/v1/me/submissions", `{"display_name":"First"}`, nil); w.Code != http.StatusCreated {
		t.Fatalf("first create: code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	w := do(eh, "POST", "/api/v1/me/submissions", `{"display_name":"Second"}`, nil)
	if w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "submission_cooldown" {
		t.Fatalf("immediate second create: code = %d body %s, want 429 submission_cooldown", w.Code, w.Body.String())
	}
	// The gate sits before the body handling: even a malformed request is
	// refused while the window is closed, so it cannot be used to probe.
	if w := do(eh, "POST", "/api/v1/me/submissions", `{`, nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("malformed create during cooldown: code = %d, want 429", w.Code)
	}
	clock = clock.Add(time.Minute + time.Second)
	if w := do(eh, "POST", "/api/v1/me/submissions", `{"display_name":"Third"}`, nil); w.Code != http.StatusCreated {
		t.Fatalf("post-cooldown create: code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
}

// A failed create frees the window: only a row that was actually recorded burns
// the cooldown, so a validation typo is not punished with a wait.
func TestCreateSubmissionFailureDoesNotBurnCooldown(t *testing.T) {
	fs := &fakeSubmissions{createErr: fmt.Errorf("%w: display name is required", submit.ErrInvalid)}
	api := appSubAPI(fs)
	api.Now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	api.SubmitCreateCooldown = time.Minute
	eh := api.ExternalHandler()

	if w := do(eh, "POST", "/api/v1/me/submissions", `{"display_name":""}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("failed create: code = %d, want 400", w.Code)
	}
	fs.createErr = nil
	if w := do(eh, "POST", "/api/v1/me/submissions", `{"display_name":"Fixed"}`, nil); w.Code != http.StatusCreated {
		t.Fatalf("retry at the same instant: code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
}

// The per-user upload cooldown bounds context streaming: a second upload in the
// same window is 429 submission_cooldown, and a FAILED upload frees the window
// for an immediate retry.
func TestUploadSubmissionContextRateLimited(t *testing.T) {
	fs := &fakeSubmissions{}
	api := appSubAPI(fs)
	clock := time.Unix(1_700_000_000, 0)
	api.Now = func() time.Time { return clock }
	api.SubmitUploadCooldown = time.Minute
	eh := api.ExternalHandler()
	body := "\x1f\x8b\x08\x00 the modpack bytes"

	if w := do(eh, "POST", "/api/v1/me/submissions/sub-9/context", body, nil); w.Code != http.StatusOK {
		t.Fatalf("first upload: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	w := do(eh, "POST", "/api/v1/me/submissions/sub-9/context", body, nil)
	if w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "submission_cooldown" {
		t.Fatalf("immediate second upload: code = %d body %s, want 429 submission_cooldown", w.Code, w.Body.String())
	}

	// A failed upload releases its reservation, so the user is not punished for
	// a genuine failure (aborted transfer, spent quota) with a cooldown wait.
	fs2 := &fakeSubmissions{uploadErr: submit.ErrUploadsUnavailable}
	api2 := appSubAPI(fs2)
	api2.Now = func() time.Time { return clock }
	api2.SubmitUploadCooldown = time.Minute
	eh2 := api2.ExternalHandler()
	if w := do(eh2, "POST", "/api/v1/me/submissions/sub-9/context", body, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed upload: code = %d, want 503", w.Code)
	}
	fs2.uploadErr = nil
	if w := do(eh2, "POST", "/api/v1/me/submissions/sub-9/context", body, nil); w.Code != http.StatusOK {
		t.Fatalf("retry at the same instant after failure: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

type limitedSubmissions struct {
	*fakeSubmissions
	limit int64
}

func (l limitedSubmissions) ContextLimit() int64 { return l.limit }

// The panel reads the per-upload cap before sending a file; a lane that cannot
// report one answers like any unwired submissions endpoint.
func TestSubmissionLimitsReportsTheContextCap(t *testing.T) {
	api := appSubAPI(&fakeSubmissions{})
	api.Submissions = limitedSubmissions{&fakeSubmissions{}, 99614720}
	w := do(api.ExternalHandler(), "GET", "/api/v1/me/submissions/limits", "", nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"max_context_bytes":99614720}` {
		t.Fatalf("limits = %d %s", w.Code, w.Body.String())
	}

	api.Submissions = nil
	w = do(api.ExternalHandler(), "GET", "/api/v1/me/submissions/limits", "", nil)
	if w.Code != 503 {
		t.Fatalf("unwired limits = %d %s", w.Code, w.Body.String())
	}
}
