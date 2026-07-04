package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// velocityV3Fixture is the PaperMC Fill v3 GET /v3/projects/velocity response body,
// captured from the live API on 2026-07-04 (keys and version strings exactly as
// returned; JSON whitespace normalized). Grounding the fixture in the real response is
// what makes this a contract test rather than a self-referential one:
//   - the newest overall version is a -SNAPSHOT (3.5.0-SNAPSHOT) while the newest
//     stable release is 3.4.0, so the stable filter runs against real data; and
//   - the "versions" object groups the ENTIRE 3.x line under a single key "3.0.0"
//     (not per-minor keys), so a parser that trusted the group key to bound the
//     versions inside it would be wrong — proof the key-agnostic flatten is required.
// (The v2 API this replaces now returns HTTP 410.)
const velocityV3Fixture = `{
  "project": {"id": "velocity", "name": "Velocity"},
  "versions": {
    "3.0.0": [
      "3.5.0-SNAPSHOT",
      "3.4.0",
      "3.4.0-SNAPSHOT",
      "3.3.0-SNAPSHOT",
      "3.2.0-SNAPSHOT",
      "3.1.2-SNAPSHOT",
      "3.1.1",
      "3.1.1-SNAPSHOT",
      "3.1.0"
    ],
    "1.1.0": ["1.1.9"],
    "1.0.0": ["1.0.10"]
  }
}`

func newTestPaperMC(srv *httptest.Server) paperMC {
	return paperMC{
		baseURL:   srv.URL,
		userAgent: "felis-updater/0.1 (+https://felis.lolicon.best)",
		hc:        srv.Client(),
	}
}

// TestPaperMCLatestStableFiltersSnapshots is the core assertion: against the real v3
// shape, latestStable returns the newest STABLE release (3.4.0), never the newer
// 3.5.0-SNAPSHOT prerelease.
func TestPaperMCLatestStableFiltersSnapshots(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/projects/velocity" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(velocityV3Fixture))
	}))
	defer srv.Close()

	v, err := newTestPaperMC(srv).latestStable(context.Background(), "velocity")
	if err != nil {
		t.Fatalf("latestStable: %v", err)
	}
	if v.String() != "3.4.0" {
		t.Errorf("latestStable = %q, want 3.4.0 (newest stable; SNAPSHOTs filtered)", v.String())
	}
	if v.IsPrerelease() {
		t.Errorf("latestStable returned a prerelease %q", v.String())
	}
}

// TestPaperMCSendsNonGenericUserAgent proves Felis transmits a descriptive,
// contact-carrying User-Agent rather than a generic library default. PaperMC's API
// usage policy asks for this and reserves the right to block anonymous/generic agents;
// upstream enforcement was not active on the project endpoint as of 2026-07-04 (a bare
// UA got HTTP 200), so this verifies OUR compliance with the policy, not an upstream
// gate we depend on.
func TestPaperMCSendsNonGenericUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(velocityV3Fixture))
	}))
	defer srv.Close()

	if _, err := newTestPaperMC(srv).latestStable(context.Background(), "velocity"); err != nil {
		t.Fatalf("latestStable: %v", err)
	}
	if gotUA == "" {
		t.Fatal("no User-Agent sent")
	}
	for _, bad := range []string{"Go-http-client", "curl", "wget"} {
		if strings.Contains(gotUA, bad) {
			t.Errorf("User-Agent %q looks generic (contains %q)", gotUA, bad)
		}
	}
	if !strings.Contains(gotUA, "felis") || !strings.Contains(gotUA, "http") {
		t.Errorf("User-Agent %q should name the software and carry a contact URL", gotUA)
	}
}

// TestPaperMCFailsClosedOnHTTPError proves a non-200 (like the real 410 Gone the dead
// v2 endpoint now returns) yields an error, never a bogus zero version.
func TestPaperMCFailsClosedOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Gone", http.StatusGone)
	}))
	defer srv.Close()

	if v, err := newTestPaperMC(srv).latestStable(context.Background(), "velocity"); err == nil {
		t.Fatalf("want error on HTTP 410, got version %q", v.String())
	}
}

// TestPaperMCFailsClosedWhenOnlySnapshots proves a feed with no stable release is an
// error, not a silent latest — so a SNAPSHOT-only line never drives an update.
func TestPaperMCFailsClosedWhenOnlySnapshots(t *testing.T) {
	const onlySnapshots = `{"versions":{"9.9":["9.9.0-SNAPSHOT","9.8.0-SNAPSHOT"]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(onlySnapshots))
	}))
	defer srv.Close()

	if _, err := newTestPaperMC(srv).latestStable(context.Background(), "velocity"); err == nil {
		t.Fatal("want error when only prereleases are published, got nil")
	}
}

// TestPaperMCSkipsUnparseableTags proves one garbled entry does not blind discovery:
// the max stable among the parseable versions is still returned.
func TestPaperMCSkipsUnparseableTags(t *testing.T) {
	const withJunk = `{"versions":{"g":["not-a-version","3.4.0","","3.4.0-SNAPSHOT"]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(withJunk))
	}))
	defer srv.Close()

	v, err := newTestPaperMC(srv).latestStable(context.Background(), "velocity")
	if err != nil {
		t.Fatalf("latestStable: %v", err)
	}
	if v.String() != "3.4.0" {
		t.Errorf("latestStable = %q, want 3.4.0 (junk/empty/snapshot skipped)", v.String())
	}
}
