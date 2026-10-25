package v1alpha1_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
	"sigs.k8s.io/yaml"
)

// crdSchema loads the openAPIV3Schema that `felis bootstrap-assets crd` applies; CEL rules ride along as extensions and are checked by a server-side dry run.
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

func TestCRDBoundsStopCommands(t *testing.T) {
	schema := crdSchema(t)
	cases := []struct {
		name     string
		commands any
		ok       bool
	}{
		{"empty", []string{}, true},
		{"at item limit", make([]string, 16), true},
		{"past item limit", make([]string, 17), false},
		{"at length limit", []string{strings.Repeat("x", 1024)}, true},
		{"past length limit", []string{strings.Repeat("x", 1025)}, false},
		{"non-string item", []any{1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(server("lifecycle", map[string]any{"stopCommands": tc.commands}))
			if err != nil {
				t.Fatal(err)
			}
			var obj any
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatal(err)
			}
			err = validate.AgainstSchema(schema, obj, strfmt.Default)
			if (err == nil) != tc.ok {
				t.Fatalf("validation error = %v, want valid=%v", err, tc.ok)
			}
		})
	}
}

func TestCRDBoundsExtraPorts(t *testing.T) {
	schema := crdSchema(t)
	port := func(protocol string, number int) map[string]any {
		return map[string]any{"protocol": protocol, "port": number}
	}
	cases := []struct {
		name  string
		ports any
		valid bool
	}{
		{"empty", []any{}, true},
		{"both protocols", []any{port("TCP", 24454), port("UDP", 24454)}, true},
		{"five entries", []any{port("TCP", 1), port("TCP", 2), port("TCP", 3), port("TCP", 4), port("UDP", 65535)}, true},
		{"six entries", []any{port("TCP", 1), port("TCP", 2), port("TCP", 3), port("TCP", 4), port("TCP", 5), port("TCP", 6)}, false},
		{"missing protocol", []any{map[string]any{"port": 24454}}, false},
		{"missing port", []any{map[string]any{"protocol": "UDP"}}, false},
		{"SCTP", []any{port("SCTP", 24454)}, false},
		{"zero", []any{port("UDP", 0)}, false},
		{"too high", []any{port("TCP", 65536)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			object := server("", nil)
			object["spec"].(map[string]any)["extraPorts"] = tc.ports
			raw, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			var decoded any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if err := validate.AgainstSchema(schema, decoded, strfmt.Default); (err == nil) != tc.valid {
				t.Fatalf("validation error = %v, want valid=%v", err, tc.valid)
			}
		})
	}
	properties := schema.Properties["spec"]
	ports := properties.Properties["extraPorts"]
	keys, err := json.Marshal(ports.Extensions["x-kubernetes-list-map-keys"])
	if err != nil || string(keys) != `["protocol","port"]` || ports.Extensions["x-kubernetes-list-type"] != "map" {
		t.Fatal("extra port list must reject duplicate protocol/port pairs")
	}
	rules, err := json.Marshal(properties.Extensions["x-kubernetes-validations"])
	if err != nil {
		t.Fatal(err)
	}
	for _, managed := range []string{"25565", "25575", "self.rcon.port", "self.startup.healthHTTPPort"} {
		if !strings.Contains(string(rules), managed) {
			t.Fatalf("CRD has no managed port constraint for %s", managed)
		}
	}
	allocated := schema.Properties["status"].Properties["extraPorts"]
	for _, number := range []int{0, 65536} {
		object := map[string]any{"protocol": "UDP", "port": 24454, "nodePort": number}
		raw, err := json.Marshal([]any{object})
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if err := validate.AgainstSchema(&allocated, decoded, strfmt.Default); err == nil {
			t.Fatal("CRD accepted an invalid allocated NodePort")
		}
	}
}
