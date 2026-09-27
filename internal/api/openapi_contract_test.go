package api

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"
)

// The route-table parity tests (openapi_test.go) prove docs/openapi.yaml names the
// right {method, path, face, tier, setup}; the struct parity test proves the named
// schemas match their Go structs. Neither sees what a handler actually answers. This
// file does: every request a handler test sends through do() is kept, and once the
// whole package has run TestMain holds each exchange to the document —
//
//   - the operation must list the status that came back (or a default / 4XX / 5XX);
//   - a JSON body must fit the schema documented for that status, and a response
//     object may carry only the properties its schema names;
//   - the JSON request behind a 2xx must fit the documented requestBody, naming
//     only the properties it names.
//
// Requests to a path the document does not have are left to the route parity test.

type contractCall struct {
	method, path string
	reqCT        string
	reqBody      []byte
	status       int
	respCT       string
	respBody     []byte
	at           string // the test line that sent it
}

var contractCalls struct {
	sync.Mutex
	list []contractCall
}

// recordContract keeps one do() exchange for checkContract.
func recordContract(r *http.Request, body string, w *httptest.ResponseRecorder) {
	at := ""
	if _, file, line, ok := runtime.Caller(2); ok {
		at = fmt.Sprintf("%s:%d", file[strings.LastIndex(file, "/")+1:], line)
	}
	contractCalls.Lock()
	defer contractCalls.Unlock()
	contractCalls.list = append(contractCalls.list, contractCall{
		method: r.Method, path: r.URL.Path,
		reqCT: r.Header.Get("Content-Type"), reqBody: []byte(body),
		status: w.Code, respCT: w.Header().Get("Content-Type"), respBody: w.Body.Bytes(),
		at: at,
	})
}

type contractDoc struct {
	// Common lists the answers any operation can give under a condition, declared
	// once at the top of the document instead of under every operation.
	Common     []contractCommon          `json:"x-felis-common-responses"`
	Paths      map[string]map[string]any `json:"paths"`
	Components struct {
		Schemas       map[string]any `json:"schemas"`
		Responses     map[string]any `json:"responses"`
		RequestBodies map[string]any `json:"requestBodies"`
	} `json:"components"`
}

type contractCommon struct {
	Status int `json:"status"`
	// When is any, internal (an operation the internal face serves), session (one
	// that takes the session cookie), setup-locked (a session one a setup-lockdown
	// session may not use) or json-body (one that takes a JSON requestBody).
	When     string         `json:"when"`
	Code     string         `json:"code"`
	Response map[string]any `json:"response"`
}

type contractRoute struct {
	template string
	re       *regexp.Regexp
	literals int
}

// checkContract returns one line per exchange that disagrees with docs/openapi.yaml.
func checkContract(path string, calls []contractCall) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc contractDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	routes := make([]contractRoute, 0, len(doc.Paths))
	for tmpl := range doc.Paths {
		routes = append(routes, compileContractRoute(tmpl))
	}
	// The most literal template wins: /servers/limbo is not /servers/{name}.
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].literals != routes[j].literals {
			return routes[i].literals > routes[j].literals
		}
		return routes[i].template < routes[j].template
	})

	var out []string
	seen := map[string]bool{}
	report := func(c contractCall, format string, args ...any) {
		line := fmt.Sprintf("%s %s → %d (%s): %s", c.method, c.path, c.status, c.at, fmt.Sprintf(format, args...))
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	for _, c := range calls {
		tmpl := ""
		for _, r := range routes {
			if r.re.MatchString(c.path) {
				tmpl = r.template
				break
			}
		}
		if tmpl == "" {
			continue
		}
		op, ok := doc.Paths[tmpl][strings.ToLower(c.method)].(map[string]any)
		if !ok {
			continue
		}
		// A path asked on the face that does not serve it: the route parity test
		// owns which face serves what.
		if c.status == http.StatusNotFound && contractErrorMessage(c.respBody) == "no such endpoint" {
			continue
		}
		v := &contractValidator{doc: &doc}
		resp, ok := contractResponse(op, c.status)
		if !ok {
			resp, ok = contractCommonResponse(&doc, op, c)
		}
		if !ok {
			report(c, "status %d is not documented for %s %s", c.status, c.method, tmpl)
			continue
		}
		resp = v.deref(resp)
		if schema, ok := jsonSchemaOf(resp); ok && isJSON(c.respCT) && len(c.respBody) > 0 {
			var body any
			if err := json.Unmarshal(c.respBody, &body); err != nil {
				report(c, "response is not JSON: %v", err)
			} else {
				for _, e := range v.check(schema, body, "response", true) {
					report(c, "%s", e)
				}
			}
		}
		// decodeJSON reads the body whatever Content-Type says, so a JSON body sent
		// without one (as most handler tests send it) is held to the document too.
		reqJSON := isJSON(c.reqCT) || (c.reqCT == "" && json.Valid(c.reqBody))
		if c.status/100 == 2 && reqJSON && len(c.reqBody) > 0 {
			rb, ok := op["requestBody"].(map[string]any)
			if !ok && c.method == http.MethodGet {
				continue // a body on a GET is ignored, whatever it holds
			}
			if !ok {
				report(c, "a JSON request body was accepted but %s %s documents none", c.method, tmpl)
				continue
			}
			if schema, ok := jsonSchemaOf(v.deref(rb)); ok {
				var body any
				if err := json.Unmarshal(c.reqBody, &body); err == nil {
					for _, e := range v.check(schema, body, "request", true) {
						report(c, "%s", e)
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func compileContractRoute(tmpl string) contractRoute {
	var b strings.Builder
	b.WriteString("^")
	literals := 0
	for _, seg := range strings.Split(strings.TrimPrefix(tmpl, "/"), "/") {
		b.WriteString("/")
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			b.WriteString(`[^/]+`)
			continue
		}
		literals++
		b.WriteString(regexp.QuoteMeta(seg))
	}
	b.WriteString("$")
	return contractRoute{template: tmpl, re: regexp.MustCompile(b.String()), literals: literals}
}

// contractResponse finds the documented response for a status: the exact code, then
// its class (4XX), then default.
func contractResponse(op map[string]any, status int) (map[string]any, bool) {
	responses, _ := op["responses"].(map[string]any)
	for _, k := range []string{strconv.Itoa(status), fmt.Sprintf("%dXX", status/100), "default"} {
		if r, ok := responses[k].(map[string]any); ok {
			return r, true
		}
	}
	return nil, false
}

// contractCommonResponse finds a common answer whose condition the operation meets
// and whose error code, when it names one, is the one that came back.
func contractCommonResponse(doc *contractDoc, op map[string]any, c contractCall) (map[string]any, bool) {
	for _, common := range doc.Common {
		if common.Status != c.status || (common.Code != "" && contractErrorCode(c.respBody) != common.Code) {
			continue
		}
		if contractOpMeets(doc, op, common.When) {
			return common.Response, true
		}
	}
	return nil, false
}

func contractOpMeets(doc *contractDoc, op map[string]any, when string) bool {
	switch when {
	case "any":
		return true
	case "internal":
		faces, _ := op["x-felis-face"].([]any)
		return containsJSON(faces, "internal")
	case "session":
		security, _ := op["security"].([]any)
		for _, req := range security {
			if m, ok := req.(map[string]any); ok {
				if _, ok := m["sessionCookie"]; ok {
					return true
				}
			}
		}
		return false
	case "setup-locked":
		allowed, _ := op["x-felis-setup-allowed"].(bool)
		return !allowed && contractOpMeets(doc, op, "session")
	case "json-body":
		rb, _ := op["requestBody"].(map[string]any)
		v := &contractValidator{doc: doc}
		_, ok := jsonSchemaOf(v.deref(rb))
		return rb != nil && ok
	}
	return false
}

func contractErrorCode(body []byte) string {
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Error.Code
}

func contractErrorMessage(body []byte) string {
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Error.Message
}

func jsonSchemaOf(r map[string]any) (map[string]any, bool) {
	content, _ := r["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	schema, ok := media["schema"].(map[string]any)
	return schema, ok
}

func isJSON(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

type contractValidator struct{ doc *contractDoc }

// deref follows a local $ref (#/components/<kind>/<name>) until it reaches the object.
func (v *contractValidator) deref(n map[string]any) map[string]any {
	for i := 0; i < 16; i++ {
		ref, ok := n["$ref"].(string)
		if !ok {
			return n
		}
		parts := strings.Split(strings.TrimPrefix(ref, "#/components/"), "/")
		if len(parts) != 2 {
			return map[string]any{"x-unresolved": ref}
		}
		name, _ := url.PathUnescape(parts[1])
		var table map[string]any
		switch parts[0] {
		case "schemas":
			table = v.doc.Components.Schemas
		case "responses":
			table = v.doc.Components.Responses
		case "requestBodies":
			table = v.doc.Components.RequestBodies
		}
		next, ok := table[name].(map[string]any)
		if !ok {
			return map[string]any{"x-unresolved": ref}
		}
		n = next
	}
	return n
}

// check returns where value leaves schema. strict also refuses object properties
// the schema does not name, unless it allows additional ones: a response field and
// a request field the document misnames (display_name for displayName) both show.
func (v *contractValidator) check(schema map[string]any, value any, at string, strict bool) []string {
	schema = v.deref(schema)
	if ref, ok := schema["x-unresolved"]; ok {
		return []string{fmt.Sprintf("%s: unresolved $ref %v", at, ref)}
	}
	var errs []string
	if all, ok := schema["allOf"].([]any); ok {
		for _, s := range all {
			if m, ok := s.(map[string]any); ok {
				errs = append(errs, v.check(m, value, at, false)...)
			}
		}
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		alts, ok := schema[key].([]any)
		if !ok {
			continue
		}
		matched := false
		for _, s := range alts {
			if m, ok := s.(map[string]any); ok && len(v.check(m, value, at, strict)) == 0 {
				matched = true
				break
			}
		}
		if !matched {
			errs = append(errs, fmt.Sprintf("%s: matches none of %s", at, key))
		}
	}
	if types := schemaTypes(schema); len(types) > 0 && !types[jsonKind(value)] &&
		!(jsonKind(value) == "integer" && types["number"]) {
		return append(errs, fmt.Sprintf("%s: is %s, documented as %s", at, jsonKind(value), typeList(types)))
	}
	if enum, ok := schema["enum"].([]any); ok && !containsJSON(enum, value) {
		errs = append(errs, fmt.Sprintf("%s: %v is not one of %v", at, value, enum))
	}
	if c, ok := schema["const"]; ok && !reflect.DeepEqual(c, value) {
		errs = append(errs, fmt.Sprintf("%s: %v is not the documented %v", at, value, c))
	}
	switch val := value.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		req, _ := schema["required"].([]any)
		for _, r := range req {
			if name, _ := r.(string); name != "" {
				if _, ok := val[name]; !ok {
					errs = append(errs, fmt.Sprintf("%s: required property %q is missing", at, name))
				}
			}
		}
		extra, hasExtra := schema["additionalProperties"]
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := props[k].(map[string]any); ok {
				errs = append(errs, v.check(ps, val[k], at+"."+k, strict)...)
				continue
			}
			switch e := extra.(type) {
			case map[string]any:
				errs = append(errs, v.check(e, val[k], at+"."+k, strict)...)
			case bool:
				if !e {
					errs = append(errs, fmt.Sprintf("%s: property %q is not documented", at, k))
				}
			default:
				if strict && !hasExtra && props != nil {
					errs = append(errs, fmt.Sprintf("%s: property %q is not documented", at, k))
				}
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range val {
				errs = append(errs, v.check(items, item, fmt.Sprintf("%s[%d]", at, i), strict)...)
			}
		}
	}
	return errs
}

func schemaTypes(s map[string]any) map[string]bool {
	out := map[string]bool{}
	switch t := s["type"].(type) {
	case string:
		out[t] = true
	case []any:
		for _, x := range t {
			if name, ok := x.(string); ok {
				out[name] = true
			}
		}
	}
	if n, _ := s["nullable"].(bool); n && len(out) > 0 {
		out["null"] = true
	}
	return out
}

func typeList(types map[string]bool) string {
	names := make([]string, 0, len(types))
	for t := range types {
		names = append(names, t)
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

func jsonKind(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if x == float64(int64(x)) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func containsJSON(list []any, v any) bool {
	for _, x := range list {
		if reflect.DeepEqual(x, v) {
			return true
		}
	}
	return false
}

// The checker is itself held to known drifts: each synthetic exchange below either
// disagrees with docs/openapi.yaml in exactly the way named, or agrees with it.
func TestContractCheckerCatchesDrift(t *testing.T) {
	jsonCT := "application/json"
	errBody := func(code, msg string) []byte {
		return []byte(`{"error":{"code":"` + code + `","message":"` + msg + `","request_id":"r"}}`)
	}
	calls := []contractCall{
		// An empty fleet sent as null.
		{method: "GET", path: "/api/v1/servers", status: 200, respCT: jsonCT, respBody: []byte(`{"servers":null}`), at: "a"},
		// A property the schema does not name.
		{method: "GET", path: "/api/v1/servers", status: 200, respCT: jsonCT, respBody: []byte(`{"servers":[],"extra":1}`), at: "b"},
		// A status the operation does not list.
		{method: "GET", path: "/api/v1/me", status: 418, respCT: jsonCT, respBody: errBody("teapot", "no"), at: "c"},
		// wrong_caller is an internal-face answer; /me is external only.
		{method: "GET", path: "/api/v1/me", status: 403, respCT: jsonCT, respBody: errBody("wrong_caller", "no"), at: "d"},
		// ...and on an internal operation it is a common answer.
		{method: "GET", path: "/api/v1/servers", status: 403, respCT: jsonCT, respBody: errBody("wrong_caller", "no"), at: "e"},
		// /me stays open during the setup lockdown, so setup_required is not its answer...
		{method: "GET", path: "/api/v1/me", status: 403, respCT: jsonCT, respBody: errBody("setup_required", "no"), at: "f"},
		// ...while a session operation outside the allow-list may give it.
		{method: "GET", path: "/api/v1/servers/survival/files", status: 403, respCT: jsonCT, respBody: errBody("setup_required", "no"), at: "g"},
		// A path this face does not serve belongs to the route parity test.
		{method: "GET", path: "/api/v1/me", status: 404, respCT: jsonCT, respBody: errBody("not_found", "no such endpoint"), at: "h"},
		// The request behind a 2xx must fit the requestBody.
		{method: "POST", path: "/api/v1/account/link/verify", reqCT: jsonCT, reqBody: []byte(`{"code":5}`),
			status: 200, respCT: jsonCT, respBody: []byte(`{"linked":true,"mc_uuid":"u","auth_source":"mojang"}`), at: "i"},
		// A response field outside its enum.
		{method: "POST", path: "/api/v1/account/link/verify", reqCT: jsonCT, reqBody: []byte(`{"code":"ABC"}`),
			status: 200, respCT: jsonCT, respBody: []byte(`{"linked":true,"mc_uuid":"u","auth_source":""}`), at: "j"},
		// A request property the requestBody does not name: the document once had
		// display_name where the handler reads displayName. Sent with no Content-Type,
		// which the handler decodes all the same.
		{method: "POST", path: "/api/v1/servers", reqBody: []byte(`{"name":"x1","subdomain":"x1","display_name":"X"}`),
			status: 201, respCT: jsonCT, respBody: []byte(`{"name":"x1","subdomain":"x1","desiredState":"Stopped"}`), at: "k"},
	}
	got, err := checkContract("../../docs/openapi.yaml", calls)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`GET /api/v1/me → 403 (d): status 403 is not documented for GET /api/v1/me`,
		`GET /api/v1/me → 403 (f): status 403 is not documented for GET /api/v1/me`,
		`GET /api/v1/me → 418 (c): status 418 is not documented for GET /api/v1/me`,
		`GET /api/v1/servers → 200 (a): response.servers: is null, documented as array`,
		`GET /api/v1/servers → 200 (b): response: property "extra" is not documented`,
		`POST /api/v1/account/link/verify → 200 (i): request.code: is integer, documented as string`,
		`POST /api/v1/account/link/verify → 200 (j): response.auth_source:  is not one of [mojang thirdparty]`,
		`POST /api/v1/servers → 201 (k): request: property "display_name" is not documented`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("violations:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
