package main

import (
	"errors"
	"strings"
	"testing"

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

func TestApplyGuidanceUpToDateNeedsForce(t *testing.T) {
	res := planResult([]updates.Action{{Component: "velocity", Kind: updates.ActionNone, LatestKnown: true}})
	sel := map[string]bool{"velocity": true}

	quiet := renderApplyGuidance(res, sel, false)
	if !strings.Contains(quiet, "already up to date") {
		t.Fatalf("want an up-to-date notice:\n%s", quiet)
	}
	if strings.Contains(quiet, "run:") {
		t.Fatalf("must not offer a command for an up-to-date component without --force:\n%s", quiet)
	}

	forced := renderApplyGuidance(res, sel, true)
	if !strings.Contains(forced, "run:") {
		t.Fatalf("--force must offer the reinstall command:\n%s", forced)
	}
}

// An undiscoverable latest version must never be reported as "up to date". Both
// states arrive as ActionNone and only LatestKnown separates them, so this is a live
// confusion, not a hypothetical one — it shipped that way until a smoke test showed
// `--panel` calling felis-api current right after the release feed returned 404.
func TestApplyGuidanceUnknownLatestIsNotUpToDate(t *testing.T) {
	res := planResult([]updates.Action{{Component: "velocity", Kind: updates.ActionNone, LatestKnown: false}})

	out := renderApplyGuidance(res, map[string]bool{"velocity": true}, false)
	if strings.Contains(out, "already up to date") {
		t.Fatalf("must not claim currency when the latest version is unknown:\n%s", out)
	}
	if !strings.Contains(out, "cannot tell") {
		t.Fatalf("want the uncertainty stated plainly:\n%s", out)
	}
	// One header per selector: the uncertainty is a note under it, not a second block.
	if n := strings.Count(out, "--velocity:"); n != 1 {
		t.Fatalf("want exactly 1 selector header, got %d:\n%s", n, out)
	}
	// The operator asked about this component, so the repair command still belongs.
	if !strings.Contains(out, "run:") {
		t.Fatalf("want the reinstall command offered despite the unknown latest:\n%s", out)
	}
}

// Minecraft is pinned and has no planner entry, so --mc explains the pin and offers
// NO command — and therefore must not print the trailer that explains the command.
func TestApplyGuidanceMinecraftOffersNoCommand(t *testing.T) {
	out := renderApplyGuidance(planResult(nil), map[string]bool{"mc": true}, true)
	if !strings.Contains(out, "pinned by policy") {
		t.Fatalf("want the pin explained:\n%s", out)
	}
	if strings.Contains(out, "run:") || strings.Contains(out, "Re-running the installer") {
		t.Fatalf("--mc must offer no command and no command trailer:\n%s", out)
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
		if target.command == "" {
			t.Fatalf("selector --%s is planner-backed but offers no apply command", target.selector)
		}
	}
}

// Re-running the installer is the one apply path this table may hand out. setup is NOT an
// updater on a completed install -- its host-bootstrap phase only runs while an install
// marker is missing, so it opens the config console and moves no component -- and even on
// the bootstrap path it re-images felis-api from the binary setup is already running. The
// table used to answer with "sudo felis setup" and scope a felis-api-only exception; both
// taught a model that does not survive contact with an installed host.
func TestApplyGuidancePointsEveryComponentAtTheInstaller(t *testing.T) {
	api := renderApplyGuidance(
		planResult([]updates.Action{{Component: "felis-api", Kind: updates.ActionNotify, LatestKnown: true}}),
		map[string]bool{"panel": true}, false)
	for _, want := range []string{"deploy/bootstrap.sh", "FELIS_VERSION_BOOTSTRAP=dev", "felis setup is not this path"} {
		if !strings.Contains(api, want) {
			t.Fatalf("--panel guidance missing %q:\n%s", want, api)
		}
	}
	if strings.Contains(api, "run: sudo felis setup") {
		t.Fatalf("setup must never be offered as the apply command:\n%s", api)
	}

	// The same path serves velocity; a scoped caveat would re-teach the old model that
	// setup fixes velocity.
	vel := renderApplyGuidance(
		planResult([]updates.Action{{Component: "velocity", Kind: updates.ActionNotify, LatestKnown: true}}),
		map[string]bool{"velocity": true}, false)
	if !strings.Contains(vel, "run: curl -fsSL") || !strings.Contains(vel, "felis setup is not this path") {
		t.Fatalf("velocity gets the same installer path:\n%s", vel)
	}

	// --mc offers no command at all, so neither trailer belongs.
	mc := renderApplyGuidance(planResult(nil), map[string]bool{"mc": true}, true)
	if strings.Contains(mc, "deploy/bootstrap.sh") || strings.Contains(mc, "FELIS_VERSION_BOOTSTRAP") {
		t.Fatalf("--mc offers no command; the trailer is a non-sequitur:\n%s", mc)
	}
}

// The re-run reads bootstrap.sh at the tag whose binary it installs. main can carry
// installer changes no release was tested with.
func TestApplyGuidanceReadsTheInstallerAtTheReleaseTag(t *testing.T) {
	v := func(s string) updates.Version {
		t.Helper()
		out, err := updates.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	cases := []struct {
		name string
		api  []updates.Action
		want string
	}{
		{"latest known", []updates.Action{{Component: "felis-api", Kind: updates.ActionNotify, Current: v("v1.3.0"), Latest: v("v1.4.0"), LatestKnown: true}}, "/FelisMC/Felis/v1.4.0/deploy/bootstrap.sh"},
		{"latest unknown", []updates.Action{{Component: "felis-api", Kind: updates.ActionNone, Current: v("v1.3.0")}}, "/FelisMC/Felis/v1.3.0/deploy/bootstrap.sh"},
		{"prerelease latest", []updates.Action{{Component: "felis-api", Kind: updates.ActionNone, Current: v("v1.3.0"), Latest: v("v1.4.0-rc.1"), LatestKnown: true}}, "/FelisMC/Felis/v1.3.0/deploy/bootstrap.sh"},
		{"nothing known", nil, "/FelisMC/Felis/main/deploy/bootstrap.sh"},
	}
	for _, c := range cases {
		out := renderApplyGuidance(planResult(c.api), map[string]bool{"velocity": true, "panel": true}, true)
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: want %q in:\n%s", c.name, c.want, out)
		}
		if strings.Contains(out, "{ref}") {
			t.Errorf("%s: placeholder left in:\n%s", c.name, out)
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

// k3s and cloudflared move only when the re-run is told to; PostgreSQL is the package
// manager's, so its guidance carries no installer trailer.
func TestApplyGuidanceForHostDependencies(t *testing.T) {
	notify := func(c string) updater.Result {
		return planResult([]updates.Action{{Component: c, Kind: updates.ActionNotify, LatestKnown: true}})
	}
	for _, sel := range []string{"k3s", "cloudflared"} {
		out := renderApplyGuidance(notify(sel), map[string]bool{sel: true}, false)
		if !strings.Contains(out, "sudo FELIS_UPGRADE_DEPS=1 bash") || !strings.Contains(out, "Re-running the installer") {
			t.Errorf("--%s guidance must re-run the installer with FELIS_UPGRADE_DEPS=1:\n%s", sel, out)
		}
	}
	jre := renderApplyGuidance(notify("jre"), map[string]bool{"jre": true}, false)
	if !strings.Contains(jre, "| sudo bash") || strings.Contains(jre, "FELIS_UPGRADE_DEPS") {
		t.Errorf("--jre guidance is the plain installer re-run:\n%s", jre)
	}
	pg := renderApplyGuidance(notify("postgresql"), map[string]bool{"postgres": true}, false)
	if !strings.Contains(pg, "apt-get install --only-upgrade") || strings.Contains(pg, "Re-running the installer") {
		t.Errorf("--postgres guidance is the package manager, without the installer trailer:\n%s", pg)
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

// A source build's v0.0.0+g<commit> names no tag, so the installer one-liner has to
// fall back to main instead of a 404ing ref.
func TestInstallerRefNamesATag(t *testing.T) {
	v := func(s string) updates.Version {
		t.Helper()
		x, err := updates.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	cases := []struct {
		name string
		api  updates.Action
		want string
	}{
		{"newest release", updates.Action{Current: v("v1.2.0"), Latest: v("v1.3.0"), LatestKnown: true}, "v1.3.0"},
		{"feed down, host on a release", updates.Action{Current: v("v1.2.0")}, "v1.2.0"},
		{"source build", updates.Action{Current: v("v0.0.0+gunknown")}, "main"},
		{"source build with commit", updates.Action{Current: v("v0.0.0+g1a2b3c4")}, "main"},
		{"prerelease", updates.Action{Current: v("v1.3.0-rc.1")}, "main"},
	}
	for _, c := range cases {
		if got := installerRef(map[string]updates.Action{"felis-api": c.api}); got != c.want {
			t.Errorf("%s: installerRef = %q, want %q", c.name, got, c.want)
		}
	}
	if got := installerRef(nil); got != "main" {
		t.Errorf("no felis-api row: installerRef = %q, want main", got)
	}
}
