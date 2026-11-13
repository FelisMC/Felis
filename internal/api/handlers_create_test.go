package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/nodecontrol"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// decodeField returns one string field from a flat JSON success body.
func decodeField(t *testing.T, w *httptest.ResponseRecorder, field string) string {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
	}
	s, _ := raw[field].(string)
	return s
}

// newCreateAPI admits the canonical test image and exposes its stores for assertions.
func newCreateAPI() (*API, *fakeRepo, *fakeCluster, *fakeBuilder) {
	repo := newFakeRepo()
	cl := newFakeCluster()
	fb := &fakeBuilder{admitted: map[string]bool{admittedImage: true}}
	api := newTestAPI(repo, cl)
	api.Builder = fb
	api.External = staticExternal{p: &Principal{UserID: "a", Email: "admin@example.net",
		Role: "admin", ViaAdminAccess: true}}
	return api, repo, cl, fb
}

const admittedImage = "registry.felis.svc:5000/mc:1"

const validCreateBody = `{"name":"survival","subdomain":"survival",` +
	`"image":"registry.felis.svc:5000/mc:1","memory":"2Gi","storage":"10Gi"}`

// createBodyWithDisplayName is validCreateBody with a displayName, JSON-escaped.
func createBodyWithDisplayName(name string) string {
	quoted, _ := json.Marshal(name)
	return strings.TrimSuffix(validCreateBody, "}") + `,"displayName":` + string(quoted) + "}"
}

func TestCreateServerSuccess(t *testing.T) {
	api, repo, cl, _ := newCreateAPI()

	w := do(api.ExternalHandler(), "POST", "/api/v1/servers", validCreateBody, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (%s)", w.Code, w.Body.String())
	}

	in, ok := cl.created["survival"]
	if !ok {
		t.Fatal("CreateServer was not called for survival")
	}
	if in.Subdomain != "survival" || in.Image != admittedImage {
		t.Errorf("unexpected created input %+v", in)
	}
	// JavaMemory is the DERIVED JVM heap, not the raw K8s quantity: 2Gi ceiling
	// minus 512Mi off-heap headroom = 1536Mi, in JVM-valid "M" units (never "Gi").
	if in.JavaMemory != "1536M" {
		t.Errorf("JavaMemory = %q, want 1536M (2Gi limit minus headroom)", in.JavaMemory)
	}
	if in.StorageSize != "10Gi" {
		t.Errorf("StorageSize = %q, want 10Gi", in.StorageSize)
	}
	// Default autostart policy is the safe ownerOnly.
	if in.AutostartPolicy != v1alpha1.AutostartOwnerOnly {
		t.Errorf("autostartPolicy = %q, want ownerOnly", in.AutostartPolicy)
	}

	memLim, has := in.Resources.Limits[corev1.ResourceMemory]
	if !has || memLim.IsZero() {
		t.Fatalf("memory limit missing or zero: %+v", in.Resources.Limits)
	}
	if want := resource.MustParse("2Gi"); memLim.Cmp(want) != 0 {
		t.Errorf("memory limit = %s, want 2Gi", memLim.String())
	}
	if memReq := in.Resources.Requests[corev1.ResourceMemory]; memReq.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("memory request = %s, want 2Gi", memReq.String())
	}

	// Business rows seeded for the unowned server, alias bound.
	if !repo.seeded["survival"] {
		t.Error("servers row was not seeded")
	}
	if repo.aliases["survival"] != "survival" {
		t.Errorf("subdomain alias = %q, want survival", repo.aliases["survival"])
	}

	// Audit written with the admin's identity.
	if len(repo.audits) != 1 || repo.audits[0].Action != "server.create" ||
		repo.audits[0].Actor != "admin@example.net" {
		t.Fatalf("audit not written as expected: %+v", repo.audits)
	}

	// Response envelope.
	if got := decodeField(t, w, "desiredState"); got != "Stopped" {
		t.Errorf("desiredState = %q, want Stopped", got)
	}
}

func TestCreateServerResourceOverride(t *testing.T) {
	api, _, cl, _ := newCreateAPI()
	body := `{"name":"survival","subdomain":"survival","image":"registry.felis.svc:5000/mc:1",` +
		`"memory":"2Gi","storage":"10Gi","resources":{"cpu":"2","cpuRequest":"500m",` +
		`"memory":"4Gi","memoryRequest":"1Gi"}}`
	w := do(api.ExternalHandler(), "POST", "/api/v1/servers", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	in := cl.created["survival"]
	if lim := in.Resources.Limits[corev1.ResourceMemory]; lim.Cmp(resource.MustParse("4Gi")) != 0 {
		t.Errorf("memory limit = %s, want 4Gi (override)", lim.String())
	}
	if req := in.Resources.Requests[corev1.ResourceMemory]; req.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("memory request = %s, want 1Gi (override)", req.String())
	}
	if lim := in.Resources.Limits[corev1.ResourceCPU]; lim.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("cpu limit = %s, want 2", lim.String())
	}
	if req := in.Resources.Requests[corev1.ResourceCPU]; req.Cmp(resource.MustParse("500m")) != 0 {
		t.Errorf("cpu request = %s, want 500m", req.String())
	}
	// The JVM heap derives from the FINAL (overridden) 4Gi ceiling, not the
	// top-level 2Gi: 4Gi minus 1Gi (25%) headroom = 3072Mi.
	if in.JavaMemory != "3072M" {
		t.Errorf("JavaMemory = %q, want 3072M (derived from overridden 4Gi limit)", in.JavaMemory)
	}
}

func TestDeriveJavaHeap(t *testing.T) {
	cases := []struct{ limit, want string }{
		{"2Gi", "1536M"},  // 2048 - 512 (25% == floor)
		{"4Gi", "3072M"},  // 4096 - 1024 (25%)
		{"8Gi", "6144M"},  // 8192 - 2048 (25%)
		{"1Gi", "512M"},   // 1024 - 512 (floor, capped at half)
		{"512Mi", "256M"}, // 512 - 256 (floor capped at half)
	}
	for _, c := range cases {
		got := deriveJavaHeap(resource.MustParse(c.limit))
		if got != c.want {
			t.Errorf("deriveJavaHeap(%s) = %q, want %q", c.limit, got, c.want)
		}
		// Invariant: the derived heap must never reach the ceiling.
		heap := resource.MustParse(got)
		if heap.Cmp(resource.MustParse(c.limit)) >= 0 {
			t.Errorf("deriveJavaHeap(%s) = %s is not below the ceiling", c.limit, got)
		}
	}
}

func TestCreateServerRejections(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		setup    func(*fakeRepo, *fakeCluster)
		wantCode int
		wantErr  string
		check    func(*testing.T, *fakeRepo, *fakeCluster)
	}{
		{
			name:     "bad name",
			body:     `{"name":"Bad_Name","subdomain":"survival","image":"registry.felis.svc:5000/mc:1","memory":"2Gi","storage":"10Gi"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_name",
		},
		{
			name:     "memory below 768Mi",
			body:     `{"name":"survival","subdomain":"survival","image":"registry.felis.svc:5000/mc:1","memory":"512Mi","storage":"10Gi"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "reserved name",
			body:     `{"name":"admin","subdomain":"survival","image":"registry.felis.svc:5000/mc:1","memory":"2Gi","storage":"10Gi"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_name",
		},
		{
			name:     "bad subdomain",
			body:     `{"name":"survival","subdomain":"Bad.Sub","image":"registry.felis.svc:5000/mc:1","memory":"2Gi","storage":"10Gi"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_subdomain",
		},
		{
			name:     "reserved subdomain",
			body:     `{"name":"survival","subdomain":"lobby","image":"registry.felis.svc:5000/mc:1","memory":"2Gi","storage":"10Gi"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_subdomain",
		},
		{
			name:     "display name too long",
			body:     createBodyWithDisplayName(strings.Repeat("生", naming.MaxDisplayName+1)),
			wantCode: http.StatusBadRequest, wantErr: "bad_display_name",
		},
		{
			name:     "display name with a line break",
			body:     createBodyWithDisplayName("Survival" + string(rune(0x0A)) + "Realm"),
			wantCode: http.StatusBadRequest, wantErr: "bad_display_name",
		},
		{
			name:     "bad autostart policy",
			body:     `{"name":"survival","subdomain":"survival","image":"registry.felis.svc:5000/mc:1","memory":"2Gi","storage":"10Gi","autostartPolicy":"sometimes"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "another account's server without the Owner role",
			body:     strings.TrimSuffix(validCreateBody, "}") + `,"ownerId":"b"}`,
			wantCode: http.StatusForbidden, wantErr: "forbidden",
		},
		{
			name:     "an owner that is not a live account",
			body:     strings.TrimSuffix(validCreateBody, "}") + `,"ownerId":"a"}`,
			setup:    func(r *fakeRepo, _ *fakeCluster) { r.assignErr = ErrUnknownAssignee },
			wantCode: http.StatusBadRequest, wantErr: "unknown_user",
			check: func(t *testing.T, repo *fakeRepo, _ *fakeCluster) {
				if repo.aliases["survival"] != "" {
					t.Error("a create refused over its owner must release the subdomain")
				}
			},
		},
		{
			name:     "missing memory",
			body:     `{"name":"survival","subdomain":"survival","image":"registry.felis.svc:5000/mc:1","storage":"10Gi"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "non-positive memory",
			body:     `{"name":"survival","subdomain":"survival","image":"registry.felis.svc:5000/mc:1","memory":"0","storage":"10Gi"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "garbage memory quantity",
			body:     `{"name":"survival","subdomain":"survival","image":"registry.felis.svc:5000/mc:1","memory":"lots","storage":"10Gi"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "memory request exceeds limit",
			body:     `{"name":"survival","subdomain":"survival","image":"registry.felis.svc:5000/mc:1","memory":"2Gi","storage":"10Gi","resources":{"memoryRequest":"4Gi"}}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "missing storage",
			body:     `{"name":"survival","subdomain":"survival","image":"registry.felis.svc:5000/mc:1","memory":"2Gi"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "empty image",
			body:     `{"name":"survival","subdomain":"survival","image":"","memory":"2Gi","storage":"10Gi"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name:     "image not whitelisted",
			body:     `{"name":"survival","subdomain":"survival","image":"docker.io/evil:latest","memory":"2Gi","storage":"10Gi"}`,
			wantCode: http.StatusBadRequest, wantErr: "image_not_whitelisted",
		},
		{
			name:     "unknown field (no free YAML)",
			body:     `{"name":"survival","subdomain":"survival","image":"registry.felis.svc:5000/mc:1","memory":"2Gi","storage":"10Gi","apiVersion":"felis/v1alpha1"}`,
			wantCode: http.StatusBadRequest, wantErr: "bad_request",
		},
		{
			name: "subdomain taken in cluster",
			body: validCreateBody,
			setup: func(_ *fakeRepo, cl *fakeCluster) {
				cl.bySub["survival"] = &ServerInfo{Name: "other", Subdomain: "survival"}
			},
			wantCode: http.StatusConflict, wantErr: "subdomain_taken",
		},
		{
			name:     "subdomain bound to different server in PG",
			body:     validCreateBody,
			setup:    func(r *fakeRepo, _ *fakeCluster) { r.aliases["survival"] = "someoneelse" },
			wantCode: http.StatusConflict, wantErr: "subdomain_taken",
		},
		{
			name: "duplicate server name",
			body: `{"name":"survival","subdomain":"fresh","image":"registry.felis.svc:5000/mc:1","memory":"2Gi","storage":"10Gi"}`,
			setup: func(_ *fakeRepo, cl *fakeCluster) {
				cl.byName["survival"] = &ServerInfo{Name: "survival", Subdomain: "legacy"}
			},
			wantCode: http.StatusConflict, wantErr: "already_exists",
			check: func(t *testing.T, repo *fakeRepo, _ *fakeCluster) {
				if repo.aliases["fresh"] != "" {
					t.Errorf("stray alias bound on failed create: fresh -> %q", repo.aliases["fresh"])
				}
				if repo.seeded["survival"] {
					t.Error("a failed dup-name create must not seed a servers row")
				}
			},
		},
		{
			// A CR deleted by hand keeps its world volume; a new server of that
			// name would mount it and inherit the old world.
			name: "world volume left by a deleted server",
			body: validCreateBody,
			setup: func(_ *fakeRepo, cl *fakeCluster) {
				cl.orphanWorld = map[string]bool{"survival": true}
			},
			wantCode: http.StatusConflict, wantErr: "world_volume_exists",
			check: func(t *testing.T, repo *fakeRepo, _ *fakeCluster) {
				if repo.seeded["survival"] {
					t.Error("a create refused over a leftover volume must not seed a servers row")
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api, repo, cl, _ := newCreateAPI()
			if c.setup != nil {
				c.setup(repo, cl)
			}
			w := do(api.ExternalHandler(), "POST", "/api/v1/servers", c.body, nil)
			if w.Code != c.wantCode {
				t.Fatalf("code = %d, want %d (%s)", w.Code, c.wantCode, w.Body.String())
			}
			if got := decodeErr(t, w); got != c.wantErr {
				t.Errorf("error code = %q, want %q", got, c.wantErr)
			}
			if len(cl.created) != 0 {
				t.Errorf("a rejected create must not create a CRD, got %+v", cl.created)
			}
			if c.check != nil {
				c.check(t, repo, cl)
			}
		})
	}
}

func TestCreateServerNeedsAFourGigabyteHost(t *testing.T) {
	for _, tc := range []struct {
		capacity int64
		want     int
	}{
		{1700 << 20, http.StatusConflict},
		{3800 << 20, http.StatusCreated},
		{0, http.StatusCreated},
	} {
		api, _, cl, _ := newCreateAPI()
		api.FreeMemory = func(context.Context) ([]nodecontrol.NodeMemory, error) {
			return []nodecontrol.NodeMemory{{Node: "host", Bytes: 1 << 30, Capacity: tc.capacity}}, nil
		}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers", validCreateBody, nil)
		if w.Code != tc.want {
			t.Fatalf("capacity %d MiB: code = %d, want %d (%s)", tc.capacity>>20, w.Code, tc.want, w.Body.String())
		}
		if tc.want == http.StatusConflict && (decodeErr(t, w) != "host_too_small" || len(cl.created) != 0) {
			t.Errorf("a refused create answered %s and created %v", w.Body.String(), cl.created)
		}
	}
}

func TestResizeBelowTheMemoryFloorIsRefused(t *testing.T) {
	str := func(s string) *string { return &s }
	old := corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	if _, _, _, err := mergeResources(old, str("640Mi"), nil); err == nil {
		t.Error("resizing to 640Mi was accepted")
	}
	if _, _, _, err := mergeResources(old, nil, &patchResourceRequest{CPU: str("1")}); err != nil {
		t.Errorf("a CPU change on a server created before the floor: %v", err)
	}
	if _, _, _, err := mergeResources(old, str("768Mi"), nil); err != nil {
		t.Errorf("resizing to 768Mi: %v", err)
	}
}

func TestCreateServerGivesItToTheChosenOwner(t *testing.T) {
	api, repo, cl, _ := newCreateAPI()
	w := do(api.ExternalHandler(), "POST", "/api/v1/servers", strings.TrimSuffix(validCreateBody, "}")+`,"ownerId":"a"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	if _, ok := cl.created["survival"]; !ok || repo.owners["survival"].OwnerID != "a" {
		t.Fatalf("the creating admin must own the new server, got %+v", repo.owners["survival"])
	}

	api.External = staticExternal{p: &Principal{UserID: "o", Role: "owner", ViaAdminAccess: true}}
	body := `{"name":"creative","subdomain":"creative","image":"registry.felis.svc:5000/mc:1","memory":"2Gi","storage":"10Gi","ownerId":"b"}`
	if w := do(api.ExternalHandler(), "POST", "/api/v1/servers", body, nil); w.Code != http.StatusCreated || repo.owners["creative"].OwnerID != "b" {
		t.Fatalf("the Owner must be able to create a server for another account: %d %s, owner %+v", w.Code, w.Body.String(), repo.owners["creative"])
	}
}

func TestCreateServerWithoutBuilderIs503(t *testing.T) {
	api, _, cl, _ := newCreateAPI()
	api.Builder = nil
	w := do(api.ExternalHandler(), "POST", "/api/v1/servers", validCreateBody, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (%s)", w.Code, w.Body.String())
	}
	if _, created := cl.created["survival"]; created {
		t.Error("no CRD may be created without a Builder")
	}
}

func TestCreateServerTrimsDisplayName(t *testing.T) {
	api, _, cl, _ := newCreateAPI()
	w := do(api.ExternalHandler(), "POST", "/api/v1/servers", createBodyWithDisplayName("  Survival Realm  "), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	if got := cl.created["survival"].DisplayName; got != "Survival Realm" {
		t.Fatalf("created displayName = %q, want %q", got, "Survival Realm")
	}
}
