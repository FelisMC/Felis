package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"felis.lolicon.best/internal/build"
)

// fakeBuilder is an in-memory ImageBuilder for the handler tests. Each field is
// the canned outcome of the matching call; the recorders let a test assert what
// the handler forwarded.
type fakeBuilder struct {
	submitted   *build.Request
	submitErr   error
	getBuilds   map[string]*build.Build
	getErr      error
	syncErr     error
	cancelErr   error
	addedRef    string
	addedBy     string
	addErr      error
	removedRef  string
	removeErr   error
	images      []build.Image
	listErr     error
	lastBuildID string
	admitted    map[string]bool
	admitErr    error
}

func (f *fakeBuilder) Submit(_ context.Context, req build.Request) (*build.Build, error) {
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	cp := req
	f.submitted = &cp
	return &build.Build{ID: "bld-1", ImageRef: req.ImageRef, Status: build.StatusBuilding,
		RequestedBy: req.RequestedBy}, nil
}

func (f *fakeBuilder) Get(_ context.Context, id string) (*build.Build, error) {
	f.lastBuildID = id
	if f.getErr != nil {
		return nil, f.getErr
	}
	if b, ok := f.getBuilds[id]; ok {
		return b, nil
	}
	return nil, build.ErrNotFound
}

func (f *fakeBuilder) Sync(_ context.Context, id string) (*build.Build, error) {
	f.lastBuildID = id
	if f.syncErr != nil {
		return nil, f.syncErr
	}
	return &build.Build{ID: id, ImageRef: "registry.felis.svc:5000/x:1", Status: build.StatusSucceeded}, nil
}

func (f *fakeBuilder) Cancel(_ context.Context, id string) (*build.Build, error) {
	f.lastBuildID = id
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	return &build.Build{ID: id, ImageRef: "registry.felis.svc:5000/x:1", Status: build.StatusCancelled}, nil
}

func (f *fakeBuilder) ListImages(context.Context) ([]build.Image, error) {
	return f.images, f.listErr
}

func (f *fakeBuilder) AddExternalImage(_ context.Context, ref, addedBy string) (*build.Image, error) {
	if f.addErr != nil {
		return nil, f.addErr
	}
	f.addedRef, f.addedBy = ref, addedBy
	return &build.Image{ImageRef: ref, Source: build.SourceExternal, AddedBy: addedBy, Enabled: true}, nil
}

func (f *fakeBuilder) RemoveImage(_ context.Context, ref string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removedRef = ref
	return nil
}

func (f *fakeBuilder) ImageAdmitted(_ context.Context, ref string) (bool, error) {
	if f.admitErr != nil {
		return false, f.admitErr
	}
	return f.admitted[ref], nil
}

func adminAPI(b ImageBuilder) *API {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.Builder = b
	api.External = staticExternal{p: &Principal{UserID: "a", Email: "admin@example.net",
		Role: "admin", ViaAdminAccess: true}}
	return api
}

// Every image route is admin-tier: a non-admin is rejected before the handler.
func TestImageRoutesAreAdminOnly(t *testing.T) {
	cases := []struct {
		method, target, body string
	}{
		{"POST", "/api/v1/images/build", `{"image_ref":"registry.felis.svc:5000/x:1","dockerfile":"FROM x","context_ref":"c"}`},
		{"GET", "/api/v1/images/build/bld-1", ""},
		{"GET", "/api/v1/images/build/bld-1/logs", ""},
		{"POST", "/api/v1/images/build/bld-1/cancel", ""},
		{"GET", "/api/v1/images", ""},
		{"POST", "/api/v1/images", `{"image_ref":"registry.felis.svc:5000/x:1"}`},
		{"DELETE", "/api/v1/images?ref=registry.felis.svc:5000/x:1", ""},
	}
	for _, c := range cases {
		api := adminAPI(&fakeBuilder{})
		// downgrade to a plain user
		api.External = staticExternal{p: &Principal{UserID: "u", Role: "user"}}
		w := do(api.ExternalHandler(), c.method, c.target, c.body, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s: code = %d, want 403", c.method, c.target, w.Code)
		}
	}
}

func TestBuildImageSubmits(t *testing.T) {
	fb := &fakeBuilder{}
	api := adminAPI(fb)
	body := `{"image_ref":"registry.felis.svc:5000/mc:1","dockerfile":"FROM eclipse-temurin:21","context_ref":"tar://c.tgz"}`
	w := do(api.ExternalHandler(), "POST", "/api/v1/images/build", body, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	if fb.submitted == nil {
		t.Fatal("Submit was not called")
	}
	// The requester identity must come from the Access principal, never the body.
	if fb.submitted.RequestedBy != "admin@example.net" {
		t.Errorf("requested_by = %q, want the admin email", fb.submitted.RequestedBy)
	}
	if fb.submitted.ImageRef != "registry.felis.svc:5000/mc:1" {
		t.Errorf("image_ref = %q", fb.submitted.ImageRef)
	}
}

func TestBuildImageValidationIs400(t *testing.T) {
	fb := &fakeBuilder{submitErr: fmt.Errorf("%w: bad ref", build.ErrInvalid)}
	api := adminAPI(fb)
	body := `{"image_ref":"docker.io/evil:1","dockerfile":"FROM x","context_ref":"c"}`
	w := do(api.ExternalHandler(), "POST", "/api/v1/images/build", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if decodeErr(t, w) != "bad_request" {
		t.Errorf("error code = %q, want bad_request", decodeErr(t, w))
	}
}

func TestGetBuildReconcilesOnRead(t *testing.T) {
	fb := &fakeBuilder{}
	api := adminAPI(fb)
	w := do(api.ExternalHandler(), "GET", "/api/v1/images/build/bld-9", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if fb.lastBuildID != "bld-9" {
		t.Errorf("Sync called with %q, want bld-9", fb.lastBuildID)
	}
	var bld build.Build
	if err := json.Unmarshal(w.Body.Bytes(), &bld); err != nil || bld.Status != build.StatusSucceeded {
		t.Fatalf("unexpected body %s err %v", w.Body.String(), err)
	}
}

func TestGetBuildNotFoundIs404(t *testing.T) {
	fb := &fakeBuilder{syncErr: build.ErrNotFound}
	api := adminAPI(fb)
	w := do(api.ExternalHandler(), "GET", "/api/v1/images/build/missing", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

func TestCancelBuild(t *testing.T) {
	fb := &fakeBuilder{}
	api := adminAPI(fb)
	w := do(api.ExternalHandler(), "POST", "/api/v1/images/build/bld-1/cancel", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if fb.lastBuildID != "bld-1" {
		t.Errorf("Cancel called with %q", fb.lastBuildID)
	}
}

func TestCancelTerminalBuildIs409(t *testing.T) {
	fb := &fakeBuilder{cancelErr: build.ErrAlreadyTerminal}
	api := adminAPI(fb)
	w := do(api.ExternalHandler(), "POST", "/api/v1/images/build/bld-1/cancel", "", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 (%s)", w.Code, w.Body.String())
	}
}

func TestListImages(t *testing.T) {
	fb := &fakeBuilder{images: []build.Image{
		{ImageRef: "registry.felis.svc:5000/a:1", Source: build.SourceBuilt, Enabled: true},
	}}
	api := adminAPI(fb)
	w := do(api.ExternalHandler(), "GET", "/api/v1/images", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var got map[string][]build.Image
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if len(got["images"]) != 1 {
		t.Fatalf("images = %d, want 1", len(got["images"]))
	}
}

func TestAddExternalImage(t *testing.T) {
	fb := &fakeBuilder{}
	api := adminAPI(fb)
	w := do(api.ExternalHandler(), "POST", "/api/v1/images",
		`{"image_ref":"registry.felis.svc:5000/ext:1"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	if fb.addedRef != "registry.felis.svc:5000/ext:1" {
		t.Errorf("added ref = %q", fb.addedRef)
	}
	if fb.addedBy != "admin@example.net" {
		t.Errorf("added_by = %q, want the admin email", fb.addedBy)
	}
}

func TestRemoveImage(t *testing.T) {
	fb := &fakeBuilder{}
	api := adminAPI(fb)
	w := do(api.ExternalHandler(), "DELETE", "/api/v1/images?ref=registry.felis.svc:5000/x:1", "", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204 (%s)", w.Code, w.Body.String())
	}
	if fb.removedRef != "registry.felis.svc:5000/x:1" {
		t.Errorf("removed ref = %q", fb.removedRef)
	}
}

func TestRemoveImageRequiresRef(t *testing.T) {
	api := adminAPI(&fakeBuilder{})
	w := do(api.ExternalHandler(), "DELETE", "/api/v1/images", "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
}

// When no Builder is configured, image routes report 503 — but only after the
// admin gate, so the boundary is still enforced.
func TestImageRoutesWithoutBuilderAre503(t *testing.T) {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.Builder = nil
	api.External = staticExternal{p: &Principal{UserID: "a", Email: "admin@example.net",
		Role: "admin", ViaAdminAccess: true}}
	w := do(api.ExternalHandler(), "GET", "/api/v1/images", "", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", w.Code)
	}
}

// Compile-time proof that the production Builder satisfies the API interface.
var _ ImageBuilder = (*build.Builder)(nil)
