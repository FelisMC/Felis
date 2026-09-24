package main

import (
	"encoding/json"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ---- policy parsing ----

func TestParseApplyAutostartPolicy(t *testing.T) {
	tests := []struct {
		input string
		want  v1alpha1.AutostartPolicy
		ok    bool
	}{
		{"", v1alpha1.AutostartOwnerOnly, true},
		{"ownerOnly", v1alpha1.AutostartOwnerOnly, true},
		{"public", v1alpha1.AutostartPublic, true},
		{"allowlist", v1alpha1.AutostartAllowlist, true},
		{"bogus", "", false},
		{"Public", "", false},
	}
	for _, tc := range tests {
		got, err := parseApplyAutostartPolicy(tc.input)
		if tc.ok {
			if err != nil {
				t.Errorf("parseApplyAutostartPolicy(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("parseApplyAutostartPolicy(%q) = %q, want %q", tc.input, got, tc.want)
			}
		} else {
			if err == nil {
				t.Errorf("parseApplyAutostartPolicy(%q) expected error, got %q", tc.input, got)
			}
		}
	}
}

// ---- quantity parsing ----

func TestParseApplyPositiveQuantity(t *testing.T) {
	tests := []struct {
		s     string
		field string
		ok    bool
		want  string
	}{
		{"1Gi", "mem", true, "1Gi"},
		{"512Mi", "mem", true, "512Mi"},
		{"4G", "mem", true, "4G"},
		{"2048M", "mem", true, "2048M"},
		{"100m", "cpu", true, "100m"},
		{"2", "cpu", true, "2"},
		{"0", "mem", false, ""},
		{"-1", "mem", false, ""},
		{"abc", "mem", false, ""},
	}
	for _, tc := range tests {
		q, err := parseApplyPositiveQuantity(tc.s, tc.field)
		if tc.ok {
			if err != nil {
				t.Errorf("parseApplyPositiveQuantity(%q, %q) unexpected error: %v", tc.s, tc.field, err)
				continue
			}
			if q.String() != tc.want {
				t.Errorf("parseApplyPositiveQuantity(%q, %q).String() = %q, want %q", tc.s, tc.field, q.String(), tc.want)
			}
		} else {
			if err == nil {
				t.Errorf("parseApplyPositiveQuantity(%q, %q) expected error", tc.s, tc.field)
			}
		}
	}
}

// ---- resource ceiling validation ----

func TestValidateResourceCeilings(t *testing.T) {
	tests := []struct {
		desc     string
		requests corev1.ResourceList
		limits   corev1.ResourceList
		ok       bool
	}{
		{"equal", resList("memory=1Gi"), resList("memory=1Gi"), true},
		{"request_under", resList("memory=512Mi"), resList("memory=1Gi"), true},
		{"request_over", resList("memory=2Gi"), resList("memory=1Gi"), false},
		{"cpu_ok", resList("cpu=1"), resList("cpu=2"), true},
		{"cpu_over", resList("cpu=3"), resList("cpu=2"), false},
		{"no_request", nil, resList("memory=1Gi"), true},
	}
	for _, tc := range tests {
		err := validateResourceCeilings(tc.requests, tc.limits)
		if tc.ok && err != nil {
			t.Errorf("validateResourceCeilings(%s) unexpected error: %v", tc.desc, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("validateResourceCeilings(%s) expected error", tc.desc)
		}
	}
}

// ---- JVM heap derivation ----

func TestDeriveApplyJavaHeap(t *testing.T) {
	// reserve = max(bytes*0.25, 512MiB), capped at bytes*0.5
	tests := []struct {
		limit string
		want  string
	}{
		{"1Gi", "512M"},   // 1Gi: reserve=512Mi (floor) → heap=512Mi
		{"2Gi", "1536M"},  // 2Gi: reserve=512Mi (floor,25%=512M=floor) → heap=1536Mi
		{"4Gi", "3072M"},  // 4Gi: reserve=1024Mi (25%) → heap=3072Mi
		{"8Gi", "6144M"},  // 8Gi: reserve=2048Mi (25%) → heap=6144Mi
		{"3Gi", "2304M"},  // 3Gi: reserve=768Mi (25%) → heap=2304Mi
		{"512Mi", "256M"}, // 512Mi: reserve=128Mi (25%) < floor=512Mi → reserve=256Mi (half cap) → heap=256Mi
		{"768Mi", "384M"}, // 768Mi: reserve=192Mi (25%) < floor=512Mi → reserve=384Mi (half cap) → heap=384Mi
		{"256Mi", "128M"}, // 256Mi: reserve=64Mi (25%) < floor=512Mi → capped at half 128Mi → heap=128Mi
	}
	for _, tc := range tests {
		limit := resource.MustParse(tc.limit)
		got := deriveApplyJavaHeap(limit)
		if got != tc.want {
			t.Errorf("deriveApplyJavaHeap(%s) = %q, want %q", tc.limit, got, tc.want)
		}
	}
}

// ---- CRD builder ----

func TestBuildMinecraftServerFromApplyRequest_Valid(t *testing.T) {
	req := applyRequest{
		Name:      "test-server",
		Subdomain: "test-server",
		Image:     "registry.felis.svc/paper:1.21",
		Memory:    "4Gi",
		Storage:   "20Gi",
	}
	ms, err := buildMinecraftServerFromApplyRequest(req, "minecraft")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ms.Name != "test-server" {
		t.Errorf("Name = %q, want test-server", ms.Name)
	}
	if ms.Namespace != "minecraft" {
		t.Errorf("Namespace = %q, want minecraft", ms.Namespace)
	}
	if ms.Spec.Subdomain != "test-server" {
		t.Errorf("Subdomain = %q", ms.Spec.Subdomain)
	}
	if ms.Spec.Image != "registry.felis.svc/paper:1.21" {
		t.Errorf("Image = %q", ms.Spec.Image)
	}
	if ms.Spec.DesiredState != v1alpha1.DesiredStopped {
		t.Errorf("DesiredState = %q, want Stopped", ms.Spec.DesiredState)
	}
	if ms.Spec.AutostartPolicy != v1alpha1.AutostartOwnerOnly {
		t.Errorf("AutostartPolicy = %q, want ownerOnly", ms.Spec.AutostartPolicy)
	}
	if ms.Spec.Storage.Size != "20Gi" {
		t.Errorf("Storage.Size = %q, want 20Gi", ms.Spec.Storage.Size)
	}
	// Same operational defaults as the API create path: without RCON the server
	// never reports players and the console answers 503; without spec.idle it
	// never stops on its own.
	if !ms.Spec.Rcon.Enabled || ms.Spec.Rcon.SecretRef.Name != naming.RconSecretName("test-server") {
		t.Errorf("Rcon = %+v, want enabled with the operator-minted secret", ms.Spec.Rcon)
	}
	if ms.Spec.Idle != v1alpha1.DefaultIdle() {
		t.Errorf("Idle = %+v, want the default %+v", ms.Spec.Idle, v1alpha1.DefaultIdle())
	}
	if ms.Spec.FallbackServer != naming.SystemLoginServer {
		t.Errorf("FallbackServer = %q, want the login gate", ms.Spec.FallbackServer)
	}
	mem, ok := ms.Spec.Resources.Limits[corev1.ResourceMemory]
	if !ok {
		t.Fatal("memory limit missing")
	}
	if mem.String() != "4Gi" {
		t.Errorf("memory limit = %q, want 4Gi", mem.String())
	}
	if ms.Spec.JavaMemory == "" {
		t.Error("JavaMemory is empty")
	}
}

func TestBuildMinecraftServerFromApplyRequest_AutostartPolicy(t *testing.T) {
	for _, p := range []string{"", "ownerOnly", "public", "allowlist"} {
		req := applyRequest{
			Name:            "srv",
			Subdomain:       "srv",
			Image:           "x",
			Memory:          "1Gi",
			Storage:         "10Gi",
			AutostartPolicy: p,
		}
		ms, err := buildMinecraftServerFromApplyRequest(req, "ns")
		if err != nil {
			t.Errorf("unexpected error for policy %q: %v", p, err)
			continue
		}
		want := v1alpha1.AutostartOwnerOnly
		if p != "" {
			want = v1alpha1.AutostartPolicy(p)
		}
		if ms.Spec.AutostartPolicy != want {
			t.Errorf("AutostartPolicy = %q, want %q", ms.Spec.AutostartPolicy, want)
		}
	}
}

func TestBuildMinecraftServerFromApplyRequest_Resources(t *testing.T) {
	req := applyRequest{
		Name:      "srv",
		Subdomain: "srv",
		Image:     "x",
		Memory:    "2Gi",
		Storage:   "10Gi",
		Resources: &resourceRequest{
			CPU:           "2",
			CPURequest:    "1",
			Memory:        "4Gi",
			MemoryRequest: "2Gi",
		},
	}
	ms, err := buildMinecraftServerFromApplyRequest(req, "ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	limits := ms.Spec.Resources.Limits
	if cpu := limits[corev1.ResourceCPU]; cpu.String() != "2" {
		t.Errorf("cpu limit = %q, want 2", cpu.String())
	}
	if mem := limits[corev1.ResourceMemory]; mem.String() != "4Gi" {
		t.Errorf("memory limit = %q, want 4Gi", mem.String())
	}
	reqs := ms.Spec.Resources.Requests
	if mem := reqs[corev1.ResourceMemory]; mem.String() != "2Gi" {
		t.Errorf("memory request = %q, want 2Gi", mem.String())
	}
	if cpu := reqs[corev1.ResourceCPU]; cpu.String() != "1" {
		t.Errorf("cpu request = %q, want 1", cpu.String())
	}
}

// ---- error cases ----

func TestBuildMinecraftServerFromApplyRequest_Errors(t *testing.T) {
	const ok = "srv" // valid name to isolate the field under test
	tests := []struct {
		desc      string
		req       applyRequest
		errSubstr string
	}{
		{
			"empty name",
			applyRequest{Name: "", Subdomain: ok, Image: "x", Memory: "1Gi", Storage: "1Gi"},
			"invalid name",
		},
		{
			"bad name chars",
			applyRequest{Name: "BAD", Subdomain: ok, Image: "x", Memory: "1Gi", Storage: "1Gi"},
			"invalid name",
		},
		{
			"reserved name",
			applyRequest{Name: "lobby", Subdomain: ok, Image: "x", Memory: "1Gi", Storage: "1Gi"},
			"reserved",
		},
		{
			"empty subdomain",
			applyRequest{Name: ok, Subdomain: "", Image: "x", Memory: "1Gi", Storage: "1Gi"},
			"invalid subdomain",
		},
		{
			"empty image",
			applyRequest{Name: ok, Subdomain: ok, Image: "", Memory: "1Gi", Storage: "1Gi"},
			"image is required",
		},
		{
			"whitespace-only image",
			applyRequest{Name: ok, Subdomain: ok, Image: "   ", Memory: "1Gi", Storage: "1Gi"},
			"image is required",
		},
		{
			"invalid policy",
			applyRequest{Name: ok, Subdomain: ok, Image: "x", Memory: "1Gi", Storage: "1Gi", AutostartPolicy: "nope"},
			"invalid autostartPolicy",
		},
		{
			"zero memory",
			applyRequest{Name: ok, Subdomain: ok, Image: "x", Memory: "0", Storage: "1Gi"},
			"must be a positive quantity",
		},
		{
			"invalid memory",
			applyRequest{Name: ok, Subdomain: ok, Image: "x", Memory: "abc", Storage: "1Gi"},
			"invalid memory quantity",
		},
		{
			"zero storage",
			applyRequest{Name: ok, Subdomain: ok, Image: "x", Memory: "1Gi", Storage: "0"},
			"must be a positive quantity",
		},
		{
			"invalid storage",
			applyRequest{Name: ok, Subdomain: ok, Image: "x", Memory: "1Gi", Storage: "xyz"},
			"invalid storage quantity",
		},
		{
			"resource request > limit",
			applyRequest{Name: ok, Subdomain: ok, Image: "x", Memory: "1Gi", Storage: "1Gi",
				Resources: &resourceRequest{MemoryRequest: "2Gi"}},
			"exceeds limit",
		},
		{
			"cpu request > limit",
			applyRequest{Name: ok, Subdomain: ok, Image: "x", Memory: "1Gi", Storage: "1Gi",
				Resources: &resourceRequest{CPU: "1", CPURequest: "2"}},
			"exceeds limit",
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := buildMinecraftServerFromApplyRequest(tc.req, "ns")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.errSubstr)
			}
		})
	}
}

// ---- JSON unknown-field rejection ----

func TestApplyRequestRejectsUnknownFields(t *testing.T) {
	raw := `{"name":"srv","subdomain":"srv","image":"x","memory":"1Gi","storage":"1Gi","bogusField":true}`
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var req applyRequest
	err := dec.Decode(&req)
	if err == nil {
		t.Fatal("expected unknown-field error, got nil")
	}
	if !strings.Contains(err.Error(), "bogusField") {
		t.Errorf("error = %q, should mention the unknown field", err)
	}
}

// ---- helpers ----

func resList(specs ...string) corev1.ResourceList {
	rl := corev1.ResourceList{}
	for _, s := range specs {
		name, val, _ := strings.Cut(s, "=")
		if name == "" {
			continue
		}
		rl[corev1.ResourceName(name)] = resource.MustParse(val)
	}
	return rl
}
