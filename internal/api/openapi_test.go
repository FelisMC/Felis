package api

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

var oasKnownMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true,
	"delete": true, "head": true, "options": true, "trace": true,
}

type oasDoc struct {
	OpenAPI string                      `json:"openapi"`
	Paths   map[string]map[string]oasOp `json:"paths"`
}

type oasOp struct {
	Faces   []string `json:"x-felis-face"`
	Tier    string   `json:"x-felis-tier"`
	Callers []string `json:"x-felis-callers"`
	// Setup marks an operation a setup-lockdown session may still use.
	Setup bool `json:"x-felis-setup-allowed"`
}

type oasFacet struct {
	faces   map[string]bool
	tier    string
	callers map[string]bool
	setup   bool
}

func TestOpenAPIMatchesServedRoutes(t *testing.T) {
	served := oasServedFacets(t)
	documented := oasDocumentedFacets(t)

	for key := range served {
		if _, ok := documented[key]; !ok {
			t.Errorf("route SERVED but not documented in api/openapi.yaml: %s", key)
		}
	}
	for key := range documented {
		if _, ok := served[key]; !ok {
			t.Errorf("route DOCUMENTED in api/openapi.yaml but not served: %s", key)
		}
	}

	for key, s := range served {
		d, ok := documented[key]
		if !ok {
			continue
		}
		if !oasSameSet(s.faces, d.faces) {
			t.Errorf("%s: x-felis-face mismatch — served %v, documented %v",
				key, oasSortedKeys(s.faces), oasSortedKeys(d.faces))
		}
		if s.tier != d.tier {
			t.Errorf("%s: x-felis-tier mismatch — served %q, documented %q", key, s.tier, d.tier)
		}
		if s.setup != d.setup {
			t.Errorf("%s: x-felis-setup-allowed mismatch — served %v, documented %v", key, s.setup, d.setup)
		}
		if !oasSameSet(s.callers, d.callers) {
			t.Errorf("%s: x-felis-callers mismatch — served %v, documented %v",
				key, oasSortedKeys(s.callers), oasSortedKeys(d.callers))
		}
	}
}

func oasServedFacets(t *testing.T) map[string]oasFacet {
	t.Helper()
	a := &API{}
	out := map[string]oasFacet{}
	add := func(method, pattern, face, tier string, setup bool, callers ...Caller) {
		key := method + " " + pattern
		f, ok := out[key]
		if !ok {
			f = oasFacet{faces: map[string]bool{}, callers: map[string]bool{}}
		}
		for _, c := range callers {
			f.callers[string(c)] = true
		}
		f.faces[face] = true
		if f.tier != "" && f.tier != tier {
			t.Fatalf("%s: route table assigns conflicting tiers %q and %q", key, f.tier, tier)
		}
		f.tier = tier
		f.setup = f.setup || setup
		out[key] = f
	}
	for _, rt := range a.internalAPIRoutes() {
		tier := "service"
		if rt.Public {
			tier = "public"
		}
		add(rt.Method, rt.Pattern, "internal", tier, rt.SetupAllowed, rt.Callers...)
	}
	for _, rt := range a.externalAPIRoutes() {
		var tier string
		switch {
		case rt.Public:
			tier = "public"
		case rt.Owner:
			tier = "owner"
		case rt.Admin:
			tier = "admin"
		default:
			tier = "app"
		}
		add(rt.Method, rt.Pattern, "external", tier, rt.SetupAllowed)
	}
	return out
}

func oasDocumentedFacets(t *testing.T) map[string]oasFacet {
	t.Helper()
	path := filepath.Join("..", "..", "api", "openapi.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc oasDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Fatalf("%s: expected OpenAPI 3.1, got openapi: %q", path, doc.OpenAPI)
	}

	out := map[string]oasFacet{}
	for rawPath, item := range doc.Paths {
		for method, op := range item {
			if !oasKnownMethods[strings.ToLower(method)] {
				continue
			}
			key := strings.ToUpper(method) + " " + rawPath
			if len(op.Faces) == 0 {
				t.Errorf("%s: missing x-felis-face", key)
				continue
			}
			if op.Tier == "" {
				t.Errorf("%s: missing x-felis-tier", key)
				continue
			}
			faces := map[string]bool{}
			for _, f := range op.Faces {
				if f != "internal" && f != "external" {
					t.Errorf("%s: x-felis-face has unknown face %q", key, f)
				}
				faces[f] = true
			}
			if _, dup := out[key]; dup {
				t.Errorf("%s: documented more than once", key)
			}
			callers := map[string]bool{}
			for _, c := range op.Callers {
				callers[c] = true
			}
			out[key] = oasFacet{faces: faces, tier: op.Tier, callers: callers, setup: op.Setup}
		}
	}
	return out
}

func oasSameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func oasSortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
