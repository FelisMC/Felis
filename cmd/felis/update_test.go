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
	if strings.Contains(out, "run:") || strings.Contains(out, "felis setup is idempotent") {
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
