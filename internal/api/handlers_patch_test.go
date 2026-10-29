package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// newPatchAPI wires the same admin-authenticated API as create, then pre-seeds an
// existing "survival" server so a PATCH has a live target to mutate.
func newPatchAPI() (*API, *fakeRepo, *fakeCluster, *fakeBuilder) {
	api, repo, cl, fb := newCreateAPI()
	cl.byName["survival"] = &ServerInfo{Name: "survival", Subdomain: "survival", StorageSize: "10Gi",
		AutostartPolicy: string(v1alpha1.AutostartOwnerOnly),
		DesiredState:    string(v1alpha1.DesiredStopped), Phase: string(v1alpha1.PhaseStopped)}
	repo.byName["survival"] = &ServerRecord{Name: "survival", Subdomain: "survival"}
	return api, repo, cl, fb
}

func patchSurvival(api *API, body string) *httptest.ResponseRecorder {
	return do(api.ExternalHandler(), "PATCH", "/api/v1/servers/survival", body, nil)
}

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
		repo.audits[0].Actor != "admin@example.net" || string(repo.audits[0].Payload) != `{"fields":["displayName"]}` {
		t.Fatalf("audit not written as expected: %+v", repo.audits)
	}
}

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

func TestPatchServerStorage(t *testing.T) {
	mk := func(quota bool) (*API, *fakeRepo, *fakeCluster) {
		api, repo, cl, _ := newPatchAPI()
		seedResources(cl)
		cl.byName["survival"].Disk = &ServerDiskUsage{UsedBytes: 3 << 30, LimitBytes: 10 << 30}
		repo.byName["survival"].OwnerID = "u1"
		repo.quota["u1"] = quota
		repo.serverResources["survival"] = ResourceSpec{CPUMilli: 1000, MemoryMB: 4096, StorageMB: 10240}
		return api, repo, cl
	}

	for _, size := range []string{"20Gi", "4Gi"} {
		api, repo, cl := mk(true)
		if w := patchSurvival(api, `{"storage":"`+size+`"}`); w.Code != http.StatusOK {
			t.Fatalf("storage %s: code = %d, want 200 (%s)", size, w.Code, w.Body.String())
		}
		if p := cl.patched["survival"]; p.StorageSize == nil || *p.StorageSize != size {
			t.Fatalf("storage %s: patched size = %v", size, p.StorageSize)
		}
		q := resource.MustParse(size)
		want := ResourceSpec{CPUMilli: 1000, MemoryMB: 4096, StorageMB: int(q.Value() >> 20)}
		if got := repo.resourceUpdates["survival"]; got != want {
			t.Fatalf("storage %s: resource cache = %+v, want %+v", size, got, want)
		}
	}

	api, _, cl := mk(true)
	w := patchSurvival(api, `{"storage":"3Gi"}`)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "storage_below_usage" {
		t.Fatalf("storage at the measured use: code = %d body %s, want 409 storage_below_usage", w.Code, w.Body.String())
	}
	if _, ok := cl.patched["survival"]; ok {
		t.Fatal("storage below the measured use reached the cluster")
	}

	api, _, cl = mk(false)
	w = patchSurvival(api, `{"storage":"20Gi"}`)
	if w.Code != http.StatusForbidden || decodeErr(t, w) != "quota_exceeded" {
		t.Fatalf("storage past the owner's quota: code = %d body %s, want 403 quota_exceeded", w.Code, w.Body.String())
	}
	if _, ok := cl.patched["survival"]; ok {
		t.Fatal("storage past the owner's quota reached the cluster")
	}
}

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
			name:     "storage that is not a quantity",
			body:     `{"storage":"lots"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "storage of zero",
			body:     `{"storage":"0"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "bad name in path",
			target:   "/api/v1/servers/Bad_Name",
			body:     `{"displayName":"x"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_name",
		},
		{
			name:     "display name too long",
			body:     `{"displayName":"` + strings.Repeat("x", 65) + `"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_display_name",
		},
		{
			name:     "display name with a right-to-left override",
			body:     `{"displayName":"abc` + string(rune(0x202E)) + `exe.txt"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_display_name",
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
			// The seeded server has no pod block, so there is no ceiling to keep.
			name:     "resources on a server with no memory ceiling",
			body:     `{"resources":{"cpu":"2"}}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "unknown field (no free YAML)",
			body:     `{"subdomain":"renamed"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
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

// seedResources gives survival the pod block create would have written: a 4Gi
// ceiling, a 1-core CPU limit and a 500m CPU request.
func seedResources(cl *fakeCluster) {
	cl.byName["survival"].JavaMemory = "3072M"
	cl.byName["survival"].Resources = corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourceCPU: resource.MustParse("1")},
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourceCPU: resource.MustParse("500m")},
	}
}

func wantQuantity(t *testing.T, what string, list corev1.ResourceList, key corev1.ResourceName, want string) {
	t.Helper()
	got, ok := list[key]
	if want == "" {
		if ok {
			t.Errorf("%s = %s, want none", what, got.String())
		}
		return
	}
	if !ok || got.Cmp(resource.MustParse(want)) != 0 {
		t.Errorf("%s = %s (present %v), want %s", what, got.String(), ok, want)
	}
}

func TestPatchServerEditsOneResource(t *testing.T) {
	t.Run("cpu only keeps the memory and the heap", func(t *testing.T) {
		api, repo, cl, _ := newPatchAPI()
		seedResources(cl)
		repo.byName["survival"].OwnerID = "u1"
		repo.quota["u1"] = true

		w := patchSurvival(api, `{"resources":{"cpu":"2"}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		p := cl.patched["survival"]
		if p.Resources == nil {
			t.Fatal("a cpu patch must carry the pod block")
		}
		wantQuantity(t, "memory limit", p.Resources.Limits, corev1.ResourceMemory, "4Gi")
		wantQuantity(t, "cpu limit", p.Resources.Limits, corev1.ResourceCPU, "2")
		wantQuantity(t, "memory request", p.Resources.Requests, corev1.ResourceMemory, "4Gi")
		wantQuantity(t, "cpu request", p.Resources.Requests, corev1.ResourceCPU, "500m")
		if p.JavaMemory != nil {
			t.Errorf("heap = %q, want untouched by a cpu patch", *p.JavaMemory)
		}
		if got := repo.resourceUpdates["survival"]; got.CPUMilli != 2000 || got.MemoryMB != 4096 {
			t.Errorf("resource cache = %+v, want cpu 2000 / mem 4096", got)
		}
		if body := w.Body.String(); !strings.Contains(body, `"patched":["resources"]`) {
			t.Errorf("response = %s, want patched [resources]", body)
		}
	})

	t.Run("memory only keeps the cpu limit and request", func(t *testing.T) {
		api, _, cl, _ := newPatchAPI()
		seedResources(cl)

		w := patchSurvival(api, `{"memory":"8Gi"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		p := cl.patched["survival"]
		wantQuantity(t, "memory limit", p.Resources.Limits, corev1.ResourceMemory, "8Gi")
		wantQuantity(t, "cpu limit", p.Resources.Limits, corev1.ResourceCPU, "1")
		wantQuantity(t, "memory request", p.Resources.Requests, corev1.ResourceMemory, "8Gi")
		wantQuantity(t, "cpu request", p.Resources.Requests, corev1.ResourceCPU, "500m")
		if p.JavaMemory == nil || *p.JavaMemory != "6144M" {
			t.Errorf("heap = %v, want 6144M derived from 8Gi", p.JavaMemory)
		}
	})

	t.Run("an empty cpu removes the limit", func(t *testing.T) {
		api, _, cl, _ := newPatchAPI()
		seedResources(cl)

		w := patchSurvival(api, `{"resources":{"cpu":""}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		p := cl.patched["survival"]
		wantQuantity(t, "cpu limit", p.Resources.Limits, corev1.ResourceCPU, "")
		wantQuantity(t, "memory limit", p.Resources.Limits, corev1.ResourceMemory, "4Gi")
	})

	for _, c := range []struct{ name, body string }{
		{"the memory ceiling cannot be emptied", `{"resources":{"memory":""}}`},
		{"a cpu request above the kept limit", `{"resources":{"cpuRequest":"2"}}`},
		{"a memory ceiling below the kept request", `{"resources":{"memory":"2Gi"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			api, _, cl, _ := newPatchAPI()
			seedResources(cl)

			w := patchSurvival(api, c.body)
			if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
				t.Fatalf("code = %d (%s), want 400 bad_request", w.Code, w.Body.String())
			}
			if len(cl.patched) != 0 {
				t.Errorf("a rejected patch must not write a spec, got %+v", cl.patched)
			}
		})
	}
}

func TestPatchServerClearsDisplayName(t *testing.T) {
	api, _, cl, _ := newPatchAPI()
	cl.byName["survival"].DisplayName = "Survival Realm"

	w := patchSurvival(api, `{"displayName":"   "}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	p := cl.patched["survival"]
	if p.DisplayName == nil || *p.DisplayName != "" {
		t.Fatalf("patched displayName = %v, want an empty one", p.DisplayName)
	}
}

func TestPatchServerOverQuotaMayShrink(t *testing.T) {
	api, repo, cl, _ := newPatchAPI()
	seedResources(cl)
	repo.byName["survival"].OwnerID = "u1"
	repo.quota["u1"] = false // over every cap: any check refuses
	repo.serverResources["survival"] = ResourceSpec{CPUMilli: 1000, MemoryMB: 4096, StorageMB: 10240}

	for _, body := range []string{`{"resources":{"cpu":"500m"}}`, `{"resources":{"cpu":"1"}}`} {
		delete(cl.patched, "survival")
		w := patchSurvival(api, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: code = %d, want 200 (%s)", body, w.Code, w.Body.String())
		}
		if _, ok := cl.patched["survival"]; !ok {
			t.Fatalf("%s: the patch did not reach the cluster", body)
		}
	}
	if len(repo.quotaChecked) != 0 {
		t.Errorf("a patch that grows nothing was quota-checked: %+v", repo.quotaChecked)
	}
	if got := repo.resourceUpdates["survival"]; got != (ResourceSpec{CPUMilli: 1000, MemoryMB: 4096, StorageMB: 10240}) {
		t.Errorf("resource cache = %+v, want cpu 1000 / mem 4096 / storage kept", got)
	}

	for _, body := range []string{`{"resources":{"cpu":"2"}}`, `{"resources":{"cpu":"500m","memory":"8Gi"}}`} {
		delete(cl.patched, "survival")
		w := patchSurvival(api, body)
		if w.Code != http.StatusForbidden || decodeErr(t, w) != "quota_exceeded" {
			t.Fatalf("growing an over-cap owner's server %s: code = %d body %s, want 403 quota_exceeded", body, w.Code, w.Body.String())
		}
		if _, ok := cl.patched["survival"]; ok {
			t.Fatalf("a refused growth %s reached the cluster", body)
		}
	}
}

func TestPatchServerResizeCache(t *testing.T) {
	const body = `{"resources":{"cpu":"2"}}`
	before := ResourceSpec{CPUMilli: 750, MemoryMB: 2048, StorageMB: 5120}
	mk := func() (*API, *fakeRepo, *fakeCluster) {
		api, repo, cl, _ := newPatchAPI()
		seedResources(cl)
		cl.byName["survival"].StorageSize = "10Gi"
		repo.serverResources["survival"] = before
		return api, repo, cl
	}

	t.Run("a cache that cannot be written stops the patch", func(t *testing.T) {
		api, repo, cl := mk()
		repo.resizeErr = errors.New("database unreachable")
		if w := patchSurvival(api, body); w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d body %s, want 500", w.Code, w.Body.String())
		}
		if _, ok := cl.patched["survival"]; ok || len(repo.audits) != 0 {
			t.Fatalf("patched=%+v audits=%+v, want neither", cl.patched, repo.audits)
		}
	})

	t.Run("the patch lands with the new size cached", func(t *testing.T) {
		api, repo, cl := mk()
		if w := patchSurvival(api, body); w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s, want 200", w.Code, w.Body.String())
		}
		if _, ok := cl.patched["survival"]; !ok {
			t.Fatal("the patch did not reach the cluster")
		}
		if got, want := repo.resourceUpdates["survival"], (ResourceSpec{CPUMilli: 2000, MemoryMB: 4096, StorageMB: 10240}); got != want {
			t.Fatalf("resource cache = %+v, want %+v", got, want)
		}
	})

	t.Run("the cluster refuses: the cache takes the cluster's size", func(t *testing.T) {
		api, repo, cl := mk()
		cl.patchErr = errors.New("apiserver unavailable")
		if w := patchSurvival(api, body); w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d body %s, want 500", w.Code, w.Body.String())
		}
		if got, want := repo.resourceUpdates["survival"], (ResourceSpec{CPUMilli: 1000, MemoryMB: 4096, StorageMB: 10240}); got != want {
			t.Fatalf("resource cache = %+v, want the cluster's %+v", got, want)
		}
	})

	t.Run("the server is gone: the cache takes back what it held", func(t *testing.T) {
		api, repo, cl := mk()
		cl.goneOnPatch = true
		if w := patchSurvival(api, body); w.Code != http.StatusNotFound || decodeErr(t, w) != "not_found" {
			t.Fatalf("code = %d body %s, want 404 not_found", w.Code, w.Body.String())
		}
		if got := repo.resourceUpdates["survival"]; got != before {
			t.Fatalf("resource cache = %+v, want %+v", got, before)
		}
	})

	t.Run("a client that hangs up still gets the cache put back", func(t *testing.T) {
		api, repo, cl := mk()
		cl.patchErr = context.Canceled
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := httptest.NewRequest("PATCH", "/api/v1/servers/survival", strings.NewReader(body)).WithContext(ctx)
		api.ExternalHandler().ServeHTTP(httptest.NewRecorder(), r)
		if got, want := repo.resourceUpdates["survival"], (ResourceSpec{CPUMilli: 1000, MemoryMB: 4096, StorageMB: 10240}); got != want {
			t.Fatalf("resource cache = %+v, want the cluster's %+v", got, want)
		}
	})
}
