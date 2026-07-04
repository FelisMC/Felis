package updater

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/updates"
)

func mustV(t *testing.T, s string) updates.Version {
	t.Helper()
	v, err := updates.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// fakeGatherer returns canned current versions, or an error for names in failFor.
type fakeGatherer struct {
	cur     map[string]updates.Version
	failFor map[string]bool
}

func (f fakeGatherer) Current(_ context.Context, s Spec) (updates.Version, error) {
	if f.failFor[s.Name] {
		return updates.Version{}, errors.New("gather boom")
	}
	v, ok := f.cur[s.Name]
	if !ok {
		return updates.Version{}, errors.New("no current for " + s.Name)
	}
	return v, nil
}

// fakeSource returns canned latest versions, isolating the runner's wiring from any
// real release source.
type fakeSource struct{ latest map[string]updates.Version }

func (f fakeSource) Latest(_ context.Context, c updates.Component) (updates.Version, error) {
	v, ok := f.latest[c.Name]
	if !ok {
		return updates.Version{}, errors.New("not found")
	}
	return v, nil
}

// TestRunnerReportOnlyComposesPlan drives the whole slice with fakes and nil
// notifier/applier (report-only): gather → assemble → updates.Run → Report. With the
// window open, the two Scheduled+manageable components (felis-api, cloudflared) plan
// to Apply, but with no applier wired the runner applies nothing and records the
// no-applier error — the honest report-only state, not a silent success.
func TestRunnerReportOnlyComposesPlan(t *testing.T) {
	now := time.Date(2026, 7, 4, 3, 30, 0, 0, time.UTC)
	window := updates.Window{
		Start: time.Date(2026, 7, 4, 3, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 4, 4, 0, 0, 0, time.UTC),
	}
	gath := fakeGatherer{cur: map[string]updates.Version{
		"felis-api":   mustV(t, "1.4.0"),
		"k3s":         mustV(t, "v1.30.2+k3s1"),
		"cloudflared": mustV(t, "2024.2.1"),
		"velocity":    mustV(t, "3.1.1"),
	}}
	src := fakeSource{latest: map[string]updates.Version{
		"felis-api":   mustV(t, "1.5.0"),        // newer stable → apply (scheduled+manageable, window open)
		"k3s":         mustV(t, "v1.30.3+k3s1"), // newer → notify (high blast radius, unmanageable)
		"cloudflared": mustV(t, "2024.3.0"),     // newer stable → apply
		"velocity":    mustV(t, "3.4.0"),        // newer stable → notify (off-cluster, unmanageable)
	}}

	rn := &Runner{Gatherer: gath, Source: src} // nil Notifier + nil Applier = report-only
	res, err := rn.Run(context.Background(), now, window)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, want := range []string{
		"felis-api", "1.5.0", "apply (scheduled window)",
		"cloudflared", "2024.3.0",
		"k3s", "update available (notify)",
		"velocity", "3.4.0",
	} {
		if !strings.Contains(res.Report, want) {
			t.Errorf("report missing %q; got:\n%s", want, res.Report)
		}
	}

	if len(res.RunResult.Applied) != 0 {
		t.Errorf("report-only run applied %+v, want nothing", res.RunResult.Applied)
	}
	for _, name := range []string{"felis-api", "cloudflared"} {
		if res.RunResult.ApplyErrors[name] == nil {
			t.Errorf("report-only run should record a no-applier error for %s (apply wanted, none wired)", name)
		}
	}
}

// TestRunnerSkipsUngatherableComponent proves a component whose current version cannot
// be read is dropped from the plan and recorded, not planned against a zero version.
func TestRunnerSkipsUngatherableComponent(t *testing.T) {
	now := time.Date(2026, 7, 4, 3, 30, 0, 0, time.UTC)
	gath := fakeGatherer{
		cur:     map[string]updates.Version{"velocity": mustV(t, "3.1.1")},
		failFor: map[string]bool{"felis-api": true, "k3s": true, "cloudflared": true},
	}
	src := fakeSource{latest: map[string]updates.Version{"velocity": mustV(t, "3.4.0")}}

	rn := &Runner{Gatherer: gath, Source: src}
	res, err := rn.Run(context.Background(), now, updates.Window{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.GatherErrors["felis-api"] == nil {
		t.Error("felis-api gather failure should be recorded")
	}
	if len(res.RunResult.Plan) != 1 || res.RunResult.Plan[0].Component != "velocity" {
		t.Errorf("plan = %+v, want just velocity (the only gatherable component)", res.RunResult.Plan)
	}
}

// TestRunnerWithRoutingSource wires the real RoutingSource — PaperMC and GitHub both
// pointed at live-shape httptest fixtures — through the runner end to end. Every
// component now discovers its real latest stable (velocity 3.4.0 via PaperMC; felis-api,
// k3s and cloudflared via GitHub Releases), the raw upstream tags survive into the report
// (k3s's "+k3s1"), and NO component falls back to a recorded source error.
func TestRunnerWithRoutingSource(t *testing.T) {
	now := time.Date(2026, 7, 5, 3, 30, 0, 0, time.UTC)

	paperSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v3/projects/") {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(velocityV3Fixture))
	}))
	defer paperSrv.Close()

	// GitHub fixtures keyed by repo (felis-api's coord is a placeholder slug). The
	// handler also mirrors GitHub's real gate: a UA-less request is refused.
	ghBodies := map[string]string{
		"/repos/felis/felis/releases/latest":           `{"tag_name":"1.5.0","prerelease":false,"draft":false}`,
		"/repos/k3s-io/k3s/releases/latest":            k3sLatestFixture,
		"/repos/cloudflare/cloudflared/releases/latest": cloudflaredLatestFixture,
	}
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "Request forbidden by administrative rules", http.StatusForbidden)
			return
		}
		body, ok := ghBodies[r.URL.Path]
		if !ok {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer ghSrv.Close()

	rs := NewRoutingSource(Topology())
	rs.paper = newTestPaperMC(paperSrv) // point PaperMC discovery at its httptest server
	rs.gh = newTestGitHub(ghSrv)        // and GitHub discovery at its own

	gath := fakeGatherer{cur: map[string]updates.Version{
		"felis-api":   mustV(t, "1.4.0"),
		"k3s":         mustV(t, "v1.35.6+k3s1"),
		"cloudflared": mustV(t, "2026.5.0"),
		"velocity":    mustV(t, "3.1.1"),
	}}
	rn := &Runner{Gatherer: gath, Source: rs}
	res, err := rn.Run(context.Background(), now, updates.Window{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Every component's real discovered latest is in the report (each current is older).
	for _, want := range []string{
		"velocity", "3.4.0",
		"felis-api", "1.5.0",
		"k3s", "v1.36.2+k3s1",
		"cloudflared", "2026.6.1",
	} {
		if !strings.Contains(res.Report, want) {
			t.Errorf("report missing discovered %q; got:\n%s", want, res.Report)
		}
	}
	// Both routes are wired now, so nothing degrades to a source error.
	if len(res.RunResult.SourceErrors) != 0 {
		t.Errorf("expected no source errors with both routes wired, got %v", res.RunResult.SourceErrors)
	}
}
