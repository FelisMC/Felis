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

// TestRunnerWithRoutingSource wires the real RoutingSource — PaperMC via the live-shape
// httptest fixture, GitHub honestly un-wired — through the runner end to end. velocity
// discovers its real latest stable (3.4.0 → notify); the GitHub-backed components
// degrade to "latest unknown" via the recorded errGitHubNotWired, never a fabricated
// version.
func TestRunnerWithRoutingSource(t *testing.T) {
	now := time.Date(2026, 7, 4, 3, 30, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v3/projects/") {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(velocityV3Fixture))
	}))
	defer srv.Close()

	rs := NewRoutingSource(Topology())
	rs.paper = newTestPaperMC(srv) // point PaperMC discovery at the httptest server

	gath := fakeGatherer{cur: map[string]updates.Version{
		"felis-api":   mustV(t, "1.4.0"),
		"k3s":         mustV(t, "v1.30.2+k3s1"),
		"cloudflared": mustV(t, "2024.2.1"),
		"velocity":    mustV(t, "3.1.1"),
	}}
	rn := &Runner{Gatherer: gath, Source: rs}
	res, err := rn.Run(context.Background(), now, updates.Window{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(res.Report, "velocity") || !strings.Contains(res.Report, "3.4.0") {
		t.Errorf("report should show velocity's discovered latest 3.4.0; got:\n%s", res.Report)
	}
	for _, name := range []string{"felis-api", "k3s", "cloudflared"} {
		if !errors.Is(res.RunResult.SourceErrors[name], errGitHubNotWired) {
			t.Errorf("SourceErrors[%s] = %v, want errGitHubNotWired", name, res.RunResult.SourceErrors[name])
		}
	}
}
