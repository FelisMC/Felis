package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// newPatchAPI wires the same admin-authenticated API as create, then pre-seeds an
// existing "survival" server so a PATCH has a live target to mutate.
func newPatchAPI() (*API, *fakeRepo, *fakeCluster, *fakeBuilder) {
	api, repo, cl, fb := newCreateAPI()
	cl.byName["survival"] = &ServerInfo{Name: "survival", Subdomain: "survival",
		AutostartPolicy: string(v1alpha1.AutostartOwnerOnly),
		DesiredState:    string(v1alpha1.DesiredStopped), Phase: string(v1alpha1.PhaseStopped)}
	repo.byName["survival"] = &ServerRecord{Name: "survival", Subdomain: "survival"}
	return api, repo, cl, fb
}

func patchSurvival(api *API, body string) *httptest.ResponseRecorder {
	return do(api.ExternalHandler(), "PATCH", "/api/v1/servers/survival", body, nil)
}

// TestPatchServerDisplayName covers the simplest spec mutation end-to-end: a
// cosmetic field is patched, the merge reaches the cluster, and the admin is
// audited. It also pins that a displayName patch needs no image admission.
func TestPatchServerDisplayName(t *testing.T) {
	api, repo, cl, _ := newPatchAPI()

	w := patchSurvival(api, `{"displayName":"Survival Realm"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	p, ok := cl.patched["survival"]
	if !ok {
		t.Fatal("PatchServerSpec was not called for survival")
	}
	if p.DisplayName == nil || *p.DisplayName != "Survival Realm" {
		t.Errorf("patched displayName = %v, want \"Survival Realm\"", p.DisplayName)
	}
	// Only the field set was carried; nothing else was touched.
	if p.AutostartPolicy != nil || p.Image != nil || p.JavaMemory != nil || p.Resources != nil {
		t.Errorf("unexpected extra fields in patch: %+v", p)
	}
	if len(repo.audits) != 1 || repo.audits[0].Action != "server.patch" ||
		repo.audits[0].Actor != "admin@example.net" {
		t.Fatalf("audit not written as expected: %+v", repo.audits)
	}
}

// TestPatchServerAutostartPolicy confirms a valid policy is parsed onto the CRD
// and the lifecycle view reflects it (the fake applies the merge).
func TestPatchServerAutostartPolicy(t *testing.T) {
	api, _, cl, _ := newPatchAPI()

	w := patchSurvival(api, `{"autostartPolicy":"public"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	p := cl.patched["survival"]
	if p.AutostartPolicy == nil || *p.AutostartPolicy != v1alpha1.AutostartPublic {
		t.Fatalf("patched autostartPolicy = %v, want public", p.AutostartPolicy)
	}
	if got := cl.byName["survival"].AutostartPolicy; got != string(v1alpha1.AutostartPublic) {
		t.Errorf("merged view autostartPolicy = %q, want public", got)
	}
}

// TestPatchServerImageReAdmitted proves a new image is re-checked against the
// whitelist before it reaches the CRD — admission is the only legal image source.
func TestPatchServerImageReAdmitted(t *testing.T) {
	api, _, cl, _ := newPatchAPI()

	w := patchSurvival(api, `{"image":"`+admittedImage+`","confirmImageChange":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if p := cl.patched["survival"]; p.Image == nil || *p.Image != admittedImage {
		t.Fatalf("patched image = %v, want %q", p.Image, admittedImage)
	}
}

// TestPatchServerMemoryReDerives confirms a memory change re-derives the §22
// ceiling and the JVM heap exactly as create does — from the FINAL limit.
func TestPatchServerMemoryReDerives(t *testing.T) {
	api, _, cl, _ := newPatchAPI()

	w := patchSurvival(api, `{"memory":"2Gi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	p := cl.patched["survival"]
	if p.JavaMemory == nil || *p.JavaMemory != "1536M" {
		t.Errorf("patched JavaMemory = %v, want 1536M", p.JavaMemory)
	}
	if p.Resources == nil {
		t.Fatal("memory patch must carry a resolved resources block (§22 ceiling)")
	}
	memLim, has := p.Resources.Limits[corev1.ResourceMemory]
	if !has || memLim.IsZero() || memLim.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("memory ceiling = %v, want non-zero 2Gi", p.Resources.Limits)
	}
}

// TestPatchServerMemoryOverride confirms the resources block widens the envelope
// and the heap derives from the OVERRIDDEN ceiling, mirroring create.
func TestPatchServerMemoryOverride(t *testing.T) {
	api, _, cl, _ := newPatchAPI()

	w := patchSurvival(api, `{"memory":"2Gi","resources":{"memory":"4Gi","cpu":"2"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	p := cl.patched["survival"]
	if p.JavaMemory == nil || *p.JavaMemory != "3072M" {
		t.Errorf("patched JavaMemory = %v, want 3072M (derived from 4Gi override)", p.JavaMemory)
	}
	if lim := p.Resources.Limits[corev1.ResourceMemory]; lim.Cmp(resource.MustParse("4Gi")) != 0 {
		t.Errorf("memory limit = %s, want 4Gi (override)", lim.String())
	}
	if lim := p.Resources.Limits[corev1.ResourceCPU]; lim.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("cpu limit = %s, want 2", lim.String())
	}
}

// TestPatchServerPreservesStorageCache pins the storage dimension of the quota
// aggregate: a resources patch cannot change storage, so the cached storage
// must survive it — passing 0 would silently zero the owner's aggregate (the
// cached columns are QuotaCheck's only input) from that patch onward.
func TestPatchServerPreservesStorageCache(t *testing.T) {
	api, repo, _, _ := newPatchAPI()
	repo.byName["survival"].OwnerID = "u1"
	repo.quota["u1"] = true
	repo.serverResources["survival"] = ResourceSpec{StorageMB: 10240}

	w := patchSurvival(api, `{"memory":"2Gi","resources":{"memory":"4Gi","cpu":"2"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	got := repo.resourceUpdates["survival"]
	if got.CPUMilli != 2000 || got.MemoryMB != 4096 || got.StorageMB != 10240 {
		t.Fatalf("resource cache = %+v, want cpu 2000 / mem 4096 / storage preserved 10240", got)
	}
}

// TestPatchServerRejections is the validation matrix: each malformed request is
// rejected with the right status and stable error code, and (critically) NOTHING
// reaches the cluster on a rejection — the analog of create's "no CRD written".
func TestPatchServerRejections(t *testing.T) {
	cases := []struct {
		name     string
		target   string // defaults to /api/v1/servers/survival
		body     string
		wantCode int
		wantErr  string
	}{
		{
			name:     "empty patch",
			body:     `{}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "storage is immutable",
			body:     `{"storage":"20Gi"}`,
			wantCode: http.StatusBadRequest, wantErr: "storage_immutable",
		},
		{
			name:     "bad name in path",
			target:   "/api/v1/servers/Bad_Name",
			body:     `{"displayName":"x"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_name",
		},
		{
			name:     "empty autostart policy",
			body:     `{"autostartPolicy":""}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "bad autostart policy",
			body:     `{"autostartPolicy":"sometimes"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "empty image",
			body:     `{"image":""}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "image not whitelisted",
			body:     `{"image":"docker.io/evil:latest"}`,
			wantCode: http.StatusBadRequest, wantErr: "image_not_whitelisted",
		},
		{
			name:     "non-positive memory",
			body:     `{"memory":"0"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "garbage memory quantity",
			body:     `{"memory":"lots"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "memory request exceeds limit",
			body:     `{"memory":"2Gi","resources":{"memoryRequest":"4Gi"}}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "resources without memory",
			body:     `{"resources":{"cpu":"2"}}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "unknown field (no free YAML)",
			body:     `{"subdomain":"renamed"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			// A well-formed patch against a missing server reaches the cluster, which
			// returns ErrNotFound -> 404. The fake records nothing on NotFound, so the
			// "no spec patched" invariant still holds.
			name:     "server not found",
			target:   "/api/v1/servers/ghost",
			body:     `{"displayName":"x"}`,
			wantCode: http.StatusNotFound, wantErr: "not_found",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api, _, cl, _ := newPatchAPI()
			target := c.target
			if target == "" {
				target = "/api/v1/servers/survival"
			}
			w := do(api.ExternalHandler(), "PATCH", target, c.body, nil)
			if w.Code != c.wantCode {
				t.Fatalf("code = %d, want %d (%s)", w.Code, c.wantCode, w.Body.String())
			}
			if got := decodeErr(t, w); got != c.wantErr {
				t.Errorf("error code = %q, want %q", got, c.wantErr)
			}
			// Every rejection short-circuits before a spec is written. (404 reaches
			// the cluster but the fake records nothing for a missing server.)
			if len(cl.patched) != 0 {
				t.Errorf("a rejected patch must not write a spec, got %+v", cl.patched)
			}
		})
	}
}

// TestPatchServerImageWithoutBuilderIs503 proves the Builder requirement is
// scoped to IMAGE patches only: a non-image patch succeeds without a Builder,
// while an image patch fails 503 before any spec is written.
func TestPatchServerImageWithoutBuilderIs503(t *testing.T) {
	api, _, cl, _ := newPatchAPI()
	api.Builder = nil

	// A displayName patch needs no admission source — it still succeeds.
	if w := patchSurvival(api, `{"displayName":"x"}`); w.Code != http.StatusOK {
		t.Fatalf("displayName patch without Builder: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	// An image patch has no whitelist to check against -> 503, nothing written.
	w := patchSurvival(api, `{"image":"`+admittedImage+`"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("image patch without Builder: code = %d, want 503 (%s)", w.Code, w.Body.String())
	}
	if p := cl.patched["survival"]; p.Image != nil {
		t.Error("no image may be patched without a Builder")
	}
}

// TestPatchServerIdleStop covers the idle auto-stop knob: 0 turns it off, a
// value inside the range is carried to the cluster, and one outside is refused
// before anything is written.
func TestPatchServerIdleStop(t *testing.T) {
	for _, tc := range []struct {
		body     string
		wantCode int
		want     int32
	}{
		{`{"idleStopSeconds":0}`, http.StatusOK, 0},
		{`{"idleStopSeconds":900}`, http.StatusOK, 900},
		{`{"idleStopSeconds":59}`, http.StatusBadRequest, 0},
		{`{"idleStopSeconds":86401}`, http.StatusBadRequest, 0},
		{`{"idleStopSeconds":-5}`, http.StatusBadRequest, 0},
	} {
		api, _, cl, _ := newPatchAPI()
		w := patchSurvival(api, tc.body)
		if w.Code != tc.wantCode {
			t.Fatalf("%s: code = %d, want %d (%s)", tc.body, w.Code, tc.wantCode, w.Body.String())
		}
		p, patched := cl.patched["survival"]
		if tc.wantCode != http.StatusOK {
			if patched {
				t.Fatalf("%s: a refused value reached the cluster: %+v", tc.body, p)
			}
			continue
		}
		if !patched || p.IdleStopSeconds == nil || *p.IdleStopSeconds != tc.want {
			t.Fatalf("%s: patched idle = %v, want %d", tc.body, p.IdleStopSeconds, tc.want)
		}
		if got := cl.byName["survival"].IdleStopSeconds; got != tc.want {
			t.Fatalf("%s: view idleStopSeconds = %d, want %d", tc.body, got, tc.want)
		}
	}
}
