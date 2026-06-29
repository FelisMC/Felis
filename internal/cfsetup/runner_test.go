package cfsetup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These exercise ExecRunner.VerifyAPIToken directly against an httptest server.
// This is the load-bearing coverage for "is the CF path usable": Setup only runs
// the pre-flight token check when the runner satisfies apiTokenVerifier, and the
// existing TestSetupVerifiesAPITokenBeforeCloudflareMutations only proves a *fake*
// runner is consulted — it says nothing about whether the production ExecRunner
// actually verifies anything. Without these, the token never gets checked before
// the tunnel and DNS are created on a real account.

// TestExecRunnerVerifyAPITokenHitsAccessApps confirms the happy path probes the
// exact account + Access-read permission Setup needs, with the Bearer token, and
// returns nil when Cloudflare answers success.
func TestExecRunnerVerifyAPITokenHitsAccessApps(t *testing.T) {
	const account = "0123456789abcdef0123456789abcdef"
	var gotPath, gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"result":[]}`))
	}))
	defer srv.Close()

	r := &ExecRunner{APIToken: "cfat_secret", AccountID: account, APIBase: srv.URL}
	if err := r.VerifyAPIToken(context.Background()); err != nil {
		t.Fatalf("VerifyAPIToken on a good token = %v, want nil", err)
	}
	if want := "/accounts/" + account + "/access/apps"; gotPath != want {
		t.Fatalf("verify hit path %q, want %q", gotPath, want)
	}
	if !strings.Contains(gotQuery, "per_page=1") {
		t.Fatalf("verify query = %q, want it to request a single page (per_page=1)", gotQuery)
	}
	if gotAuth != "Bearer cfat_secret" {
		t.Fatalf("verify Authorization = %q, want Bearer token", gotAuth)
	}
}

// TestExecRunnerVerifyAPITokenSurfaces401 locks that an invalid/expired token is
// reported with the actionable permission checklist apiGet renders — this is the
// message the operator sees BEFORE anything is created, which is the whole point.
func TestExecRunnerVerifyAPITokenSurfaces401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`))
	}))
	defer srv.Close()

	r := &ExecRunner{APIToken: "bad", AccountID: "0123456789abcdef0123456789abcdef", APIBase: srv.URL}
	err := r.VerifyAPIToken(context.Background())
	if err == nil {
		t.Fatal("VerifyAPIToken on a 401 = nil, want an error")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Bearer API Token") {
		t.Fatalf("401 error should carry the actionable checklist, got: %v", err)
	}
}

// TestExecRunnerVerifyAPITokenSurfaces403 locks that a wrong-account or
// under-permissioned token names the account in the guidance.
func TestExecRunnerVerifyAPITokenSurfaces403(t *testing.T) {
	const account = "ffffffffffffffffffffffffffffffff"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Unauthorized to access requested resource"}]}`))
	}))
	defer srv.Close()

	r := &ExecRunner{APIToken: "scoped-wrong", AccountID: account, APIBase: srv.URL}
	err := r.VerifyAPIToken(context.Background())
	if err == nil {
		t.Fatal("VerifyAPIToken on a 403 = nil, want an error")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), account) {
		t.Fatalf("403 error should name the account, got: %v", err)
	}
}

// TestExecRunnerVerifyAPITokenPreflight confirms empty credentials fail without a
// network call — the http handler would panic the test if it were reached.
func TestExecRunnerVerifyAPITokenPreflight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("VerifyAPIToken must not hit the wire when credentials are empty")
	}))
	defer srv.Close()

	cases := []struct{ token, account string }{
		{"", "0123456789abcdef0123456789abcdef"},
		{"cfat_x", ""},
	}
	for _, c := range cases {
		r := &ExecRunner{APIToken: c.token, AccountID: c.account, APIBase: srv.URL}
		if err := r.VerifyAPIToken(context.Background()); err == nil {
			t.Fatalf("VerifyAPIToken(token=%q account=%q) = nil, want a pre-flight error", c.token, c.account)
		}
	}
}

// TestExecRunnerSatisfiesAPITokenVerifier is a compile-time-ish guard that the
// production runner actually implements the seam Setup probes for. If someone
// renames or changes the method signature, Setup would silently skip the check
// again (the bug this whole effort fixes); this fails loudly instead.
func TestExecRunnerSatisfiesAPITokenVerifier(t *testing.T) {
	var r Runner = &ExecRunner{}
	if _, ok := r.(apiTokenVerifier); !ok {
		t.Fatal("ExecRunner must implement apiTokenVerifier so Setup verifies the token before side effects")
	}
}
