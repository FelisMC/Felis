package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"felis.lolicon.best/internal/submit"
)

// fakeSubmissions is an in-memory SubmissionService for the handler tests. Each
// *Err field injects a canned outcome; the recorders let a test assert exactly
// what the handler forwarded (the point of the owner-scoping checks: the
// submitter and reviewer must come from the principal, never the body).
type fakeSubmissions struct {
	created    *submit.CreateRequest
	createErr  error
	listedBy   string
	byResult   []submit.Submission
	byErr      error
	listed     []submit.Submission
	listErr    error
	approvedID string
	approvedBy string
	approveErr error
	rejectedID string
	rejectedBy string
	rejectReas string
	rejectErr  error
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

func (f *fakeSubmissions) ListBy(_ context.Context, submittedBy string) ([]submit.Submission, error) {
	f.listedBy = submittedBy
	return f.byResult, f.byErr
}

func (f *fakeSubmissions) List(_ context.Context) ([]submit.Submission, error) {
	return f.listed, f.listErr
}

func (f *fakeSubmissions) Approve(_ context.Context, id, reviewedBy string) (*submit.Submission, error) {
	f.approvedID, f.approvedBy = id, reviewedBy
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
	var got map[string][]submit.Submission
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if len(got["submissions"]) != 1 {
		t.Fatalf("submissions = %d, want 1", len(got["submissions"]))
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
		{ID: "sub-2", SubmittedBy: "user-9", Status: submit.StatusApproved},
	}}
	api := adminSubAPI(fs)
	w := do(api.ExternalHandler(), "GET", "/api/v1/submissions", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var got map[string][]submit.Submission
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if len(got["submissions"]) != 2 {
		t.Fatalf("submissions = %d, want 2", len(got["submissions"]))
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
