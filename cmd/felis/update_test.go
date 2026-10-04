package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/updater"
	"felis.lolicon.best/internal/updates"
)

// planResult builds a Result carrying the given plan, as updater.Runner would.
func planResult(plan []updates.Action) updater.Result {
	return updater.Result{
		RunResult:    updates.RunResult{Plan: plan, SourceErrors: map[string]error{}},
		GatherErrors: map[string]error{},
	}
}

func TestUpdateReportFiltersToSelection(t *testing.T) {
	res := planResult([]updates.Action{
		{Component: "felis-api", Kind: updates.ActionNotify},
		{Component: "velocity", Kind: updates.ActionNone, LatestKnown: true},
		{Component: "k3s", Kind: updates.ActionNotify},
	})

	all := renderUpdateReport(res, nil)
	for _, want := range []string{"felis-api", "velocity", "k3s"} {
		if !strings.Contains(all, want) {
			t.Fatalf("bare report missing %q:\n%s", want, all)
		}
	}

	// --velocity must answer about velocity only; leaking k3s into a focused query is
	// the whole reason the selector exists.
	only := renderUpdateReport(res, map[string]bool{"velocity": true})
	if !strings.Contains(only, "velocity") {
		t.Fatalf("selected report missing velocity:\n%s", only)
	}
	if strings.Contains(only, "k3s") || strings.Contains(only, "felis-api") {
		t.Fatalf("selected report leaked unselected components:\n%s", only)
	}
}

// A component that cannot be read must never vanish silently, and "latest unknown"
// must never stand in for "the release feed was unreachable" — both causes are named.
func TestUpdateReportNamesBothFailureCauses(t *testing.T) {
	res := planResult(nil)
	res.GatherErrors["velocity"] = errors.New("jar missing")
	res.RunResult.SourceErrors["felis-api"] = errors.New("HTTP 404")

	out := renderUpdateReport(res, nil)
	if !strings.Contains(out, "current version unreadable") || !strings.Contains(out, "jar missing") {
		t.Fatalf("gather failure not explained:\n%s", out)
	}
	if !strings.Contains(out, "latest version undiscoverable") || !strings.Contains(out, "HTTP 404") {
		t.Fatalf("source failure not explained:\n%s", out)
	}
}

// Every selector in the table must be a real flag on the FlagSet, and every
// planner-backed selector must name a component the topology actually tracks —
// otherwise a selector silently matches nothing at runtime.
func TestUpdateTargetsMatchTopology(t *testing.T) {
	tracked := map[string]bool{}
	for _, s := range updater.Topology() {
		tracked[s.Name] = true
	}
	for _, target := range updateTargets {
		if target.component == "" {
			continue // deliberately untracked (Minecraft is pinned)
		}
		if !tracked[target.component] {
			t.Fatalf("selector --%s maps to %q, which Topology() does not track", target.selector, target.component)
		}
	}
}

// Every selector in the table is a flag: the FlagSet is built from the table.
func TestUpdateSelectorsAreFlags(t *testing.T) {
	var out, errb strings.Builder
	if code := cmdUpdate([]string{"-h"}, &out, &errb); code != 2 {
		t.Fatalf("-h exit = %d, want 2", code)
	}
	for _, target := range updateTargets {
		if !strings.Contains(errb.String(), "-"+target.selector+"\n") {
			t.Errorf("usage has no -%s flag:\n%s", target.selector, errb.String())
		}
	}
}

func TestRenderNotesHonoursSelectors(t *testing.T) {
	notes := map[string]string{"postgresql": "PostgreSQL 13 reached end of life on 2025-11-13"}
	if out := renderNotes(notes, nil); !strings.Contains(out, "postgresql") || !strings.Contains(out, "note: PostgreSQL 13 reached end of life") {
		t.Errorf("unfiltered notes = %q", out)
	}
	if out := renderNotes(notes, map[string]bool{"postgres": true}); !strings.Contains(out, "end of life") {
		t.Errorf("--postgres must show its note, got %q", out)
	}
	if out := renderNotes(notes, map[string]bool{"velocity": true}); out != "" {
		t.Errorf("--velocity must not show the postgresql note, got %q", out)
	}
}

func mustVersion(t *testing.T, s string) updates.Version {
	t.Helper()
	v, err := updates.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// The record the panel shows keeps every component with a state that cannot be
// mistaken: a feed failure is "unknown" with its reason, an unreadable install is
// listed after the plan, and each row carries the selector that prints its apply.
func TestBuildStatusReport(t *testing.T) {
	res := planResult([]updates.Action{
		{Component: "felis-api", Current: mustVersion(t, "v0.4.0"), Latest: mustVersion(t, "v0.5.0"), LatestKnown: true, Kind: updates.ActionNotify},
		{Component: "velocity", Current: mustVersion(t, "3.4.0"), Latest: mustVersion(t, "3.4.0"), LatestKnown: true, Kind: updates.ActionNone},
		{Component: "k3s", Current: mustVersion(t, "v1.36.2+k3s1"), Kind: updates.ActionNone},
		{Component: "cloudflared", Current: mustVersion(t, "2026.6.1"), Latest: mustVersion(t, "2026.9.0"), LatestKnown: true, Kind: updates.ActionApply},
		{Component: "mc-lobby", Current: mustVersion(t, "1.21.4"), Kind: updates.ActionPinned},
	})
	res.RunResult.SourceErrors["k3s"] = errors.New("github: HTTP 403")
	res.GatherErrors["postgresql"] = errors.New("psql: not found")
	res.GatherErrors["jre"] = errors.New("release file missing")
	notes := map[string]string{"postgresql": "PostgreSQL 13 is past its end of life", "velocity": "pinned minor 3.4"}
	now := time.Date(2026, 9, 25, 3, 4, 5, 0, time.FixedZone("CST", 8*3600))

	b, err := json.Marshal(buildStatusReport(res, notes, "v0.4.0", now))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"checked_at":"2026-09-24T19:04:05Z","felis":"v0.4.0","components":[` +
		`{"name":"felis-api","current":"v0.4.0","latest":"v0.5.0","state":"available","selector":"panel"},` +
		`{"name":"velocity","current":"3.4.0","state":"current","selector":"velocity","note":"pinned minor 3.4"},` +
		`{"name":"k3s","current":"v1.36.2+k3s1","state":"unknown","selector":"k3s","error":"github: HTTP 403"},` +
		`{"name":"cloudflared","current":"2026.6.1","latest":"2026.9.0","state":"available","selector":"cloudflared"},` +
		`{"name":"mc-lobby","current":"1.21.4","state":"pinned"},` +
		`{"name":"jre","state":"unreadable","selector":"jre","error":"release file missing"},` +
		`{"name":"postgresql","state":"unreadable","selector":"postgres","note":"PostgreSQL 13 is past its end of life","error":"psql: not found"}]}`
	if string(b) != want {
		t.Errorf("status report =\n%s\nwant\n%s", b, want)
	}

	// Nothing tracked still records an empty list, so the panel can tell "checked,
	// nothing to show" from a report that never arrived.
	b, _ = json.Marshal(buildStatusReport(planResult(nil), nil, "v0.4.0", now))
	if !strings.Contains(string(b), `"components":[]`) {
		t.Errorf("an empty check = %s, want an empty components list", b)
	}
}
