package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"felis.lolicon.best/internal/updater"
	"felis.lolicon.best/internal/updates"
)

// updateTimeout bounds the whole discovery pass. Each release source already caps
// its own HTTP client, but a hung DNS or a stalled TLS handshake would otherwise
// leave the operator staring at a silent terminal.
const updateTimeout = 60 * time.Second

// updateTarget maps a user-facing selector (`--panel`) onto the planner's component
// name and the command that actually performs the update.
//
// The apply side is deliberately NOT implemented in this command. Every component
// here is installed by deploy/bootstrap.sh, which is idempotent, already handles the
// parts that are easy to get wrong (Velocity's pinned MINOR, the atomic jar install,
// the k3s image re-import that a byte-identical StatefulSet template will not
// trigger on its own), and is the path that gets exercised on every install. A
// second installer living in this file would duplicate that policy, could drift from
// it silently, and would be reachable only on a live node where a mistake takes the
// proxy or the control plane down. So `felis update` reports, and hands the operator
// the tested command — it does not re-implement it.
type updateTarget struct {
	// selector is the flag name without dashes.
	selector string
	// component is the updates planner's name for this piece, or "" when the planner
	// deliberately does not track it (Minecraft, which is pinned).
	component string
	// note explains what this selector covers, printed above the command.
	note string
	// command is the exact, already-tested way to apply it.
	command string
}

// updateTargets is the selector table. panel and plugins both resolve to felis-api
// because they are not separately versioned: the panel is compiled into the felis
// binary with //go:embed, and the plugin jars are built from this same repo in the
// same bootstrap run, so all three move together and carry one version.
var updateTargets = []updateTarget{
	{
		selector:  "panel",
		component: "felis-api",
		note:      "the panel is embedded in the felis binary (//go:embed), so updating it means rebuilding the felis image and rolling felis-api",
		command:   "sudo felis setup",
	},
	{
		selector:  "velocity",
		component: "velocity",
		note:      "re-runs install_velocity: newest BUILD of the pinned minor (FELIS_VELOCITY_VERSION), atomic jar install, then restarts felis-velocity",
		command:   "sudo felis setup",
	},
	{
		selector:  "plugins",
		component: "felis-api",
		note:      "felis-velocity.jar is a host-file swap, but felis-paper.jar and felis-limbo.jar are baked into the lobby/limbo images and need a rebuild + k3s image re-import",
		command:   "sudo felis setup",
	},
	{
		selector:  "mc",
		component: "", // never tracked: see the pin note below
		note:      "Minecraft is pinned by policy (\"能不动的就别动\") and Felis never proposes a version change for it. A server's version is a property of that server's image — change it on the server, not through a platform update",
		command:   "",
	},
}

// cmdUpdate reports what can be updated and what is already current.
//
// Bare `felis update` prints the status of every tracked component. Selector flags
// (--panel/--velocity/--mc/--plugins/--all) narrow that report to the components
// they name AND print how to apply each one. --force additionally prints the apply
// instruction for a selected component that is already up to date, for the
// reinstall/repair case.
//
// It never applies anything and never mutates the node, so unlike setup/breakGlass
// it needs no root. The versions it reads come from this host: k3s and cloudflared
// answer `--version`, Velocity's version is read out of the installed jar's
// manifest, and felis-api's is this binary's own build stamp — the same value
// `felis version` prints, which is what the user asked to be the source of truth.
func cmdUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	panel := fs.Bool("panel", false, "select the panel + control plane (felis-api)")
	velocity := fs.Bool("velocity", false, "select the Velocity proxy")
	mc := fs.Bool("mc", false, "select Minecraft (pinned; reported only)")
	plugins := fs.Bool("plugins", false, "select the Felis plugin jars (velocity/paper/limbo)")
	all := fs.Bool("all", false, "select every component above")
	force := fs.Bool("force", false, "print the apply command for a selected component even when it is already up to date")
	velocityJar := fs.String("velocity-jar", updater.DefaultVelocityJarPath, "path to the installed Velocity jar to read the current version from")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "felis update: unexpected argument %q (this command takes flags only)\n", fs.Arg(0))
		return 2
	}

	selected := map[string]bool{}
	for sel, on := range map[string]bool{"panel": *panel, "velocity": *velocity, "mc": *mc, "plugins": *plugins} {
		if on || *all {
			selected[sel] = true
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()

	rn := &updater.Runner{
		Gatherer: updater.NewHostGatherer(resolvedVersion(), *velocityJar),
		Source:   updater.NewRoutingSource(updater.Topology()),
		// Notifier and Applier stay nil on purpose: a human typing this command IS the
		// notification, and nothing here applies. The zero Window below means every
		// Scheduled component degrades to a notify, so the report can never claim an
		// apply is under way.
	}
	res, err := rn.Run(ctx, time.Now(), updates.Window{})
	if err != nil {
		fmt.Fprintf(stderr, "felis update: %v\n", err)
		return 1
	}

	fmt.Fprint(stdout, renderUpdateReport(res, selected))
	if len(selected) > 0 {
		fmt.Fprint(stdout, renderApplyGuidance(res, selected, *force))
	}
	return 0
}

// renderUpdateReport renders the component status table. With no selectors it shows
// every tracked component; with selectors it shows only the components those
// selectors name, so `felis update --velocity` is a focused answer rather than the
// whole platform. Components whose current version could not be read are listed
// separately rather than silently dropped — a component missing from the table with
// no explanation reads as "fine", which is the one thing it is not.
func renderUpdateReport(res updater.Result, selected map[string]bool) string {
	plan := res.RunResult.Plan
	if len(selected) > 0 {
		want := map[string]bool{}
		for _, t := range updateTargets {
			if selected[t.selector] && t.component != "" {
				want[t.component] = true
			}
		}
		filtered := make([]updates.Action, 0, len(plan))
		for _, a := range plan {
			if want[a.Component] {
				filtered = append(filtered, a)
			}
		}
		plan = filtered
	}

	var b strings.Builder
	if len(plan) > 0 {
		b.WriteString(updates.Report(plan))
	}

	// Two different failures both end up as a component the report cannot speak to,
	// and both are printed rather than swallowed. A component that simply vanishes
	// from the table reads as "fine", and a bare "latest unknown" line reads as "there
	// is nothing newer" — when the truth may be that the release feed was unreachable.
	// Naming the cause is the difference between a report and a guess.
	writeErrs(&b, "current version unreadable", res.GatherErrors, selected)
	writeErrs(&b, "latest version undiscoverable", res.RunResult.SourceErrors, selected)

	// With selectors active an empty table is not a surprise — `--mc` names a component
	// the planner deliberately does not track — and the guidance below already explains
	// it, so stay quiet rather than printing a bare "nothing matched" that reads as an
	// error. Without selectors an empty table means nothing is tracked at all, which
	// does need saying.
	if b.Len() == 0 && len(selected) == 0 {
		return "No components tracked.\n"
	}
	return b.String()
}

// writeErrs appends one explanatory line per failed component, in a stable order so
// the output does not shuffle between runs, honouring the active selector filter.
func writeErrs(b *strings.Builder, label string, errs map[string]error, selected map[string]bool) {
	names := make([]string, 0, len(errs))
	for name := range errs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if len(selected) > 0 && !selectedCovers(selected, name) {
			continue
		}
		fmt.Fprintf(b, "%-13s %s: %v\n", name, label, errs[name])
	}
}

// selectedCovers reports whether any chosen selector maps to this component.
func selectedCovers(selected map[string]bool, component string) bool {
	for _, t := range updateTargets {
		if selected[t.selector] && t.component == component {
			return true
		}
	}
	return false
}

// renderApplyGuidance prints, for each selected target, how to actually apply the
// update. A target that is already current is skipped unless --force was passed.
func renderApplyGuidance(res updater.Result, selected map[string]bool, force bool) string {
	byComponent := map[string]updates.Action{}
	for _, a := range res.RunResult.Plan {
		byComponent[a.Component] = a
	}

	var b strings.Builder
	var offeredCommand bool
	var offeredFelisAPI bool
	for _, t := range updateTargets {
		if !selected[t.selector] {
			continue
		}
		// Minecraft has no planner entry by design; state the pin and move on. There is
		// no command to offer, so this must not arm the trailer below.
		if t.component == "" {
			fmt.Fprintf(&b, "\n--%s: %s.\n", t.selector, t.note)
			continue
		}
		a, planned := byComponent[t.component]
		// "Up to date" requires actually KNOWING the latest version. ActionNone covers
		// both "nothing newer exists" and "the release feed could not be read", and
		// collapsing those would report an unreachable upstream as currency — telling an
		// operator they are current when nobody checked is the one answer an update tool
		// must never give. LatestKnown is what separates them.
		if planned && a.Kind == updates.ActionNone && a.LatestKnown && !force {
			fmt.Fprintf(&b, "\n--%s: %s is already up to date; nothing to apply (use --force to reinstall anyway).\n", t.selector, t.component)
			continue
		}
		fmt.Fprintf(&b, "\n--%s: %s\n", t.selector, t.note)
		if planned && !a.LatestKnown {
			// Unknown latest still offers the command: the operator asked about this
			// component, and reinstalling the current release is a valid repair action.
			fmt.Fprintf(&b, "  note: cannot tell whether %s is current — its latest version could not be discovered (see above); this reinstalls it either way\n", t.component)
		}
		fmt.Fprintf(&b, "  run: %s\n", t.command)
		offeredCommand = true
		offeredFelisAPI = offeredFelisAPI || t.component == "felis-api"
	}
	// Only explain the command when one was actually offered; a --mc-only run has
	// nothing to run and the trailer would be a non-sequitur.
	if offeredCommand {
		b.WriteString("\nfelis setup is idempotent and re-runs the installer that owns these components;\nit does not reinstall what is already current. Restart game servers afterwards.\n")
	}
	// Scoped to felis-api because it is the only component setup cannot move forward.
	// velocity is fine: install_velocity re-resolves the newest build of the pinned minor
	// on every run. But setup hands deploy/bootstrap.sh the binary it is itself running
	// (FELIS_BOOTSTRAP_BINARY), and that arm skips the release lookup entirely, so it
	// rebuilds the image and rolls the deployment from the SAME binary -- a run that looks
	// like a successful update and leaves the version unchanged.
	//
	// The installer is the only thing that moves felis-api, but it is NOT an updater and
	// must not be recommended as one without this warning. detect_node_ip re-derives
	// FELIS_ROOT_DOMAIN on every run and defaults it to <node-ip>.nip.io -- nothing reads
	// the domain back out of the felis.toml a previous run wrote. A bare re-run therefore
	// rewrites root-domain/panel-hostname/admin-hostname to nip.io names while
	// ensure_panel_tls_cert, which is write-once, keeps serving the old ones: the console
	// stops matching its own certificate. There is no re-domain flow to recover with.
	if offeredFelisAPI {
		b.WriteString("\nfelis-api (panel, plugins) is the exception: setup re-images it from the felis binary\nalready on this host, so it cannot install a NEWER felis-api. Only re-running the\nbootstrap installer does that, and it is a full install run, not an update: give it the\nSAME environment as the original install, FELIS_ROOT_DOMAIN above all. It defaults to\n<node-ip>.nip.io, and a bare re-run re-domains this install while the write-once panel\ncertificate keeps the old hostnames.\n")
	}
	return b.String()
}
