package api

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// This is the §28 #7 OpenAPI parity gate. docs/openapi.yaml is checked against
// the routes the handlers are actually built from — the same internalAPIRoutes /
// externalAPIRoutes tables in api.go — in BOTH directions and including each
// route's Zero-Trust tier. So three things cannot silently drift:
//
//   - a route served but undocumented (or documented but unserved) fails the build;
//   - a route whose documented face set disagrees with where it is mounted fails;
//   - a route whose documented tier disagrees with its computed tier fails — this
//     is the §14 four-power guard: an admin route quietly demoted to app, or an
//     internal service route re-faced as external, breaks `go test ./...`.
//
// http.ServeMux exposes no way to enumerate its patterns, so a doc-vs-mux probe is
// impossible; reading the shared route table is the only exact check (api.go).

// oasKnownMethods is the set of OpenAPI path-item keys that denote operations.
// Any other key (summary, parameters, description, servers, ...) is ignored.
var oasKnownMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true,
	"delete": true, "head": true, "options": true, "trace": true,
}

// oasDoc is the slice of docs/openapi.yaml this test reads. Body schemas and the
// rest of the document are intentionally not modelled — only the surface the
// handlers must agree with.
type oasDoc struct {
	OpenAPI string                      `json:"openapi"`
	Paths   map[string]map[string]oasOp `json:"paths"`
}

type oasOp struct {
	Faces []string `json:"x-felis-face"`
	Tier  string   `json:"x-felis-tier"`
}

// oasFacet is the classification of one {method, path}: which face(s) serve it
// and at which Zero-Trust tier.
type oasFacet struct {
	faces map[string]bool
	tier  string
}

func TestOpenAPIMatchesServedRoutes(t *testing.T) {
	served := oasServedFacets(t)
	documented := oasDocumentedFacets(t)

	// 1. Exact {method, path} set parity, both directions.
	for key := range served {
		if _, ok := documented[key]; !ok {
			t.Errorf("route SERVED but not documented in docs/openapi.yaml: %s", key)
		}
	}
	for key := range documented {
		if _, ok := served[key]; !ok {
			t.Errorf("route DOCUMENTED in docs/openapi.yaml but not served: %s", key)
		}
	}

	// 2. For every shared route, face set and tier must agree exactly.
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
	}
}

// oasServedFacets derives the live route surface from the route tables the
// handlers are built from, keyed by "METHOD /path". The tier is computed the same
// way buildFace treats the route: Public -> public; internal-face -> service;
// external-face Admin -> admin; otherwise app. /healthz is added by both tables,
// so its face set merges to {internal, external} and its tier must agree (public).
func oasServedFacets(t *testing.T) map[string]oasFacet {
	t.Helper()
	a := &API{}
	out := map[string]oasFacet{}
	add := func(method, pattern, face, tier string) {
		key := method + " " + pattern
		f, ok := out[key]
		if !ok {
			f = oasFacet{faces: map[string]bool{}}
		}
		f.faces[face] = true
		if f.tier != "" && f.tier != tier {
			t.Fatalf("%s: route table assigns conflicting tiers %q and %q", key, f.tier, tier)
		}
		f.tier = tier
		out[key] = f
	}
	for _, rt := range a.internalAPIRoutes() {
		tier := "service"
		if rt.Public {
			tier = "public"
		}
		add(rt.Method, rt.Pattern, "internal", tier)
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
		add(rt.Method, rt.Pattern, "external", tier)
	}
	return out
}

// oasDocumentedFacets parses docs/openapi.yaml into the same shape. An operation
// missing x-felis-face or x-felis-tier is a failure, not a skip: a new route
// cannot be documented without classifying its face and tier.
func oasDocumentedFacets(t *testing.T) map[string]oasFacet {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "openapi.yaml")
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
			out[key] = oasFacet{faces: faces, tier: op.Tier}
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
