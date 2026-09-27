package submit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/build"
)

// gzBody returns a minimal gzip-magic-prefixed blob standing in for a real
// context.tar.gz: UploadContext only sniffs the first two bytes, so the payload
// after the magic is opaque.
func gzBody(payload string) string { return "\x1f\x8b\x08\x00" + payload }

// fakeBlobs is an in-memory Blobs transport. It records what was stored so a test
// can assert the derived id was used, and can force Exists/Put outcomes.
type fakeBlobs struct {
	stored      map[string][]byte
	putErr      error
	existsErr   error
	sizeErr     error
	deleteErr   error
	forceExists *bool // overrides the stored-map lookup for the approve-gate tests
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{stored: map[string][]byte{}} }

func (f *fakeBlobs) Put(_ context.Context, id string, r io.Reader) (int64, error) {
	if f.putErr != nil {
		return 0, f.putErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err // e.g. cappedReader tripping — persist nothing, mirror the real store
	}
	f.stored[id] = b
	return int64(len(b)), nil
}

func (f *fakeBlobs) Exists(_ context.Context, id string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	if f.forceExists != nil {
		return *f.forceExists, nil
	}
	_, ok := f.stored[id]
	return ok, nil
}

// Size mirrors the real stores: a missing blob is (0, false, nil).
func (f *fakeBlobs) Size(_ context.Context, id string) (int64, bool, error) {
	if f.sizeErr != nil {
		return 0, false, f.sizeErr
	}
	b, ok := f.stored[id]
	if !ok {
		return 0, false, nil
	}
	return int64(len(b)), true, nil
}

// Delete mirrors the real stores: idempotent, nothing stored is success.
func (f *fakeBlobs) Delete(_ context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.stored, id)
	return nil
}

func (f *fakeBlobs) Open(_ context.Context, id string) (io.ReadCloser, error) {
	b, ok := f.stored[id]
	if !ok {
		return nil, fmt.Errorf("%w: no blob for %s", ErrBlobNotFound, id)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

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
	countErr   error
	approveErr error
	rejectErr  error
	linkErr    error
	deleteErr  error

	linked   []string // "id=buildID" recorder
	pageOpts ListOpts // the last PageSubmissions call

	// beforeApprove runs between the Manager's read and its CAS, the window a
	// concurrent re-upload lands in.
	beforeApprove func()
}

func newFakeStore() *fakeStore { return &fakeStore{subs: map[string]*Submission{}} }

func (f *fakeStore) CreateSubmission(_ context.Context, s *Submission, maxPending int) (int, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}
	var n int
	for _, x := range f.subs {
		if x.SubmittedBy == s.SubmittedBy && x.Status == StatusPendingReview {
			n++
		}
	}
	if n >= maxPending {
		return n, ErrQuotaExceeded
	}
	if f.createErr != nil {
		return n, f.createErr
	}
	cp := *s
	f.subs[s.ID] = &cp
	return n, nil
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

// PageSubmissions records the options the Manager settled on and scopes by
// submitter; the SQL filters and paging are pgint's to prove.
func (f *fakeStore) PageSubmissions(_ context.Context, opts ListOpts) (Page, error) {
	f.pageOpts = opts
	page := Page{Counts: map[Status]int{}}
	for _, s := range f.subs {
		if opts.SubmittedBy == "" || s.SubmittedBy == opts.SubmittedBy {
			page.Submissions = append(page.Submissions, *s)
			page.Counts[s.Status]++
		}
	}
	page.Total = len(page.Submissions)
	return page, nil
}

func (f *fakeStore) ApproveSubmission(_ context.Context, id, reviewedBy, imageRef, digest string, at time.Time) (bool, error) {
	if f.beforeApprove != nil {
		f.beforeApprove()
	}
	if f.approveErr != nil {
		return false, f.approveErr
	}
	s, ok := f.subs[id]
	if !ok || s.Status != StatusPendingReview || s.ContextSHA256 != digest {
		return false, nil // CAS lost / nonexistent
	}
	s.Status = StatusApproved
	s.ImageRef = imageRef
	s.ReviewedBy = reviewedBy
	t := at
	s.ReviewedAt = &t
	return true, nil
}

func (f *fakeStore) SetContextDigest(_ context.Context, id, digest string) (bool, error) {
	s, ok := f.subs[id]
	if !ok || s.Status != StatusPendingReview {
		return false, nil
	}
	s.ContextSHA256 = digest
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

func (f *fakeStore) DeleteSubmission(_ context.Context, id string) (bool, error) {
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	if _, ok := f.subs[id]; !ok {
		return false, nil // concurrent delete won / nonexistent
	}
	delete(f.subs, id)
	return true, nil
}

func (f *fakeStore) DeletePendingSubmission(_ context.Context, id, by string) (bool, error) {
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	s, ok := f.subs[id]
	if !ok || s.SubmittedBy != by || s.Status != StatusPendingReview {
		return false, nil
	}
	delete(f.subs, id)
	return true, nil
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

// With a ContextBaseURL the derived ref is the internal-face URL the build Pod's
// fetch initContainer dials — not a filesystem path it could never read across
// namespaces. OpenContext then serves whatever the Blobs transport stored.
func TestContextRefIsFetchURLAndOpenContextServesIt(t *testing.T) {
	m, _, _ := newManager()
	m.ContextBaseURL = "http://felis-api-internal.felis.svc.cluster.local:8081/"
	m.Blobs = newFakeBlobs()
	ctx := context.Background()

	sub, err := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := "http://felis-api-internal.felis.svc.cluster.local:8081/api/v1/internal/submissions/sub-1/context"
	if sub.ContextRef != want {
		t.Fatalf("context_ref = %q, want the internal fetch URL %q", sub.ContextRef, want)
	}

	// Before any upload the read path reports not-found (the route's 404).
	if _, _, err := m.OpenContext(ctx, sub.ID); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("OpenContext before upload = %v, want ErrBlobNotFound", err)
	}
	payload := "\x1f\x8b\x08\x00payload"
	if _, err := m.UploadContext(ctx, sub.ID, "user-1", strings.NewReader(payload)); err != nil {
		t.Fatalf("UploadContext: %v", err)
	}
	rc, digest, err := m.OpenContext(ctx, sub.ID)
	if err != nil {
		t.Fatalf("OpenContext: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != payload {
		t.Fatalf("OpenContext served %q, want %q", got, payload)
	}
	if digest != sha256Hex(payload) {
		t.Fatalf("OpenContext digest = %q, want the payload's sha256", digest)
	}
}

// No upload transport ⇒ no readable blob: the route reports the same 503 the
// upload endpoint does, rather than a misleading 404.
func TestOpenContextWithoutTransportIsUnavailable(t *testing.T) {
	m, _, _ := newManager()
	if _, _, err := m.OpenContext(context.Background(), "sub-1"); !errors.Is(err, ErrUploadsUnavailable) {
		t.Fatalf("OpenContext with nil Blobs = %v, want ErrUploadsUnavailable", err)
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

	sub, err := m.Approve(context.Background(), seed.ID, "admin@example.test", "")
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

	if _, err := m.Approve(context.Background(), seed.ID, "admin@x", ""); err != nil {
		t.Fatalf("first Approve: %v", err)
	}
	// A second approve loses the CAS and must NOT start another build.
	_, err := m.Approve(context.Background(), seed.ID, "admin@x", "")
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
	_, err := m.Approve(context.Background(), seed.ID, "admin@x", "")
	if !errors.Is(err, ErrAlreadyReviewed) {
		t.Fatalf("Approve after reject err = %v, want ErrAlreadyReviewed", err)
	}
	if bl.calls != 0 {
		t.Fatalf("builds started = %d, want 0", bl.calls)
	}
}

func TestApproveUnknown(t *testing.T) {
	m, _, _ := newManager()
	_, err := m.Approve(context.Background(), "sub-nope", "admin@x", "")
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

	_, err := m.Approve(context.Background(), seed.ID, "admin@x", "")
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

	_, err := m.Approve(context.Background(), seed.ID, "admin@x", "")
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

	_, err := m.Approve(context.Background(), seed.ID, "admin@x", "")
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
	// The scope is the argument: a SubmittedBy smuggled into opts cannot widen it.
	mine, err := m.ListBy(context.Background(), "user-1", ListOpts{SubmittedBy: "user-2"})
	if err != nil {
		t.Fatalf("ListBy: %v", err)
	}
	if len(mine.Submissions) != 1 || mine.Submissions[0].SubmittedBy != "user-1" || mine.Total != 1 {
		t.Fatalf("ListBy scoped wrong: %+v", mine)
	}
	if _, err := m.ListBy(context.Background(), "", ListOpts{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty submitter err = %v, want ErrInvalid", err)
	}
}

// List is the whole lane whatever opts names, and both lists settle the page the
// same way: a missing limit takes the default, an oversized one the cap, a
// negative offset zero, and the query loses its padding.
func TestListPagingOptions(t *testing.T) {
	m, st, _ := newManager()
	for _, by := range []string{"user-1", "user-2"} {
		if _, err := m.Create(context.Background(), CreateRequest{DisplayName: "P", SubmittedBy: by}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := m.List(context.Background(), ListOpts{SubmittedBy: "user-1", Query: "  pack  ", Offset: -5})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if all.Total != 2 {
		t.Fatalf("List total = %d, want 2 (the admin queue ignores a submitter in opts)", all.Total)
	}
	want := ListOpts{Query: "pack", Limit: 20, Offset: 0}
	if st.pageOpts != want {
		t.Fatalf("List settled %+v, want %+v", st.pageOpts, want)
	}
	if _, err := m.ListBy(context.Background(), "user-2", ListOpts{Limit: 5000, Offset: 40, Status: StatusApproved}); err != nil {
		t.Fatalf("ListBy: %v", err)
	}
	want = ListOpts{SubmittedBy: "user-2", Status: StatusApproved, Limit: 100, Offset: 40}
	if st.pageOpts != want {
		t.Fatalf("ListBy settled %+v, want %+v", st.pageOpts, want)
	}
	if _, err := m.List(context.Background(), ListOpts{Status: "building"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown status err = %v, want ErrInvalid", err)
	}
}

func TestUploadContextStoresUnderDerivedID(t *testing.T) {
	m, _, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	payload := gzBody("the modpack bytes")
	sub, err := m.UploadContext(context.Background(), seed.ID, "user-1", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("UploadContext: %v", err)
	}
	if sub.ID != seed.ID {
		t.Fatalf("returned submission %q, want %q", sub.ID, seed.ID)
	}
	// The blob is stored under the submission id (the pinned, id-namespaced key),
	// never a caller-supplied path.
	got, ok := fb.stored[seed.ID]
	if !ok {
		t.Fatalf("nothing stored under id %q; stored keys: %v", seed.ID, keysOf(fb.stored))
	}
	if string(got) != payload {
		t.Fatalf("stored %q, want the uploaded bytes", got)
	}
}

func TestUploadContextRejectsNonGzip(t *testing.T) {
	m, _, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	_, err := m.UploadContext(context.Background(), seed.ID, "user-1", strings.NewReader("PK\x03\x04 a zip, not gzip"))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if len(fb.stored) != 0 {
		t.Fatal("a wrong-format upload must persist nothing")
	}
}

func TestUploadContextOversizeRejectedAndNotPersisted(t *testing.T) {
	m, _, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	m.MaxContextBytes = 8 // tiny cap
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	// gzBody's 4-byte magic + payload well over 8 bytes total.
	_, err := m.UploadContext(context.Background(), seed.ID, "user-1", strings.NewReader(gzBody("this is far too large")))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid (too large)", err)
	}
	if len(fb.stored) != 0 {
		t.Fatal("an oversize upload must persist nothing")
	}
}

func TestUploadContextExactlyAtCapAccepted(t *testing.T) {
	m, _, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	body := gzBody("payload") // measure and cap at exactly this length
	m.MaxContextBytes = int64(len(body))
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	if _, err := m.UploadContext(context.Background(), seed.ID, "user-1", strings.NewReader(body)); err != nil {
		t.Fatalf("a blob of exactly the cap must be accepted, got %v", err)
	}
	if string(fb.stored[seed.ID]) != body {
		t.Fatalf("stored %q, want the full body (no truncation at the cap)", fb.stored[seed.ID])
	}
}

func TestUploadContextWrongOwnerIsNotFound(t *testing.T) {
	m, _, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	// A different user uploading to user-1's submission sees 404, not 403: the id
	// is invisible so it cannot be probed.
	_, err := m.UploadContext(context.Background(), seed.ID, "user-2", strings.NewReader(gzBody("x")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(fb.stored) != 0 {
		t.Fatal("a non-owner upload must persist nothing")
	}
}

func TestUploadContextNotPendingIsAlreadyReviewed(t *testing.T) {
	m, _, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	if _, err := m.Reject(context.Background(), seed.ID, "admin@x", "nope"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	_, err := m.UploadContext(context.Background(), seed.ID, "user-1", strings.NewReader(gzBody("x")))
	if !errors.Is(err, ErrAlreadyReviewed) {
		t.Fatalf("err = %v, want ErrAlreadyReviewed (context is frozen once reviewed)", err)
	}
}

func TestUploadContextUnknownSubmission(t *testing.T) {
	m, _, _ := newManager()
	m.Blobs = newFakeBlobs()
	_, err := m.UploadContext(context.Background(), "sub-nope", "user-1", strings.NewReader(gzBody("x")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUploadContextNoTransportUnavailable(t *testing.T) {
	m, _, _ := newManager() // Blobs left nil
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	_, err := m.UploadContext(context.Background(), seed.ID, "user-1", strings.NewReader(gzBody("x")))
	if !errors.Is(err, ErrUploadsUnavailable) {
		t.Fatalf("err = %v, want ErrUploadsUnavailable", err)
	}
}

// The per-user pending cap bounds the review queue: at the limit a new create is
// refused with ErrQuotaExceeded (403), a reviewed row frees a slot, and another
// user's queue is unaffected.
func TestCreatePendingCapRejects(t *testing.T) {
	m, _, _ := newManager()
	m.MaxPendingPerUser = 2
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"}); err != nil {
			t.Fatalf("create %d: %v", i+1, err)
		}
	}
	_, err := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("create past the cap = %v, want ErrQuotaExceeded", err)
	}

	// A verdict moves the row out of pending_review, so the slot frees up.
	if _, err := m.Reject(ctx, "sub-1", "admin@example.net", "out of scope"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if _, err := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"}); err != nil {
		t.Fatalf("create after review: %v", err)
	}

	// The cap is per user, never global.
	if _, err := m.Create(ctx, CreateRequest{DisplayName: "Other", SubmittedBy: "user-2"}); err != nil {
		t.Fatalf("other user's first create: %v", err)
	}
}

// The per-user storage budget bounds the sum of a user's stored contexts. It is
// charged against the blob store's actual sizes, refuses at the boundary with
// ErrQuotaExceeded (403 — not the 400 a single oversize blob gets), and does not
// double-charge a re-upload of the same submission.
func TestUploadContextStorageBudget(t *testing.T) {
	m, _, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	m.MaxStoredBytesPerUser = 10 // tiny budget; gzBody is 4 magic bytes + payload
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{DisplayName: "A", SubmittedBy: "user-1"})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	b, err := m.Create(ctx, CreateRequest{DisplayName: "B", SubmittedBy: "user-1"})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	if _, err := m.UploadContext(ctx, a.ID, "user-1", strings.NewReader(gzBody("a"))); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	// A's 5 bytes leave 5. B's exactly-5-byte blob must be accepted — the budget
	// binds only past the limit, never at it.
	if _, err := m.UploadContext(ctx, b.ID, "user-1", strings.NewReader(gzBody("b"))); err != nil {
		t.Fatalf("upload exactly at the budget = %v, want accepted", err)
	}
	if _, err := m.UploadContext(ctx, a.ID, "user-1", strings.NewReader(gzBody("far too much"))); err != nil {
		// A re-upload is charged only for its NEW bytes (its old blob is
		// superseded), and 16 bytes exceed the 5 bytes left after B.
		if !errors.Is(err, ErrQuotaExceeded) {
			t.Fatalf("re-upload past the budget = %v, want ErrQuotaExceeded", err)
		}
	} else {
		t.Fatal("re-upload past the budget was accepted")
	}
	// Nothing oversize persisted, and A's good blob was not clobbered.
	if string(fb.stored[a.ID]) != gzBody("a") {
		t.Fatalf("A's blob = %q, want the original (a failed re-upload must not replace it)", fb.stored[a.ID])
	}

	// B now holds 5 and the budget is full: a fresh submission's upload is
	// refused up front, before reading any body.
	c, err := m.Create(ctx, CreateRequest{DisplayName: "C", SubmittedBy: "user-1"})
	if err != nil {
		t.Fatalf("create C: %v", err)
	}
	_, err = m.UploadContext(ctx, c.ID, "user-1", strings.NewReader(gzBody("c")))
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("upload with no budget left = %v, want ErrQuotaExceeded", err)
	}
	if _, ok := fb.stored[c.ID]; ok {
		t.Fatal("a budget-refused upload must persist nothing")
	}

	// A 5-byte replacement of A fits exactly (10 − B's 5), proving the
	// replacement is not double-charged against its own old bytes.
	if _, err := m.UploadContext(ctx, a.ID, "user-1", strings.NewReader(gzBody("z"))); err != nil {
		t.Fatalf("budget-exact replacement = %v, want accepted", err)
	}
}

// An approved context stays on the store for rebuilds, out of its uploader's
// hands; it leaves their budget, so approval gives the allowance back. It still
// fills the store's total, and a rejected context keeps its uploader's budget
// until it is reaped.
func TestApprovedContextsLeaveTheUploadersBudget(t *testing.T) {
	m, _, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	m.MaxStoredBytesPerUser = 5 // one gzBody("x") of 5 bytes
	m.MaxStoredBytesTotal = 10
	ctx := context.Background()
	create := func(user string) *Submission {
		t.Helper()
		sub, err := m.Create(ctx, CreateRequest{DisplayName: "P", SubmittedBy: user})
		if err != nil {
			t.Fatalf("create for %s: %v", user, err)
		}
		return sub
	}
	upload := func(sub *Submission) (*Submission, error) {
		return m.UploadContext(ctx, sub.ID, sub.SubmittedBy, strings.NewReader(gzBody("x")))
	}

	a, b := create("user-1"), create("user-1")
	a, err := upload(a)
	if err != nil {
		t.Fatalf("upload A: %v", err)
	}
	if _, err := upload(b); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("upload B beside a pending A = %v, want ErrQuotaExceeded", err)
	}
	if _, err := m.Approve(ctx, a.ID, "admin@example.test", a.ContextSHA256); err != nil {
		t.Fatalf("approve A: %v", err)
	}
	if _, err := upload(b); err != nil {
		t.Fatalf("upload B beside an approved A = %v, want accepted", err)
	}

	// A (approved) and B hold the store's 10 bytes between them.
	if _, err := upload(create("user-2")); !errors.Is(err, ErrUploadsFull) {
		t.Fatalf("upload into a store the approved A helps fill = %v, want ErrUploadsFull", err)
	}

	if _, err := m.Reject(ctx, b.ID, "admin@example.test", "no"); err != nil {
		t.Fatalf("reject B: %v", err)
	}
	m.MaxStoredBytesTotal = 100
	if _, err := upload(create("user-1")); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("upload beside a rejected B = %v, want ErrQuotaExceeded", err)
	}
}

// A single oversize blob stays a 400 (ErrInvalid), distinct from the 403 the
// per-user budget answers with — the two failure classes must not collapse.
func TestUploadContextOversizeIsNotQuotaError(t *testing.T) {
	m, _, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	m.MaxContextBytes = 4
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	_, err := m.UploadContext(context.Background(), seed.ID, "user-1", strings.NewReader(gzBody("too big")))
	if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrInvalid and NOT ErrQuotaExceeded", err)
	}
}

// A store failure while counting the pending queue must surface as-is, never as
// a quota verdict that blames the user.
func TestCreatePendingCountFailureSurfaces(t *testing.T) {
	m, st, _ := newManager()
	st.countErr = errors.New("db is down")
	_, err := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	if err == nil || errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want the raw store failure", err)
	}
}

// Withdraw retracts the submitter's own pending submission: the row AND its
// uploaded context are gone — which is what frees the pending slot and the
// storage budget for a fresh submission.
func TestWithdrawDeletesRowAndBlob(t *testing.T) {
	m, st, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	ctx := context.Background()
	seed, _ := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	if _, err := m.UploadContext(ctx, seed.ID, "user-1", strings.NewReader(gzBody("bytes"))); err != nil {
		t.Fatalf("UploadContext: %v", err)
	}

	sub, err := m.Withdraw(ctx, seed.ID, "user-1")
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if sub.ID != seed.ID || sub.Status != StatusPendingReview {
		t.Fatalf("withdrawn row = %+v, want the pending row back", sub)
	}
	if _, ok := st.subs[seed.ID]; ok {
		t.Fatal("withdraw must delete the row")
	}
	if _, ok := fb.stored[seed.ID]; ok {
		t.Fatal("withdraw must reap the uploaded context")
	}
}

// Another user's id is invisible on the withdraw path (404, not 403), and
// nothing is touched.
func TestWithdrawNotOwnerIsNotFound(t *testing.T) {
	m, st, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	ctx := context.Background()
	seed, _ := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	if _, err := m.UploadContext(ctx, seed.ID, "user-1", strings.NewReader(gzBody("x"))); err != nil {
		t.Fatalf("UploadContext: %v", err)
	}

	if _, err := m.Withdraw(ctx, seed.ID, "user-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, ok := st.subs[seed.ID]; !ok {
		t.Fatal("a non-owner withdraw must not delete the row")
	}
	if _, ok := fb.stored[seed.ID]; !ok {
		t.Fatal("a non-owner withdraw must not reap the context")
	}
}

// A reviewed submission is frozen: the withdraw CAS refuses it (409), keeping
// the context a running/queued build may still consume.
func TestWithdrawReviewedIsAlreadyReviewed(t *testing.T) {
	m, st, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	ctx := context.Background()
	seed, _ := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	if _, err := m.UploadContext(ctx, seed.ID, "user-1", strings.NewReader(gzBody("x"))); err != nil {
		t.Fatalf("UploadContext: %v", err)
	}
	if _, err := m.Reject(ctx, seed.ID, "admin@example.net", "no"); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	if _, err := m.Withdraw(ctx, seed.ID, "user-1"); !errors.Is(err, ErrAlreadyReviewed) {
		t.Fatalf("err = %v, want ErrAlreadyReviewed", err)
	}
	if _, ok := st.subs[seed.ID]; !ok {
		t.Fatal("a reviewed row must survive a withdraw attempt")
	}
	if _, ok := fb.stored[seed.ID]; !ok {
		t.Fatal("a reviewed row's context must survive a withdraw attempt")
	}
}

// The admin delete retires ANY status, reaping the context with it.
func TestDeleteAnyStatusReapsContext(t *testing.T) {
	m, st, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	ctx := context.Background()
	seed, _ := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	if _, err := m.UploadContext(ctx, seed.ID, "user-1", strings.NewReader(gzBody("x"))); err != nil {
		t.Fatalf("UploadContext: %v", err)
	}
	if _, err := m.Reject(ctx, seed.ID, "admin@example.net", "no"); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	sub, err := m.Delete(ctx, seed.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if sub.Status != StatusRejected {
		t.Fatalf("returned status = %q, want the row as it was before the delete", sub.Status)
	}
	if _, ok := st.subs[seed.ID]; ok {
		t.Fatal("delete must remove the row")
	}
	if _, ok := fb.stored[seed.ID]; ok {
		t.Fatal("delete must reap the context")
	}
}

// Deleting an unknown id — including the second delete of one already gone —
// is ErrNotFound (404), not a crash and not a silent success.
func TestDeleteUnknownIsNotFound(t *testing.T) {
	m, _, _ := newManager()
	m.Blobs = newFakeBlobs()
	if _, err := m.Delete(context.Background(), "sub-nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A deployment without an upload transport has no blobs to reap: delete still
// retires the row.
func TestDeleteWithoutTransport(t *testing.T) {
	m, st, _ := newManager() // Blobs nil
	ctx := context.Background()
	seed, _ := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	if _, err := m.Delete(ctx, seed.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := st.subs[seed.ID]; ok {
		t.Fatal("delete must remove the row even with no transport")
	}
}

// A blob-cleanup failure after the row is gone must surface loudly (naming what
// was left behind), never masquerade as success.
func TestDeleteBlobCleanupFailureSurfaces(t *testing.T) {
	m, st, _ := newManager()
	fb := newFakeBlobs()
	fb.deleteErr = errors.New("pvc read-only")
	m.Blobs = fb
	ctx := context.Background()
	seed, _ := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	_, err := m.Delete(ctx, seed.ID)
	if err == nil || !strings.Contains(err.Error(), "could not be deleted") {
		t.Fatalf("err = %v, want the cleanup failure named", err)
	}
	if _, ok := st.subs[seed.ID]; ok {
		t.Fatal("the row is deleted before the blob reap; it must stay deleted")
	}
}

func TestApproveRefusesMissingContext(t *testing.T) {
	// With a transport wired, approving a submission whose context was never
	// uploaded fails BEFORE the CAS: the row stays pending and no build starts.
	m, st, bl := newManager()
	m.Blobs = newFakeBlobs() // empty → Exists=false
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})

	_, err := m.Approve(context.Background(), seed.ID, "admin@x", sha256Hex("never uploaded"))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid (no context uploaded)", err)
	}
	if got := st.subs[seed.ID]; got.Status != StatusPendingReview {
		t.Fatalf("status = %q, want still pending_review (CAS not reached)", got.Status)
	}
	if bl.calls != 0 {
		t.Fatalf("builds started = %d, want 0", bl.calls)
	}
}

func TestApproveProceedsWithUploadedContext(t *testing.T) {
	// The end-to-end user path: create -> upload -> admin approve -> exactly one
	// build through the SAME gated Builder.
	m, _, bl := newManager()
	m.Blobs = newFakeBlobs()
	m.ContextBaseURL = "http://felis-api-internal.felis.svc.cluster.local:8081"
	seed, _ := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	up, err := m.UploadContext(context.Background(), seed.ID, "user-1", strings.NewReader(gzBody("mods")))
	if err != nil {
		t.Fatalf("UploadContext: %v", err)
	}
	if up.ContextSHA256 != sha256Hex(gzBody("mods")) {
		t.Fatalf("upload digest = %q, want the stored bytes' sha256", up.ContextSHA256)
	}

	sub, err := m.Approve(context.Background(), seed.ID, "admin@x", up.ContextSHA256)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if sub.Status != StatusApproved {
		t.Fatalf("status = %q, want approved", sub.Status)
	}
	if bl.calls != 1 {
		t.Fatalf("builds started = %d, want 1", bl.calls)
	}
	if bl.got[0].ContextDigest != up.ContextSHA256 {
		t.Fatalf("build digest = %q, want the approved %q", bl.got[0].ContextDigest, up.ContextSHA256)
	}
}

// Review binds to bytes (build-supply-chain-6): an approval names the digest
// the admin inspected, and a re-upload after that makes it fail instead of
// building content nobody saw.
func TestApproveBindsTheReviewedDigest(t *testing.T) {
	ctx := context.Background()
	m, st, bl := newManager()
	m.Blobs = newFakeBlobs()
	seed, _ := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	benign, evil := gzBody("benign"), gzBody("evil")
	if _, err := m.UploadContext(ctx, seed.ID, "user-1", strings.NewReader(benign)); err != nil {
		t.Fatal(err)
	}
	reviewed := sha256Hex(benign)

	// The swap lands after the review.
	if _, err := m.UploadContext(ctx, seed.ID, "user-1", strings.NewReader(evil)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Approve(ctx, seed.ID, "admin@x", reviewed); !errors.Is(err, ErrContextChanged) {
		t.Fatalf("approve after a swap = %v, want ErrContextChanged", err)
	}

	// The swap lands between the Manager's read and its CAS.
	if _, err := m.UploadContext(ctx, seed.ID, "user-1", strings.NewReader(benign)); err != nil {
		t.Fatal(err)
	}
	st.beforeApprove = func() { st.subs[seed.ID].ContextSHA256 = sha256Hex(evil) }
	if _, err := m.Approve(ctx, seed.ID, "admin@x", reviewed); !errors.Is(err, ErrContextChanged) {
		t.Fatalf("approve racing a swap = %v, want ErrContextChanged", err)
	}
	st.beforeApprove = nil
	if st.subs[seed.ID].Status != StatusPendingReview || bl.calls != 0 {
		t.Fatalf("status %q, builds %d; want still pending with nothing built", st.subs[seed.ID].Status, bl.calls)
	}

	for _, bad := range []string{"", "not-a-digest"} {
		if _, err := m.Approve(ctx, seed.ID, "admin@x", bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("approve with digest %q = %v, want ErrInvalid", bad, err)
		}
	}

	// A context uploaded before digests were recorded must be uploaded again.
	st.subs[seed.ID].ContextSHA256 = ""
	if _, err := m.Approve(ctx, seed.ID, "admin@x", reviewed); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "upload it again") {
		t.Fatalf("approve of an undigested context = %v, want the re-upload instruction", err)
	}
}

// An upload that finishes after the approval won cannot slip its digest in: the
// row keeps the approved one, which the build's fetch step then enforces, and
// the uploader hears that the submission was reviewed.
func TestUploadAfterApprovalKeepsTheApprovedDigest(t *testing.T) {
	ctx := context.Background()
	m, st, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	seed, _ := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	up, _ := m.UploadContext(ctx, seed.ID, "user-1", strings.NewReader(gzBody("benign")))

	// The second upload passed its pending check; the approval wins while its
	// bytes are still streaming in.
	m.Blobs = approvingBlobs{fakeBlobs: fb, approve: func() { st.subs[seed.ID].Status = StatusApproved }}
	if _, err := m.UploadContext(ctx, seed.ID, "user-1", strings.NewReader(gzBody("evil"))); !errors.Is(err, ErrAlreadyReviewed) {
		t.Fatalf("late upload = %v, want ErrAlreadyReviewed", err)
	}
	if st.subs[seed.ID].ContextSHA256 != up.ContextSHA256 {
		t.Fatalf("row digest = %q, want the approved %q", st.subs[seed.ID].ContextSHA256, up.ContextSHA256)
	}
}

// approvingBlobs flips the row to approved once Put has stored the bytes.
type approvingBlobs struct {
	*fakeBlobs
	approve func()
}

func (a approvingBlobs) Put(ctx context.Context, id string, r io.Reader) (int64, error) {
	n, err := a.fakeBlobs.Put(ctx, id, r)
	a.approve()
	return n, err
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// keysOf lists a map's keys for test diagnostics.
func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Every user's contexts together are budgeted too: once they fill it, the next
// upload is refused as a full store (507), whoever makes it and however little
// of their own allowance they used.
func TestUploadContextGlobalBudget(t *testing.T) {
	m, _, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	m.MaxStoredBytesPerUser = 100
	m.MaxStoredBytesTotal = 10 // gzBody("x") is 5 bytes
	ctx := context.Background()

	for _, user := range []string{"user-1", "user-2"} {
		sub, err := m.Create(ctx, CreateRequest{DisplayName: "P", SubmittedBy: user})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.UploadContext(ctx, sub.ID, user, strings.NewReader(gzBody("x"))); err != nil {
			t.Fatalf("%s upload within the total = %v", user, err)
		}
	}
	sub, err := m.Create(ctx, CreateRequest{DisplayName: "P", SubmittedBy: "user-3"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.UploadContext(ctx, sub.ID, "user-3", strings.NewReader(gzBody("x")))
	if !errors.Is(err, ErrUploadsFull) || errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("upload into a full store = %v, want ErrUploadsFull", err)
	}
	if _, ok := fb.stored[sub.ID]; ok {
		t.Fatal("a refused upload must persist nothing")
	}
}

type roomyBlobs struct {
	*fakeBlobs
	err  error
	need int64
}

func (r *roomyBlobs) CheckRoom(need int64) error { r.need = need; return r.err }

// A store on the node's filesystem is asked for room for the most the upload may
// write before any of it is read.
func TestUploadContextChecksRoom(t *testing.T) {
	m, _, _ := newManager()
	rb := &roomyBlobs{fakeBlobs: newFakeBlobs(), err: fmt.Errorf("%w: disk", ErrUploadsFull)}
	m.Blobs = rb
	m.MaxContextBytes = 1000
	ctx := context.Background()
	sub, err := m.Create(ctx, CreateRequest{DisplayName: "P", SubmittedBy: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.UploadContext(ctx, sub.ID, "user-1", strings.NewReader(gzBody("x"))); !errors.Is(err, ErrUploadsFull) {
		t.Fatalf("upload onto a full disk = %v, want ErrUploadsFull", err)
	}
	if rb.need != 1000 {
		t.Fatalf("room asked for %d bytes, want the 1000-byte cap", rb.need)
	}
	rb.err = nil
	if _, err := m.UploadContext(ctx, sub.ID, "user-1", strings.NewReader(gzBody("x"))); err != nil {
		t.Fatalf("upload with room = %v", err)
	}
}

// A rejected submission's context goes once the retention has passed; an approved
// one stays (it rebuilds the image after a registry loss), and so do the rows.
func TestReapRejected(t *testing.T) {
	m, st, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	ctx := context.Background()
	ids := map[string]string{}
	for _, name := range []string{"old-rejected", "new-rejected", "approved", "pending"} {
		sub, err := m.Create(ctx, CreateRequest{DisplayName: name, SubmittedBy: "user-" + name})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.UploadContext(ctx, sub.ID, "user-"+name, strings.NewReader(gzBody(name))); err != nil {
			t.Fatal(err)
		}
		ids[name] = sub.ID
	}
	old, recent := testNow.Add(-8*24*time.Hour), testNow.Add(-time.Hour)
	st.subs[ids["old-rejected"]].Status, st.subs[ids["old-rejected"]].ReviewedAt = StatusRejected, &old
	st.subs[ids["new-rejected"]].Status, st.subs[ids["new-rejected"]].ReviewedAt = StatusRejected, &recent
	st.subs[ids["approved"]].Status, st.subs[ids["approved"]].ReviewedAt = StatusApproved, &old

	n, err := m.ReapRejected(ctx, RejectedContextRetention)
	if err != nil || n != 1 {
		t.Fatalf("ReapRejected = %d, %v; want 1", n, err)
	}
	for name, id := range ids {
		_, kept := fb.stored[id]
		if want := name != "old-rejected"; kept != want {
			t.Errorf("%s context kept = %v, want %v", name, kept, want)
		}
		if _, ok := st.subs[id]; !ok {
			t.Errorf("%s row deleted", name)
		}
	}
	if n, err := m.ReapRejected(ctx, RejectedContextRetention); err != nil || n != 0 {
		t.Fatalf("second ReapRejected = %d, %v; want 0", n, err)
	}
}
