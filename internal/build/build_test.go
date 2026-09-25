package build

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var testNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// ---- in-memory fakes ----

type fakeStore struct {
	builds map[string]*Build
	images map[string]Image
	// call recorders
	admitted  []Image
	finished  []string // "id:status"
	removeErr error
	createErr error
	listOpts  ListOpts
}

func newFakeStore() *fakeStore {
	return &fakeStore{builds: map[string]*Build{}, images: map[string]Image{}}
}

func (f *fakeStore) CreateBuild(_ context.Context, b *Build) error {
	if f.createErr != nil {
		return f.createErr
	}
	cp := *b
	f.builds[b.ID] = &cp
	return nil
}

func (f *fakeStore) GetBuild(_ context.Context, id string) (*Build, error) {
	b, ok := f.builds[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *b
	return &cp, nil
}

func (f *fakeStore) SetBuildJob(_ context.Context, id, jobName string) error {
	b, ok := f.builds[id]
	if !ok {
		return ErrNotFound
	}
	b.JobName = jobName
	b.Status = StatusBuilding
	return nil
}

func (f *fakeStore) FinishBuild(_ context.Context, id string, status Status, errMsg string, at time.Time) error {
	b, ok := f.builds[id]
	if !ok {
		return ErrNotFound
	}
	b.Status = status
	b.Error = errMsg
	finished := at
	b.FinishedAt = &finished
	f.finished = append(f.finished, id+":"+string(status))
	return nil
}

func (f *fakeStore) ListUnfinishedBuilds(_ context.Context) ([]Build, error) {
	var out []Build
	for _, b := range f.builds {
		if !b.Status.terminal() {
			out = append(out, *b)
		}
	}
	// Oldest first, like the SQL; the frozen test clock ties, so the id breaks it.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (f *fakeStore) ListBuilds(_ context.Context, opts ListOpts) ([]Build, int, error) {
	f.listOpts = opts
	var out []Build
	for _, b := range f.builds {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	total := len(out)
	out = out[min(opts.Offset, total):min(opts.Offset+opts.Limit, total)]
	return out, total, nil
}

func (f *fakeStore) AdmitBuiltImage(_ context.Context, img Image) error {
	f.images[img.ImageRef] = img
	f.admitted = append(f.admitted, img)
	return nil
}

func (f *fakeStore) ListImages(_ context.Context) ([]Image, error) {
	var out []Image
	for _, img := range f.images {
		out = append(out, img)
	}
	return out, nil
}

func (f *fakeStore) AddExternalImage(_ context.Context, img Image) error {
	f.images[img.ImageRef] = img
	return nil
}

func (f *fakeStore) RemoveImage(_ context.Context, ref string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	if _, ok := f.images[ref]; !ok {
		return ErrNotFound
	}
	delete(f.images, ref)
	return nil
}

type fakeJobs struct {
	phase     JobPhase
	phaseErr  error
	createErr error
	// Per-job overrides of phase / phaseErr, keyed by Job name.
	phases    map[string]JobPhase
	phaseErrs map[string]error

	created   []JobParams
	cancelled []string
}

func (f *fakeJobs) CreateBuildJob(_ context.Context, p JobParams) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, p)
	return BuildJobName(p.BuildID), nil
}

func (f *fakeJobs) JobPhase(_ context.Context, name string) (JobPhase, error) {
	if err, ok := f.phaseErrs[name]; ok {
		return JobUnknown, err
	}
	if p, ok := f.phases[name]; ok {
		return p, nil
	}
	return f.phase, f.phaseErr
}

func (f *fakeJobs) CancelBuildJob(_ context.Context, jobName string) error {
	f.cancelled = append(f.cancelled, jobName)
	return nil
}

// newBuilder wires a Builder over fresh fakes with a frozen clock and a
// deterministic id generator.
func newBuilder() (*Builder, *fakeStore, *fakeJobs) {
	st := newFakeStore()
	jb := &fakeJobs{phase: JobPending}
	n := 0
	b := &Builder{
		Store: st,
		Jobs:  jb,
		Config: Config{
			RegistryURL: "registry.felis.svc:5000",
		},
		Now: func() time.Time { return testNow },
		IDGen: func() string {
			n++
			return "bld-" + string(rune('0'+n))
		},
	}
	return b, st, jb
}

func goodRequest() Request {
	return Request{
		ImageRef:    "registry.felis.svc:5000/mc-paper:1.0",
		Dockerfile:  "FROM eclipse-temurin:21\nCOPY . /data\n",
		ContextRef:  "tar://contexts/abc.tar.gz",
		RequestedBy: "admin@example.net",
	}
}

// ---- submission ----

func TestSubmitCreatesPendingBuildAndStartsJob(t *testing.T) {
	b, st, jb := newBuilder()
	bld, err := b.Submit(context.Background(), goodRequest())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if bld.Status != StatusBuilding {
		t.Errorf("status = %q, want building", bld.Status)
	}
	if bld.JobName == "" {
		t.Error("job name not recorded")
	}
	if got := st.builds[bld.ID]; got == nil || got.Dockerfile == "" {
		t.Error("build row not persisted with dockerfile")
	}
	if len(jb.created) != 1 {
		t.Fatalf("CreateBuildJob calls = %d, want 1", len(jb.created))
	}
	// The Job must be parameterised with the weak build SA and the namespace,
	// never the api identity — this is the §16 red line, asserted at the seam.
	p := jb.created[0]
	if p.ServiceAccount != defaultServiceAccount {
		t.Errorf("job SA = %q, want %q", p.ServiceAccount, defaultServiceAccount)
	}
	if p.Namespace != defaultNamespace {
		t.Errorf("job namespace = %q, want %q", p.Namespace, defaultNamespace)
	}
	if p.RegistryURL != "registry.felis.svc:5000" {
		t.Errorf("job registry = %q", p.RegistryURL)
	}
	if p.Deadline <= 0 {
		t.Error("job deadline not set")
	}
}

func TestSubmitRejectsExternalRegistryTarget(t *testing.T) {
	b, st, jb := newBuilder()
	req := goodRequest()
	req.ImageRef = "docker.io/library/evil:latest"
	if _, err := b.Submit(context.Background(), req); err == nil {
		t.Fatal("expected rejection of non-internal registry target")
	}
	if len(st.builds) != 0 {
		t.Error("a rejected build must not be persisted")
	}
	if len(jb.created) != 0 {
		t.Error("a rejected build must not start a job")
	}
}

// The platform's own images and the scanner's DB mirrors live under felis/ and
// mirror/; the registry gate refuses the build principal there, and Validate turns
// that into a 400 before a Job spends minutes building an image it cannot push.
// A context digest must be a real sha256 and needs a fetch step to enforce it.
func TestValidateContextDigest(t *testing.T) {
	cfg := Config{RegistryURL: "registry.felis.svc:5000", ContextOrigin: "http://felis-api-internal:8081"}
	req := Request{ImageRef: "registry.felis.svc:5000/user-uploads/sub-1:latest", Dockerfile: "FROM scratch",
		ContextRef: "http://felis-api-internal:8081/api/v1/internal/submissions/sub-1/context", ContextDigest: strings.Repeat("e", 64)}
	if err := Validate(req, cfg); err != nil {
		t.Fatalf("valid digest: %v", err)
	}
	bad := req
	bad.ContextDigest = strings.Repeat("E", 64)
	if err := Validate(bad, cfg); err == nil {
		t.Fatal("an uppercase digest was accepted")
	}
	bad = req
	bad.ContextRef = "s3://bucket/ctx.tar.gz"
	if err := Validate(bad, cfg); err == nil {
		t.Fatal("a digest on a context Kaniko fetches itself was accepted")
	}
}

// The fetch step hands the service token to the context URL's host, so an
// http(s) context must be an upload on the internal face (build-supply-chain-13).
func TestValidateConfinesHTTPContextsToTheInternalFace(t *testing.T) {
	cfg := Config{RegistryURL: "registry.felis.svc:5000", ContextOrigin: "http://felis-api-internal.felis.svc.cluster.local:8081/"}
	req := goodRequest()
	for _, ref := range []string{
		"http://felis-api-internal.felis.svc.cluster.local:8081/api/v1/internal/submissions/sub-1/context",
		"HTTP://Felis-API-Internal.felis.svc.cluster.local:8081/api/v1/internal/submissions/sub-1/context",
		"tar://contexts/abc.tar.gz",
	} {
		req.ContextRef = ref
		if err := Validate(req, cfg); err != nil {
			t.Errorf("Validate(%q) = %v, want accepted", ref, err)
		}
	}
	for _, ref := range []string{
		"https://attacker.example/api/v1/internal/submissions/sub-1/context",
		"http://felis-api-internal.felis.svc.cluster.local:8082/api/v1/internal/submissions/sub-1/context",
		"https://felis-api-internal.felis.svc.cluster.local:8081/api/v1/internal/submissions/sub-1/context",
		"http://felis-api-internal.felis.svc.cluster.local:8081/api/v1/internal/servers",
		"http://felis-api-internal.felis.svc.cluster.local:8081/api/v1/internal/submissions/../x/context",
		"http://felis-api-internal.felis.svc.cluster.local:8081/api/v1/internal/submissions/sub-1/context?next=https://x",
		"http://u:p@felis-api-internal.felis.svc.cluster.local:8081/api/v1/internal/submissions/sub-1/context",
	} {
		req.ContextRef = ref
		if err := Validate(req, cfg); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%q) = %v, want ErrInvalid", ref, err)
		}
	}
	req.ContextRef = "http://felis-api-internal.felis.svc.cluster.local:8081/api/v1/internal/submissions/sub-1/context"
	if err := Validate(req, Config{RegistryURL: "registry.felis.svc:5000"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("without an internal face configured an http context was accepted: %v", err)
	}
}

func TestValidateRejectsReservedRepos(t *testing.T) {
	cfg := Config{RegistryURL: "registry.felis.svc:5000"}
	for _, ref := range []string{
		"registry.felis.svc:5000/felis/felis:v0.1.0",
		"registry.felis.svc:5000/felis:latest",
		"registry.felis.svc:5000/felis",
		"registry.felis.svc:5000/mirror/trivy-db:2",
	} {
		req := goodRequest()
		req.ImageRef = ref
		if err := Validate(req, cfg); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%q) = %v, want ErrInvalid", ref, err)
		}
	}
	for _, ref := range []string{
		"registry.felis.svc:5000/user-uploads/s1:latest",
		"registry.felis.svc:5000/felis-pack:1",
		"registry.felis.svc:5000/builds/felis:1",
	} {
		req := goodRequest()
		req.ImageRef = ref
		if err := Validate(req, cfg); err != nil {
			t.Errorf("Validate(%q) = %v, want accepted", ref, err)
		}
	}
}

func TestSubmitRejectsEmptyAndOversizeDockerfile(t *testing.T) {
	b, _, _ := newBuilder()
	req := goodRequest()
	req.Dockerfile = "   "
	if _, err := b.Submit(context.Background(), req); err == nil {
		t.Error("expected rejection of empty dockerfile")
	}

	b2, _, _ := newBuilder()
	b2.Config.MaxDockerfileBytes = 16
	req2 := goodRequest()
	req2.Dockerfile = "FROM scratch\n# padding padding padding padding\n"
	if _, err := b2.Submit(context.Background(), req2); err == nil {
		t.Error("expected rejection of oversize dockerfile")
	}
}

func TestSubmitRequiresContext(t *testing.T) {
	b, _, _ := newBuilder()
	req := goodRequest()
	req.ContextRef = ""
	if _, err := b.Submit(context.Background(), req); err == nil {
		t.Error("expected rejection when context reference is missing")
	}
}

func TestSubmitMarksBuildFailedWhenJobCreationFails(t *testing.T) {
	b, st, jb := newBuilder()
	jb.createErr = errors.New("apiserver down")
	bld, err := b.Submit(context.Background(), goodRequest())
	if err == nil {
		t.Fatal("expected error when job creation fails")
	}
	if bld == nil || bld.Status != StatusFailed {
		t.Fatalf("build should be marked failed, got %+v", bld)
	}
	// The persisted row must not be left pending forever.
	if got := st.builds[bld.ID]; got == nil || got.Status != StatusFailed {
		t.Errorf("persisted status = %v, want failed", got)
	}
}

// ---- the scan gate (Job phase -> DB) ----

func TestSyncSucceededAdmitsImageWithAddedBy(t *testing.T) {
	b, st, jb := newBuilder()
	bld, _ := b.Submit(context.Background(), goodRequest())

	jb.phase = JobSucceeded
	got, err := b.Sync(context.Background(), bld.ID)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got.Status != StatusSucceeded {
		t.Errorf("status = %q, want succeeded", got.Status)
	}
	img, ok := st.images[bld.ImageRef]
	if !ok {
		t.Fatal("succeeded build did not admit its image to the whitelist")
	}
	if !img.Enabled {
		t.Error("admitted image must be enabled=true")
	}
	if img.Source != SourceBuilt {
		t.Errorf("source = %q, want built", img.Source)
	}
	if img.AddedBy != "admin@example.net" {
		t.Errorf("added_by = %q, want the requester", img.AddedBy)
	}
	if img.BuildID != bld.ID {
		t.Errorf("build_id = %q, want %q", img.BuildID, bld.ID)
	}
}

func TestSyncFailedDoesNotAdmitImage(t *testing.T) {
	// A failed Job is exactly the CRITICAL-CVE case: trivy --exit-code 1 fails
	// the Pod, so the gate is enforced as the Job verdict (spec §16).
	b, st, jb := newBuilder()
	bld, _ := b.Submit(context.Background(), goodRequest())

	jb.phase = JobFailed
	got, err := b.Sync(context.Background(), bld.ID)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if len(st.images) != 0 {
		t.Error("a failed/CRITICAL build must NOT admit any image")
	}
	if len(st.admitted) != 0 {
		t.Error("AdmitBuiltImage must not be called on failure")
	}
}

func TestSyncRunningIsNoOp(t *testing.T) {
	b, _, jb := newBuilder()
	bld, _ := b.Submit(context.Background(), goodRequest())
	jb.phase = JobRunning
	got, err := b.Sync(context.Background(), bld.ID)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got.Status != StatusBuilding {
		t.Errorf("status = %q, want building (unchanged)", got.Status)
	}
}

func TestSyncIsIdempotentOnTerminalBuild(t *testing.T) {
	b, st, jb := newBuilder()
	bld, _ := b.Submit(context.Background(), goodRequest())
	jb.phase = JobSucceeded
	if _, err := b.Sync(context.Background(), bld.ID); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	// Flip the phase to a value that would admit again; a terminal build must
	// not be re-reconciled.
	before := len(st.admitted)
	if _, err := b.Sync(context.Background(), bld.ID); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if len(st.admitted) != before {
		t.Error("terminal build was re-admitted on a second Sync")
	}
}

func TestSyncAllAdvancesUnfinishedBuilds(t *testing.T) {
	b, _, jb := newBuilder()
	if _, err := b.Submit(context.Background(), goodRequest()); err != nil {
		t.Fatalf("submit: %v", err)
	}
	jb.phase = JobSucceeded
	n, err := b.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if n != 1 {
		t.Errorf("advanced = %d, want 1", n)
	}
}

// Builds past MaxConcurrent wait pending and start oldest first as running ones
// finish (build-supply-chain-12).
func TestSubmitQueuesPastMaxConcurrent(t *testing.T) {
	b, st, jb := newBuilder()
	b.Config.MaxConcurrent = 1
	ctx := context.Background()
	first, err := b.Submit(ctx, goodRequest())
	if err != nil || first.Status != StatusBuilding {
		t.Fatalf("first = %+v, %v; want building", first, err)
	}
	var queued []*Build
	for range 2 {
		q, err := b.Submit(ctx, goodRequest())
		if err != nil || q.Status != StatusPending || q.JobName != "" {
			t.Fatalf("queued = %+v, %v; want pending with no Job", q, err)
		}
		queued = append(queued, q)
	}
	if len(jb.created) != 1 {
		t.Fatalf("jobs created = %d, want 1", len(jb.created))
	}

	// A queued build is not a phantom: Sync leaves it alone.
	if got, err := b.Sync(ctx, queued[0].ID); err != nil || got.Status != StatusPending {
		t.Fatalf("Sync(queued) = %+v, %v; want still pending", got, err)
	}
	// Nothing frees up while the first runs.
	if _, err := b.SyncAll(ctx); err != nil || len(jb.created) != 1 {
		t.Fatalf("SyncAll while full: err %v, jobs %d", err, len(jb.created))
	}

	jb.phases = map[string]JobPhase{first.JobName: JobSucceeded}
	n, err := b.SyncAll(ctx)
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if n != 1 || len(jb.created) != 2 || jb.created[1].BuildID != queued[0].ID {
		t.Fatalf("advanced %d, jobs %v; want the first finished and the oldest queued started", n, jb.created)
	}
	if st.builds[queued[0].ID].Status != StatusBuilding || st.builds[queued[1].ID].Status != StatusPending {
		t.Fatalf("statuses = %s, %s; want building, pending",
			st.builds[queued[0].ID].Status, st.builds[queued[1].ID].Status)
	}
}

// One build whose Job cannot be read does not stall the others.
func TestSyncAllContinuesPastOneBuildsError(t *testing.T) {
	b, st, jb := newBuilder()
	b.Config.MaxConcurrent = 3
	ctx := context.Background()
	var ids []string
	for range 3 {
		bld, err := b.Submit(ctx, goodRequest())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, bld.ID)
	}
	jb.phase = JobSucceeded
	jb.phaseErrs = map[string]error{BuildJobName(ids[0]): errors.New("apiserver hiccup")}
	n, err := b.SyncAll(ctx)
	if err == nil || !strings.Contains(err.Error(), ids[0]) {
		t.Fatalf("err = %v, want it to name %s", err, ids[0])
	}
	if n != 2 || st.builds[ids[1]].Status != StatusSucceeded || st.builds[ids[2]].Status != StatusSucceeded {
		t.Fatalf("advanced %d; statuses %s %s; want the other two succeeded", n,
			st.builds[ids[1]].Status, st.builds[ids[2]].Status)
	}
}

// TestImageBuildFailuresMetricCountsFailedBuilds asserts felis_image_build_failures_total
// (spec §23) advances exactly once per failed build from BOTH terminal-failure
// producers, and never on a successful build. There are two distinct Inc sites —
// finishAt (the Sync verdict) and Submit's job-creation bypass — so each is
// exercised separately; the success case is the negative control proving the
// StatusFailed guard discriminates rather than firing on every terminal write.
// Deltas are read around each action because the counter is a process-global
// singleton these package tests share (they run sequentially).
func TestImageBuildFailuresMetricCountsFailedBuilds(t *testing.T) {
	read := func() float64 { return testutil.ToFloat64(metrics.ImageBuildFailuresTotal) }

	t.Run("sync job-failed verdict increments via finishAt", func(t *testing.T) {
		b, _, jb := newBuilder()
		bld, _ := b.Submit(context.Background(), goodRequest())
		before := read()
		jb.phase = JobFailed
		if _, err := b.Sync(context.Background(), bld.ID); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if got := read() - before; got != 1 {
			t.Errorf("failures delta = %v, want 1", got)
		}
	})

	t.Run("submit job-create failure increments via bypass", func(t *testing.T) {
		b, _, jb := newBuilder()
		jb.createErr = errors.New("apiserver down")
		before := read()
		if _, err := b.Submit(context.Background(), goodRequest()); err == nil {
			t.Fatal("expected Submit error when job creation fails")
		}
		if got := read() - before; got != 1 {
			t.Errorf("failures delta = %v, want 1", got)
		}
	})

	t.Run("successful build does not increment", func(t *testing.T) {
		b, _, jb := newBuilder()
		bld, _ := b.Submit(context.Background(), goodRequest())
		before := read()
		jb.phase = JobSucceeded
		if _, err := b.Sync(context.Background(), bld.ID); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if got := read() - before; got != 0 {
			t.Errorf("failures delta on success = %v, want 0", got)
		}
	})
}

// ---- cancellation ----

func TestCancelDeletesJobAndMarksCancelled(t *testing.T) {
	b, _, jb := newBuilder()
	bld, _ := b.Submit(context.Background(), goodRequest())
	got, err := b.Cancel(context.Background(), bld.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got.Status != StatusCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	if len(jb.cancelled) != 1 {
		t.Errorf("CancelBuildJob calls = %d, want 1", len(jb.cancelled))
	}
}

func TestCancelTerminalBuildFails(t *testing.T) {
	b, _, jb := newBuilder()
	bld, _ := b.Submit(context.Background(), goodRequest())
	jb.phase = JobSucceeded
	if _, err := b.Sync(context.Background(), bld.ID); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := b.Cancel(context.Background(), bld.ID); !errors.Is(err, ErrAlreadyTerminal) {
		t.Errorf("Cancel on terminal build err = %v, want ErrAlreadyTerminal", err)
	}
}

// ---- external admission ----

func TestAddExternalImageIsEnabledAndRecorded(t *testing.T) {
	b, st, _ := newBuilder()
	img, err := b.AddExternalImage(context.Background(), "registry.felis.svc:5000/ext:1", "admin@example.net")
	if err != nil {
		t.Fatalf("AddExternalImage: %v", err)
	}
	if img.Source != SourceExternal || !img.Enabled {
		t.Errorf("external image = %+v, want enabled external", img)
	}
	if _, ok := st.images["registry.felis.svc:5000/ext:1"]; !ok {
		t.Error("external image not persisted")
	}
}

func TestAddExternalImageRejectsMalformedRef(t *testing.T) {
	b, _, _ := newBuilder()
	if _, err := b.AddExternalImage(context.Background(), "not a ref!!", "admin@example.net"); err == nil {
		t.Error("expected rejection of malformed image ref")
	}
}

func TestRemoveImage(t *testing.T) {
	b, st, _ := newBuilder()
	st.images["registry.felis.svc:5000/x:1"] = Image{ImageRef: "registry.felis.svc:5000/x:1"}
	if err := b.RemoveImage(context.Background(), "registry.felis.svc:5000/x:1"); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	if _, ok := st.images["registry.felis.svc:5000/x:1"]; ok {
		t.Error("image not removed")
	}
}

// ---- whitelist admission for the create-server form (spec §15) ----

func TestImageMatches(t *testing.T) {
	cases := []struct {
		ref, pattern string
		want         bool
	}{
		// exact match
		{"registry.felis.svc:5000/mc:1.0", "registry.felis.svc:5000/mc:1.0", true},
		// exact mismatch on tag
		{"registry.felis.svc:5000/mc:1.0", "registry.felis.svc:5000/mc:2.0", false},
		// tag wildcard admits any concrete tag on the repo
		{"registry.felis.svc:5000/mc:1.0", "registry.felis.svc:5000/mc:*", true},
		{"registry.felis.svc:5000/mc:anything", "registry.felis.svc:5000/mc:*", true},
		// wildcard does not cross repos
		{"registry.felis.svc:5000/other:1", "registry.felis.svc:5000/mc:*", false},
		// a registry-port colon is not a tag separator: an untagged ref under a
		// ported host has no tag, so a wildcard (which needs a non-empty tag) misses
		{"registry.felis.svc:5000/mc", "registry.felis.svc:5000/mc:*", false},
		// a non-wildcard pattern never matches via the prefix branch
		{"registry.felis.svc:5000/mc:1", "registry.felis.svc:5000/mc", false},
	}
	for _, c := range cases {
		if got := imageMatches(c.ref, c.pattern); got != c.want {
			t.Errorf("imageMatches(%q, %q) = %v, want %v", c.ref, c.pattern, got, c.want)
		}
	}
}

func TestImageAdmitted(t *testing.T) {
	b, st, _ := newBuilder()
	st.images["registry.felis.svc:5000/exact:1"] = Image{ImageRef: "registry.felis.svc:5000/exact:1", Enabled: true}
	st.images["registry.felis.svc:5000/wild:*"] = Image{ImageRef: "registry.felis.svc:5000/wild:*", Enabled: true}
	st.images["registry.felis.svc:5000/off:1"] = Image{ImageRef: "registry.felis.svc:5000/off:1", Enabled: false}

	cases := []struct {
		ref  string
		want bool
	}{
		{"registry.felis.svc:5000/exact:1", true},    // exact, enabled
		{"registry.felis.svc:5000/exact:2", false},   // wrong tag
		{"registry.felis.svc:5000/wild:99", true},    // wildcard, enabled, port-colon safe
		{"registry.felis.svc:5000/off:1", false},     // present but disabled
		{"registry.felis.svc:5000/unknown:1", false}, // not on the list
		{"", false},    // empty ref
		{"   ", false}, // blank ref
		// A pinned ref is admitted by its name:tag, wildcard or exact.
		{"registry.felis.svc:5000/exact:1@sha256:" + strings.Repeat("a", 64), true},
		{"registry.felis.svc:5000/wild:99@sha256:" + strings.Repeat("b", 64), true},
		{"registry.felis.svc:5000/exact:2@sha256:" + strings.Repeat("a", 64), false},
		{"registry.felis.svc:5000/off:1@sha256:" + strings.Repeat("a", 64), false},
		{"@sha256:" + strings.Repeat("a", 64), false},
	}
	for _, c := range cases {
		got, err := b.ImageAdmitted(context.Background(), c.ref)
		if err != nil {
			t.Fatalf("ImageAdmitted(%q): %v", c.ref, err)
		}
		if got != c.want {
			t.Errorf("ImageAdmitted(%q) = %v, want %v", c.ref, got, c.want)
		}
	}
}

// A 'recommended' row (0018) is curation, not capability: it must be admitted by
// exactly the rule that governs every other source, and it must not become a way
// to bypass the disable switch. Both halves are pinned here because the failure
// modes are silent and opposite — make admission source-aware in one direction
// and the curated images quietly vanish from the create-server form; in the
// other, a disabled recommendation stays creatable after an admin pulled it.
func TestRecommendedImageAdmittedLikeAnyOtherSource(t *testing.T) {
	b, st, _ := newBuilder()
	st.images["felis-lobby:demo"] = Image{
		ImageRef: "felis-lobby:demo", Source: SourceRecommended, Enabled: true,
	}
	st.images["felis-lobby:pulled"] = Image{
		ImageRef: "felis-lobby:pulled", Source: SourceRecommended, Enabled: false,
	}

	admitted, err := b.ImageAdmitted(context.Background(), "felis-lobby:demo")
	if err != nil {
		t.Fatalf("ImageAdmitted: %v", err)
	}
	if !admitted {
		t.Error("an enabled recommended image must be admitted; curation must not cost admission")
	}

	admitted, err = b.ImageAdmitted(context.Background(), "felis-lobby:pulled")
	if err != nil {
		t.Fatalf("ImageAdmitted: %v", err)
	}
	if admitted {
		t.Error("a disabled recommended image must not be admitted; curation is not a disable bypass")
	}
}

// ListBuilds keeps one page bounded whatever the query string asks for.
func TestListBuildsBoundsThePage(t *testing.T) {
	cases := []struct {
		in   ListOpts
		want ListOpts
	}{
		{ListOpts{}, ListOpts{Limit: DefaultListLimit}},
		{ListOpts{Limit: 5, Offset: 40}, ListOpts{Limit: 5, Offset: 40}},
		{ListOpts{Limit: 100000}, ListOpts{Limit: MaxListLimit}},
		{ListOpts{Limit: -3, Offset: -7}, ListOpts{Limit: DefaultListLimit}},
		{ListOpts{Query: "  paper \t"}, ListOpts{Query: "paper", Limit: DefaultListLimit}},
	}
	for _, c := range cases {
		b, st, _ := newBuilder()
		if _, _, err := b.ListBuilds(context.Background(), c.in); err != nil {
			t.Fatalf("ListBuilds(%+v): %v", c.in, err)
		}
		if st.listOpts != c.want {
			t.Errorf("ListBuilds(%+v) asked the store for %+v, want %+v", c.in, st.listOpts, c.want)
		}
	}
}
