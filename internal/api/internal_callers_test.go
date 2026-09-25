package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func callerTokensForTest() CallerTokens {
	return CallerTokens{
		CallerVelocity: "tok-velocity",
		CallerLimbo:    "tok-limbo",
		CallerBuild:    "tok-build",
		CallerOps:      "tok-ops",
	}
}

func bearer(tok string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tok, "Content-Type": "application/json"}
}

// Each token answers with its own caller, and nothing else gets in.
func TestCallerTokensNameTheCaller(t *testing.T) {
	c := callerTokensForTest()
	for tok, want := range map[string]Caller{
		"tok-velocity": CallerVelocity,
		"tok-limbo":    CallerLimbo,
		"tok-build":    CallerBuild,
		"tok-ops":      CallerOps,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		got, err := c.Authenticate(r)
		if err != nil || got != want {
			t.Errorf("%s: got (%q, %v), want %q", tok, got, err, want)
		}
	}
	for _, header := range []string{"", "Bearer tok-velocityX", "Bearer tok-veloci", "Basic tok-ops"} {
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got, err := c.Authenticate(r); err == nil {
			t.Errorf("%q authenticated as %q", header, got)
		}
	}

	// A caller whose Secret is missing has an empty token; that must not turn into
	// "any empty-ish credential passes".
	partial := CallerTokens{CallerVelocity: "tok-velocity", CallerBuild: ""}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer  ")
	if got, err := partial.Authenticate(r); err == nil {
		t.Fatalf("blank bearer authenticated as %q", got)
	}
}

func TestNewCallerTokensRefusesAmbiguousSets(t *testing.T) {
	if _, err := NewCallerTokens(map[Caller]string{CallerVelocity: "a", CallerLimbo: "b", CallerBuild: "", CallerOps: "c"}); err != nil {
		t.Fatalf("distinct tokens refused: %v", err)
	}
	_, err := NewCallerTokens(map[Caller]string{CallerVelocity: "same", CallerBuild: "same"})
	if err == nil || !strings.Contains(err.Error(), "same value") {
		t.Fatalf("shared value: err = %v, want a same-value refusal", err)
	}
}

// The caller scopes the gap asked for, spelled out rather than read back from the
// route table: the build token reads a submission's context and nothing else, only
// the proxy and the login gate mint link codes, only the proxy approves an
// op-login, and only the on-node console asks for a break-glass backup.
func TestInternalRoutesServeOnlyTheirCallers(t *testing.T) {
	a := newTestAPI(newFakeRepo(), newFakeCluster())
	a.Internal = callerTokensForTest()
	h := a.InternalHandler()

	cases := []struct {
		method, path string
		allowed      []Caller
	}{
		{"GET", "/api/v1/servers", []Caller{CallerVelocity}},
		{"GET", "/api/v1/internal/submissions/sub-1/context", []Caller{CallerBuild}},
		{"POST", "/api/v1/internal/account/link/code", []Caller{CallerVelocity, CallerLimbo}},
		{"GET", "/api/v1/internal/account/link/status/00000000-0000-0000-0000-000000000001", []Caller{CallerVelocity, CallerLimbo}},
		{"GET", "/api/v1/internal/player/blacklist/00000000-0000-0000-0000-000000000001", []Caller{CallerVelocity, CallerLimbo}},
		{"POST", "/api/v1/internal/op-login/req-1/approve", []Caller{CallerVelocity}},
		{"GET", "/api/v1/internal/op-login/pending", []Caller{CallerVelocity}},
		{"POST", "/api/v1/internal/account/migrate/start", []Caller{CallerVelocity}},
		{"POST", "/api/v1/internal/player/reclaim", []Caller{CallerVelocity}},
		{"POST", "/api/v1/internal/servers/survival/wake", []Caller{CallerVelocity}},
		{"POST", "/api/v1/internal/servers/survival/claim", []Caller{CallerVelocity}},
		{"POST", "/api/v1/internal/servers/survival/backup", []Caller{CallerOps}},
	}
	tokens := map[Caller]string{CallerVelocity: "tok-velocity", CallerLimbo: "tok-limbo", CallerBuild: "tok-build", CallerOps: "tok-ops"}
	for _, tc := range cases {
		for caller, tok := range tokens {
			allowed := false
			for _, c := range tc.allowed {
				allowed = allowed || c == caller
			}
			w := do(h, tc.method, tc.path, "{}", bearer(tok))
			refused := w.Code == http.StatusForbidden && decodeErr(t, w) == "wrong_caller"
			if allowed && (refused || w.Code == http.StatusUnauthorized) {
				t.Errorf("%s %s as %s: refused (%d %s), want it served", tc.method, tc.path, caller, w.Code, w.Body.String())
			}
			if !allowed && !refused {
				t.Errorf("%s %s as %s: got %d %s, want 403 wrong_caller", tc.method, tc.path, caller, w.Code, w.Body.String())
			}
		}
	}
}

// The audit names the caller whose token asked for the action.
func TestInternalAuditNamesTheCaller(t *testing.T) {
	repo := newFakeRepo()
	a := newTestAPI(repo, newFakeCluster())
	a.Internal = callerTokensForTest()
	r := httptest.NewRequest("POST", "/", nil)
	if got := internalSource(r); got != "internal" {
		t.Fatalf("no caller: source = %q, want internal", got)
	}
	var seen string
	probe := a.requireInternal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = internalSource(r)
	}))
	r.Header.Set("Authorization", "Bearer tok-limbo")
	probe.ServeHTTP(httptest.NewRecorder(), r)
	if seen != "internal:limbo" {
		t.Fatalf("source = %q, want internal:limbo", seen)
	}
}

// A route added to the internal table without saying who calls it would be open
// to every token; the face refuses to build instead. Callers on an external route
// would mean nothing, so that is refused too.
func TestBuildFaceRequiresCallersOnInternalRoutes(t *testing.T) {
	a := newTestAPI(newFakeRepo(), newFakeCluster())
	noop := func(w http.ResponseWriter, r *http.Request) {}
	pass := func(h http.Handler) http.Handler { return h }
	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: built without complaint", name)
			}
		}()
		fn()
	}
	mustPanic("internal route without callers", func() {
		a.buildFace("internal", []apiRoute{{Method: "GET", Pattern: "/api/v1/internal/x", h: noop}}, pass)
	})
	mustPanic("external route with callers", func() {
		a.buildFace("external", []apiRoute{{Method: "GET", Pattern: "/api/v1/x", Callers: []Caller{CallerOps}, h: noop}}, pass)
	})
	// Public internal routes (probes, hasJoined) carry no token and list no callers.
	a.buildFace("internal", []apiRoute{{Method: "GET", Pattern: "/readyz", Public: true, h: noop}}, pass)
}
