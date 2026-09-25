package api

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/submit"
	"felis.lolicon.best/internal/updates"
	"sigs.k8s.io/yaml"
)

// TestOpenAPISchemasMatchWireStructs keeps docs/openapi.yaml honest about the
// response bodies. Each named schema is compared with the Go struct the handler
// actually encodes: the property set must equal the struct's JSON field set, and
// `required` must list exactly the fields that are always on the wire (no
// omitempty). The panel's types are checked against the same schemas at compile
// time (panel/src/lib/types.parity.ts), so a field added here without the docs
// fails in Go, and one added to the docs without the panel fails in tsc.
func TestOpenAPISchemasMatchWireStructs(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]schemaDoc `json:"schemas"`
		} `json:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}

	pairs := map[string]any{
		"ServerInfo":            ServerInfo{},
		"FleetServer":           fleetServerView{},
		"MyServerView":          MyServerView{},
		"BackupView":            BackupView{},
		"Build":                 build.Build{},
		"Image":                 build.Image{},
		"BuildScan":             buildScanView{},
		"ScanSummary":           build.ScanSummary{},
		"ScanPolicy":            build.ScanPolicy{},
		"ScanFinding":           build.ScanFinding{},
		"Submission":            submissionView{},
		"ContextUploadProgress": submit.UploadProgress{},
		"UserView":              UserView{},
		"UserDetail":            UserDetail{},
		"QuotaView":             QuotaView{},
		"SessionView":           SessionView{},
		"PasskeyCredential":     passkeyCredentialView{},
		"UpdateWindow":          updateWindow{},
		"DBBackupStatus":        dbBackupView{},
		"UpdateReport":          updateReportView{},
		"UpdateComponent":       updates.ComponentStatus{},
	}
	for name, v := range pairs {
		s, ok := doc.Components.Schemas[name]
		if !ok {
			t.Errorf("openapi.yaml has no components.schemas.%s", name)
			continue
		}
		compareSchema(t, name, flatten(s, doc.Components.Schemas), reflect.TypeOf(v))
	}
}

type schemaDoc struct {
	Ref        string               `json:"$ref"`
	AllOf      []schemaDoc          `json:"allOf"`
	Type       any                  `json:"type"`
	Required   []string             `json:"required"`
	Properties map[string]schemaDoc `json:"properties"`
	Items      *schemaDoc           `json:"items"`
}

// flatten resolves a top-level $ref and merges allOf parts into one object
// schema, which is how a Go struct embedding another one is documented.
func flatten(s schemaDoc, all map[string]schemaDoc) schemaDoc {
	if s.Ref != "" {
		s = all[strings.TrimPrefix(s.Ref, "#/components/schemas/")]
	}
	if len(s.AllOf) == 0 {
		return s
	}
	out := schemaDoc{Properties: map[string]schemaDoc{}}
	for _, part := range append(s.AllOf, schemaDoc{Required: s.Required, Properties: s.Properties}) {
		part = flatten(part, all)
		out.Required = append(out.Required, part.Required...)
		for k, v := range part.Properties {
			out.Properties[k] = v
		}
	}
	return out
}

type wireField struct {
	omitempty bool
	typ       reflect.Type
}

// wireFields lists the JSON fields encoding/json emits for t, following
// embedded structs the way the encoder does.
func wireFields(t reflect.Type) map[string]wireField {
	out := map[string]wireField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			for k, v := range wireFields(f.Type) {
				out[k] = v
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = wireField{omitempty: strings.Contains(","+opts+",", ",omitempty,"), typ: f.Type}
	}
	return out
}

func structOf(t reflect.Type) (reflect.Type, bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	return t, t.Kind() == reflect.Struct && t.PkgPath() != "time"
}

func compareSchema(t *testing.T, path string, s schemaDoc, typ reflect.Type) {
	t.Helper()
	fields := wireFields(typ)
	var missing, extra, notRequired, wronglyRequired []string
	for name, f := range fields {
		if _, ok := s.Properties[name]; !ok {
			missing = append(missing, name)
		}
		if !f.omitempty && !contains(s.Required, name) {
			notRequired = append(notRequired, name)
		}
	}
	for name := range s.Properties {
		if _, ok := fields[name]; !ok {
			extra = append(extra, name)
		}
	}
	for _, name := range s.Required {
		if f, ok := fields[name]; ok && f.omitempty {
			wronglyRequired = append(wronglyRequired, name)
		}
	}
	report := func(what string, names []string) {
		if len(names) > 0 {
			sort.Strings(names)
			t.Errorf("%s: %s: %s", path, what, strings.Join(names, ", "))
		}
	}
	report("sent by Go but missing from openapi.yaml", missing)
	report("documented but never sent by Go", extra)
	report("always sent but not in required", notRequired)
	report("required but omitted when empty", wronglyRequired)

	// Nested objects (an object property, or an array of objects) are held to
	// the same rule when the schema spells their properties out.
	for name, f := range fields {
		p, ok := s.Properties[name]
		if !ok {
			continue
		}
		inner, isStruct := structOf(f.typ)
		if !isStruct {
			continue
		}
		switch {
		case len(p.Properties) > 0:
			compareSchema(t, path+"."+name, p, inner)
		case p.Items != nil && len(p.Items.Properties) > 0:
			compareSchema(t, path+"."+name+"[]", *p.Items, inner)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
