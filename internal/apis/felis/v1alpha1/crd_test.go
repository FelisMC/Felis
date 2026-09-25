package v1alpha1_test

import (
	"encoding/json"
	"os"
	"testing"

	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
	"sigs.k8s.io/yaml"
)

// crdSchema loads the openAPIV3Schema the cluster enforces, from the same YAML
// `felis bootstrap-assets crd` applies. The x-kubernetes-validations (CEL) rules
// ride along as extensions and are checked live with a server-side dry run.
func crdSchema(t *testing.T) *spec.Schema {
	t.Helper()
	raw, err := os.ReadFile("../../../../deploy/crd/felis.lolicon.best_minecraftservers.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema json.RawMessage `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("versions = %d, want the single v1alpha1", len(crd.Spec.Versions))
	}
	var s spec.Schema
	if err := json.Unmarshal(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

// server is a minimal valid MinecraftServer with one extra spec section.
func server(section string, body map[string]any) map[string]any {
	sp := map[string]any{"image": "paper", "subdomain": "survival"}
	if section != "" {
		sp[section] = body
	}
	return map[string]any{
		"apiVersion": "felis.lolicon.best/v1alpha1",
		"kind":       "MinecraftServer",
		"metadata":   map[string]any{"name": "survival"},
		"spec":       sp,
	}
}

func TestCRDBoundsNumericFields(t *testing.T) {
	schema := crdSchema(t)
	cases := []struct {
		name    string
		section string
		body    map[string]any
		ok      bool
	}{
		{"defaults", "", nil, true},
		{"rcon port 0 means default", "rcon", map[string]any{"port": 0}, true},
		{"rcon port 25575", "rcon", map[string]any{"port": 25575}, true},
		{"rcon port negative", "rcon", map[string]any{"port": -1}, false},
		{"rcon port past 65535", "rcon", map[string]any{"port": 70000}, false},
		{"grace period 3600", "lifecycle", map[string]any{"terminationGracePeriodSeconds": 3600}, true},
		{"grace period negative", "lifecycle", map[string]any{"terminationGracePeriodSeconds": -1}, false},
		{"grace period past an hour", "lifecycle", map[string]any{"terminationGracePeriodSeconds": 3601}, false},
		{"startup timeout a day", "startup", map[string]any{"timeoutSeconds": 86400}, true},
		{"startup timeout negative", "startup", map[string]any{"timeoutSeconds": -5}, false},
		{"startup timeout past a day", "startup", map[string]any{"timeoutSeconds": 100000}, false},
		{"readiness timeout negative", "startup", map[string]any{"readinessTimeoutSeconds": -1}, false},
		{"readiness timeout past a day", "startup", map[string]any{"readinessTimeoutSeconds": 86401}, false},
		{"health port 8080", "startup", map[string]any{"healthHTTPPort": 8080}, true},
		{"health port past 65535", "startup", map[string]any{"healthHTTPPort": 70000}, false},
		{"health port negative", "startup", map[string]any{"healthHTTPPort": -1}, false},
		{"idle a week", "idle", map[string]any{"emptySecondsBeforeStop": 604800}, true},
		{"idle negative", "idle", map[string]any{"emptySecondsBeforeStop": -1}, false},
		{"idle past a week", "idle", map[string]any{"emptySecondsBeforeStop": 604801}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Round-trip through JSON so numbers arrive as the float64 the
			// apiserver's decoder hands the validator.
			raw, _ := json.Marshal(server(tc.section, tc.body))
			var obj any
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatal(err)
			}
			err := validate.AgainstSchema(schema, obj, strfmt.Default)
			if tc.ok && err != nil {
				t.Fatalf("rejected a valid spec: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("accepted an out-of-range value")
			}
		})
	}
}
