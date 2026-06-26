package submit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/build"
)

// testNow is the frozen clock for hermetic assertions.
var testNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const (
	testRegistry = "registry.felis.svc:5000"
	testContext  = "s3://felis-user-uploads"
)

// fakeStore is an in-memory Store with CAS semantics and error injection.
type fakeStore struct {
	subs map[string]*Submission

	createErr  error
	approveErr error
	rejectErr  error
	linkErr    error

	linked []string // "id=buildID" recorder
}

func newFakeStore() *fakeStore { return &fakeStore{subs: map[string]*Submission{}} }

func (f *fakeStore) CreateSubmission(_ context.Context, s *Submission) error {
	if f.createErr != nil {
		return f.createErr
	}
	cp := *s
	f.subs[s.ID] = &cp
	return nil
}

func (f *fakeStore) GetSubmission(_ context.Context, id string) (*Submission, error) {
	s, ok := f.subs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (f *fakeStore) ListSubmissions(_ context.Context) ([]Submission, error) {
	out := make([]Submission, 0, len(f.subs))
	for _, s := range f.subs {
		out = append(out, *s)
	}
	return out, nil
}

func (f *fakeStore) ListSubmissionsBy(_ context.Context, by string) ([]Submission, error) {
	var out []Submission
	for _, s := range f.subs {
		if s.SubmittedBy == by {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *fakeStore) ApproveSubmission(_ context.Context, id, reviewedBy, imageRef string, at time.Time) (bool, error) {
	if f.approveErr != nil {
		return false, f.approveErr
	}
	s, ok := f.subs[id]
	if !ok || s.Status != StatusPendingReview {
		return false, nil // CAS lost / nonexistent
	}
	s.Status = StatusApproved
	s.ImageRef = imageRef
	s.ReviewedBy = reviewedBy
	t := at
	s.ReviewedAt = &t
	return true, nil
}

func (f *fakeStore) RejectSubmission(_ context.Context, id, reviewedBy, reason string, at time.Time) (bool, error) {
	if f.rejectErr != nil {
		return false, f.rejectErr
	}
	s, ok := f.subs[id]
	if !ok || s.Status != StatusPendingReview {
		return false, nil
	}
	s.Status = StatusRejected
	s.ReviewedBy = reviewedBy
	s.RejectReason = reason
	t := at
	s.ReviewedAt = &t
	return true, nil
}

func (f *fakeStore) LinkBuild(_ context.Context, id, buildID string) error {
	if f.linkErr != nil {
		return f.linkErr
	}
	s, ok := f.subs[id]
	if !ok {
		return ErrNotFound
	}
	s.BuildID = buildID
	f.linked = append(f.linked, id+"="+buildID)
	return nil
}

// fakeBuilds records Submit calls and can inject a failure.
type fakeBuilds struct {
	submitErr error
	calls     int
	got       []build.Request
}

func (f *fakeBuilds) Submit(_ context.Context, req build.Request) (*build.Build, error) {
	f.calls++
	f.got = append(f.got, req)
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	return &build.Build{ID: "bld-x", ImageRef: req.ImageRef, Status: build.StatusBuilding}, nil
}

// newManager wires the fakes with a frozen clock and a deterministic lowercase
// id generator (sub-1, sub-2, ...).
func newManager() (*Manager, *fakeStore, *fakeBuilds) {
	st := newFakeStore()
	bl := &fakeBuilds{}
	n := 0
	m := &Manager{
		Store:        st,
		Builds:       bl,
		Registry:     testRegistry,
		ContextStore: testContext,
		Now:          func() time.Time { return testNow },
		IDGen:        func() string { n++; return "sub-" + string(rune('0'+n)) },
	}
	return m, st, bl
}

func TestCreatePendingDoesNotBuild(t *testing.T) {
	m, st, bl := newManager()

	sub, err := m.Create(context.Background(), CreateRequest{
		DisplayName: "My Modpack",
		SubmittedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sub.Status != StatusPendingReview {
		t.Fatalf("status = %q, want pending_review", sub.Status)
	}
	if sub.ID != "sub-1" {
		t.Fatalf("id = %q, want sub-1", sub.ID)
	}
	// Context ref is derived, never user-supplied.
	if want := testContext + "/sub-1/context.tar.gz"; sub.ContextRef != want {
		t.Fatalf("context_ref = %q, want %q", sub.ContextRef, want)
	}
	if sub.ImageRef != "" || sub.BuildID != "" {
		t.Fatalf("image/build set before approval: %+v", sub)
	}
	if bl.calls != 0 {
		t.Fatalf("Create started %d builds, want 0", bl.calls)
	}
	if _, ok := st.subs["sub-1"]; !ok {
		t.Fatal("submission not persisted")
	}
}

func TestCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		req  CreateRequest
	}{
		{"empty name", CreateRequest{DisplayName: "  ", SubmittedBy: "user-1"}},
		{"oversize name", CreateRequest{DisplayName: strings.Repeat("a", maxDisplayName+1), SubmittedBy: "user-1"}},
		{"control chars", CreateRequest{DisplayName: "bad\nname", SubmittedBy: "user-1"}},
		{"no submitter", CreateRequest{DisplayName: "ok", SubmittedBy: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, st, bl := newManager()
			_, err := m.Create(context.Background(), tc.req)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if len(st.subs) != 0 || bl.calls != 0 {
				t.Fatal("invalid Create persisted or built")
			}
		})
	}
}

func TestApproveStartsExactlyOneBuildAndLinks(t *testing.T) {
	m, st, bl := newManager()
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	sub, err := m.Approve(context.Background(), seed.ID, "admin@example.test")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if sub.Status != StatusApproved {
		t.Fatalf("status = %q, want approved", sub.Status)
	}
	wantImage := testRegistry + "/user-uploads/sub-1:latest"
	if sub.ImageRef != wantImage {
		t.Fatalf("image_ref = %q, want %q", sub.ImageRef, wantImage)
	}
	if sub.BuildID != "bld-x" {
		t.Fatalf("build_id = %q, want bld-x", sub.BuildID)
	}
	if sub.ReviewedBy != "admin@example.test" || sub.ReviewedAt == nil {
		t.Fatalf("review metadata missing: %+v", sub)
	}
	// Exactly one build, carrying the derived refs + reviewer + audit Dockerfile.
	if bl.calls != 1 {
		t.Fatalf("builds started = %d, want 1", bl.calls)
	}
	req := bl.got[0]
	if req.ImageRef != wantImage {
		t.Fatalf("build ImageRef = %q, want %q", req.ImageRef, wantImage)
	}
	if req.ContextRef != seed.ContextRef {
		t.Fatalf("build ContextRef = %q, want %q", req.ContextRef, seed.ContextRef)
	}
	if req.RequestedBy != "admin@example.test" {
		t.Fatalf("build RequestedBy = %q, want admin", req.RequestedBy)
	}
	if strings.TrimSpace(req.Dockerfile) == "" {
		t.Fatal("audit Dockerfile must be non-empty (build.Validate requires it)")
	}
	// The persisted row links the build.
	if got := st.subs[seed.ID]; got.BuildID != "bld-x" || got.Status != StatusApproved {
		t.Fatalf("persisted row not linked/approved: %+v", got)
	}
	if len(st.linked) != 1 {
		t.Fatalf("LinkBuild called %d times, want 1", len(st.linked))
	}
}

func TestApproveDerivedRefValidates(t *testing.T) {
	// The derived push target must satisfy the build subsystem's own admission
	// rules, else Approve would CAS then fail at Submit. Prove it against the SAME
	// validator the Builder uses.
	m, _, _ := newManager()
	ref := m.deriveImageRef("sub-1")
	req := build.Request{
		ImageRef:   ref,
		Dockerfile: auditDockerfile("sub-1", m.deriveContextRef("sub-1")),
		ContextRef: m.deriveContextRef("sub-1"),
	}
	if err := build.Validate(req, build.Config{RegistryURL: testRegistry}); err != nil {
		t.Fatalf("derived build request does not validate: %v", err)
	}
	if !strings.HasPrefix(ref, testRegistry+"/user-uploads/") {
		t.Fatalf("derived ref %q is not in the user-uploads namespace", ref)
	}
}

func TestApproveIsCASNoDoubleBuild(t *testing.T) {
	m, _, bl := newManager()
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	if _, err := m.Approve(context.Background(), seed.ID, "admin@x"); err != nil {
		t.Fatalf("first Approve: %v", err)
	}
	// A second approve loses the CAS and must NOT start another build.
	_, err := m.Approve(context.Background(), seed.ID, "admin@x")
	if !errors.Is(err, ErrAlreadyReviewed) {
		t.Fatalf("second Approve err = %v, want ErrAlreadyReviewed", err)
	}
	if bl.calls != 1 {
		t.Fatalf("builds started = %d, want 1 (no double-build)", bl.calls)
	}
}

func TestApproveRejectedSubmission(t *testing.T) {
	m, _, bl := newManager()
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	if _, err := m.Reject(context.Background(), seed.ID, "admin@x", "nope"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	_, err := m.Approve(context.Background(), seed.ID, "admin@x")
	if !errors.Is(err, ErrAlreadyReviewed) {
		t.Fatalf("Approve after reject err = %v, want ErrAlreadyReviewed", err)
	}
	if bl.calls != 0 {
		t.Fatalf("builds started = %d, want 0", bl.calls)
	}
}

func TestApproveUnknown(t *testing.T) {
	m, _, _ := newManager()
	_, err := m.Approve(context.Background(), "sub-nope", "admin@x")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestApproveBuildFailureLeavesApprovedUnlinked(t *testing.T) {
	// The post-CAS Submit failure is the documented KNOWN-LIMITATION: the row is
	// left approved with build_id NULL, recoverable by an admin via the direct
	// build path. It must NOT roll back to pending or double-count.
	m, st, bl := newManager()
	bl.submitErr = errors.New("apiserver unreachable")
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	_, err := m.Approve(context.Background(), seed.ID, "admin@x")
	if err == nil {
		t.Fatal("Approve should surface the build hand-off failure")
	}
	if bl.calls != 1 {
		t.Fatalf("builds attempted = %d, want 1", bl.calls)
	}
	got := st.subs[seed.ID]
	if got.Status != StatusApproved {
		t.Fatalf("status = %q, want approved (CAS already committed)", got.Status)
	}
	if got.BuildID != "" {
		t.Fatalf("build_id = %q, want empty (Submit failed before LinkBuild)", got.BuildID)
	}
	if len(st.linked) != 0 {
		t.Fatal("LinkBuild must not run when Submit failed")
	}
}

func TestApproveLinkFailureNamesRunningBuild(t *testing.T) {
	// The WORSE post-CAS partial state: Submit SUCCEEDS (a build is genuinely
	// running and will push to image_ref) but the follow-up LinkBuild write fails,
	// so the row is approved with build_id still NULL. On the row this is
	// indistinguishable from the benign Submit-fail case above, so a blind re-drive
	// would double-build/double-push to :latest. Approve must therefore return a
	// DISTINCT error that names the running build id and forbids re-submission —
	// this exercises that branch (the benign test above does not).
	m, st, bl := newManager()
	st.linkErr = errors.New("db write timeout")
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	_, err := m.Approve(context.Background(), seed.ID, "admin@x")
	if err == nil {
		t.Fatal("Approve must surface the link-write failure (a build is running)")
	}
	// A build WAS started — that is precisely why re-driving is unsafe.
	if bl.calls != 1 {
		t.Fatalf("builds started = %d, want 1 (Submit succeeded, build is running)", bl.calls)
	}
	// The error must name the running build id and the do-not-resubmit warning, and
	// wrap the underlying store error so callers can still inspect the cause.
	if !strings.Contains(err.Error(), "bld-x") {
		t.Fatalf("error must name the running build id bld-x: %v", err)
	}
	if !strings.Contains(err.Error(), "do NOT re-submit") {
		t.Fatalf("error must warn against re-submission: %v", err)
	}
	if !errors.Is(err, st.linkErr) {
		t.Fatalf("error must wrap the underlying link failure: %v", err)
	}
	// The row is approved (CAS committed) but build_id stays empty — the very
	// ambiguity the distinct error compensates for.
	got := st.subs[seed.ID]
	if got.Status != StatusApproved {
		t.Fatalf("status = %q, want approved (CAS already committed)", got.Status)
	}
	if got.BuildID != "" {
		t.Fatalf("build_id = %q, want empty (LinkBuild failed to record it)", got.BuildID)
	}
	if len(st.linked) != 0 {
		t.Fatal("LinkBuild errored before recording — must not appear linked")
	}
}

func TestApproveValidatesBeforeCAS(t *testing.T) {
	// A misconfigured registry makes the derived ref fail build.Validate. That
	// deterministic failure must happen BEFORE the CAS, so the row stays pending
	// and no build is attempted — never a stranded approved row.
	m, st, bl := newManager()
	m.Registry = "" // derived ref becomes "/user-uploads/...", not host-qualified
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	_, err := m.Approve(context.Background(), seed.ID, "admin@x")
	if !errors.Is(err, build.ErrInvalid) {
		t.Fatalf("err = %v, want build.ErrInvalid (pre-CAS validation)", err)
	}
	if got := st.subs[seed.ID]; got.Status != StatusPendingReview {
		t.Fatalf("status = %q, want still pending_review (CAS not reached)", got.Status)
	}
	if bl.calls != 0 {
		t.Fatalf("builds started = %d, want 0", bl.calls)
	}
}

func TestRejectDoesNotBuild(t *testing.T) {
	m, st, bl := newManager()
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	sub, err := m.Reject(context.Background(), seed.ID, "admin@x", "ships a coin miner")
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if sub.Status != StatusRejected || sub.RejectReason != "ships a coin miner" {
		t.Fatalf("reject verdict not recorded: %+v", sub)
	}
	if sub.ReviewedBy != "admin@x" || sub.ReviewedAt == nil {
		t.Fatalf("review metadata missing: %+v", sub)
	}
	if bl.calls != 0 {
		t.Fatalf("builds started = %d, want 0", bl.calls)
	}
	if st.subs[seed.ID].Status != StatusRejected {
		t.Fatal("rejection not persisted")
	}
}

func TestRejectRequiresReason(t *testing.T) {
	m, st, _ := newManager()
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	_, err := m.Reject(context.Background(), seed.ID, "admin@x", "   ")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if got := st.subs[seed.ID]; got.Status != StatusPendingReview {
		t.Fatalf("status = %q, want pending_review", got.Status)
	}
}

func TestListBy(t *testing.T) {
	m, _, _ := newManager()
	if _, err := m.Create(context.Background(), CreateRequest{DisplayName: "A", SubmittedBy: "user-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), CreateRequest{DisplayName: "B", SubmittedBy: "user-2"}); err != nil {
		t.Fatal(err)
	}
	mine, err := m.ListBy(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("ListBy: %v", err)
	}
	if len(mine) != 1 || mine[0].SubmittedBy != "user-1" {
		t.Fatalf("ListBy scoped wrong: %+v", mine)
	}
	if _, err := m.ListBy(context.Background(), ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty submitter err = %v, want ErrInvalid", err)
	}
}
