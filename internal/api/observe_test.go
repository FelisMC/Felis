package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func counterValue(t *testing.T, labels ...string) float64 {
	t.Helper()
	var m dto.Metric
	if err := httpRequestsTotal.WithLabelValues(labels...).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

// accessLines decodes the JSON access-log lines written to buf.
func accessLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("access log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// TestObserveLabelsByRoutePattern pins the access log and the request series to
// the matched route pattern: a server name never becomes a label, a path no route
// matched is "unmatched", the caller is named, and the probes stay out of the log.
func TestObserveLabelsByRoutePattern(t *testing.T) {
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Running"}
	a := newTestAPI(repo, cl)
	a.External = staticExternal{p: &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}}
	var buf bytes.Buffer
	a.AccessLog = slog.New(slog.NewJSONHandler(&buf, nil))
	h := a.ExternalHandler()

	const route = "/api/v1/servers/{name}/status"
	before := counterValue(t, "external", "GET", route, "200")
	if w := do(h, "GET", "/api/v1/servers/survival/status", "", map[string]string{"X-Request-Id": "trace-1"}); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if got := counterValue(t, "external", "GET", route, "200") - before; got != 1 {
		t.Fatalf("felis_http_requests_total{route=%q} grew by %v, want 1", route, got)
	}

	unmatched := counterValue(t, "external", "GET", routeUnmatched, "404")
	if w := do(h, "GET", "/api/v1/no/such/thing", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown path = %d, want 404", w.Code)
	}
	if got := counterValue(t, "external", "GET", routeUnmatched, "404") - unmatched; got != 1 {
		t.Fatalf("unknown path counted %v times under %q, want 1", got, routeUnmatched)
	}

	do(h, "GET", "/healthz", "", nil)

	lines := accessLines(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("access log has %d lines, want 2 (the probe is quiet): %s", len(lines), buf.String())
	}
	first := lines[0]
	for k, want := range map[string]any{
		"msg": "request", "face": "external", "method": "GET", "route": route,
		"path": "/api/v1/servers/survival/status", "status": float64(200),
		"request_id": "trace-1", "principal": "owner1",
	} {
		if first[k] != want {
			t.Errorf("access log %s = %v, want %v", k, first[k], want)
		}
	}
	if first["bytes"].(float64) <= 0 {
		t.Errorf("access log bytes = %v, want the body size", first["bytes"])
	}
	if lines[1]["route"] != routeUnmatched || lines[1]["status"] != float64(404) {
		t.Errorf("unknown path logged as %v", lines[1])
	}
}

// TestNoRouteEnvelopes: a path no route serves is a 404 envelope, and a known path
// asked with the wrong method is a 405 that names the methods it does take.
func TestNoRouteEnvelopes(t *testing.T) {
	a := newTestAPI(newFakeRepo(), newFakeCluster())
	a.External = staticExternal{p: &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}}

	for _, face := range []struct {
		name string
		h    http.Handler
	}{{"external", a.ExternalHandler()}, {"internal", a.InternalHandler()}} {
		w := do(face.h, "GET", "/api/v1/definitely-not-a-route", "", nil)
		if w.Code != http.StatusNotFound || errCode(w.Body.Bytes()) != "not_found" {
			t.Fatalf("%s unknown path = %d %s, want 404 not_found", face.name, w.Code, w.Body)
		}
		w = do(face.h, "DELETE", "/healthz", "", nil)
		if w.Code != http.StatusMethodNotAllowed || errCode(w.Body.Bytes()) != "method_not_allowed" {
			t.Fatalf("%s DELETE /healthz = %d %s, want 405 method_not_allowed", face.name, w.Code, w.Body)
		}
		if got := w.Header().Get("Allow"); got != "GET" {
			t.Fatalf("%s DELETE /healthz Allow = %q, want GET", face.name, got)
		}
	}

	// An authenticated route answers 405 the same way.
	w := do(a.ExternalHandler(), "PUT", "/api/v1/servers/survival/status", "", nil)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET" {
		t.Fatalf("PUT status = %d Allow %q, want 405 Allow GET", w.Code, w.Header().Get("Allow"))
	}
}

// TestOversizedJSONBodyIs413: a JSON body past maxBodyBytes is refused as too
// large, where it used to read as a malformed one.
func TestOversizedJSONBodyIs413(t *testing.T) {
	r := httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"`+strings.Repeat("a", maxBodyBytes)+`"}`))
	w := httptest.NewRecorder()
	var v struct{ Name string }
	err := decodeJSON(w, r, &v)
	writeError(w, r, err)
	if w.Code != http.StatusRequestEntityTooLarge || errCode(w.Body.Bytes()) != "too_large" {
		t.Fatalf("oversized body = %d %s, want 413 too_large", w.Code, w.Body)
	}
}

// TestSecurityHeaders: every API answer forbids sniffing, framing and referrers,
// and HSTS goes out on what the edge served over HTTPS.
func TestSecurityHeaders(t *testing.T) {
	h := newTestAPI(newFakeRepo(), newFakeCluster()).ExternalHandler()

	w := do(h, "GET", "/healthz", "", nil)
	for k, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got := w.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS on a direct request = %q, want none", got)
	}

	// The tunnel reaches the origin over TLS as well.
	r := httptest.NewRequest("GET", "https://console."+testRoot+"/healthz", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := w.Header().Get("Strict-Transport-Security"); got == "" {
		t.Error("no HSTS behind the TLS edge")
	}
}

// TestCrossSiteWritesRefused: a browser write from another site (a sibling
// subdomain included) is refused before auth; reads, same-origin writes and
// non-browser callers pass.
func TestCrossSiteWritesRefused(t *testing.T) {
	a := newTestAPI(newFakeRepo(), newFakeCluster())
	a.External = staticExternal{p: &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}}
	h := a.ExternalHandler()
	const host = "console." + testRoot

	send := func(method string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/v1/servers/survival/stop", nil)
		r.Host = host
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	for _, c := range []struct {
		name string
		hdr  map[string]string
	}{
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"sibling subdomain", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://evil." + testRoot}},
		{"foreign Origin only", map[string]string{"Origin": "https://evil.example.org"}},
		{"null Origin", map[string]string{"Origin": "null"}},
	} {
		w := send("POST", c.hdr)
		if w.Code != http.StatusForbidden || errCode(w.Body.Bytes()) != "cross_site" {
			t.Errorf("%s: POST = %d %s, want 403 cross_site", c.name, w.Code, w.Body)
		}
	}

	for _, c := range []struct {
		name string
		hdr  map[string]string
	}{
		{"same-origin", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://" + host}},
		{"typed URL", map[string]string{"Sec-Fetch-Site": "none"}},
		{"Origin matches Host", map[string]string{"Origin": "https://" + host}},
		{"no browser headers", nil},
	} {
		if w := send("POST", c.hdr); w.Code == http.StatusForbidden && errCode(w.Body.Bytes()) == "cross_site" {
			t.Errorf("%s: POST refused as cross-site", c.name)
		}
	}

	if w := send("GET", map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code == http.StatusForbidden && errCode(w.Body.Bytes()) == "cross_site" {
		t.Error("a cross-site GET was refused; only writes are fenced")
	}
}

// TestBodyDeadlineCutsATrickle runs a real server: a body that stops arriving is
// cut off at the grace period, and a body that did arrive clears the deadline so
// the request context outlives it while the handler keeps working.
func TestBodyDeadlineCutsATrickle(t *testing.T) {
	oldGrace, oldRate := bodyGrace, bodyMinRate
	bodyGrace, bodyMinRate = 150*time.Millisecond, 1<<20
	defer func() { bodyGrace, bodyMinRate = oldGrace, oldRate }()

	type outcome struct {
		readErr error
		ctxErr  error
	}
	results := make(chan outcome, 1)
	srv := httptest.NewServer(withBodyDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err == nil {
			time.Sleep(3 * bodyGrace)
		}
		results <- outcome{readErr: err, ctxErr: r.Context().Err()}
	})))
	defer srv.Close()

	// A whole body, then a handler slower than the grace period.
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if o := <-results; o.readErr != nil || o.ctxErr != nil {
		t.Fatalf("complete body: read err %v, ctx err %v; want both nil", o.readErr, o.ctxErr)
	}

	// A body that sends a few bytes and stalls.
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() { _, _ = pw.Write([]byte(`{"slow":`)) }()
	req, _ := http.NewRequest("POST", srv.URL, pr)
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case o := <-results:
		if o.readErr == nil {
			t.Fatal("a stalled body read to the end")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled body was never cut off")
	}
}

// TestStatusTrimmedForOthers: GET /servers/{name}/status gives the owner the whole
// record and any other signed-in user what the game's server list shows.
func TestStatusTrimmedForOthers(t *testing.T) {
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", Subdomain: "survival", Phase: "Running", Ready: true,
		Image: "paper-1.21", JavaMemory: "4G", EndpointAddress: "10.0.0.7:25565", PlayersOnline: 2, PlayersMax: 20}

	status := func(uid string) map[string]any {
		a := newTestAPI(repo, cl)
		a.External = staticExternal{p: &Principal{UserID: uid, Email: uid + "@example.net", Role: "user"}}
		w := do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/status", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d %s", uid, w.Code, w.Body)
		}
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		return m
	}

	if m := status("owner1"); m["image"] != "paper-1.21" || m["endpointAddress"] != "10.0.0.7:25565" {
		t.Fatalf("owner sees %v, want the whole record", m)
	}
	m := status("stranger")
	for _, hidden := range []string{"image", "javaMemory", "endpointAddress"} {
		if _, ok := m[hidden]; ok {
			t.Errorf("a stranger sees %s", hidden)
		}
	}
	if m["phase"] != "Running" || m["playersOnline"] != float64(2) {
		t.Errorf("a stranger sees %v, want phase and player count", m)
	}
}
