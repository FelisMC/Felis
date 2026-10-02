package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"felis.lolicon.best/internal/config"
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
// the image re-import + registry push that a byte-identical StatefulSet template
// will not trigger on its own), and is the path that gets exercised on every
// install. A
// second installer living in this file would duplicate that policy, could drift from
// it silently, and would be reachable only on a live node where a mistake takes the
// proxy or the control plane down. So `felis update` reports, and hands the operator
// the tested command — it does not re-implement it.
type updateTarget struct {
	// selector is the flag name without dashes.
	selector string
	// help is the flag's usage line.
	help string
	// component is the updates planner's name for this piece, or "" when the planner
	// deliberately does not track it (Minecraft, which is pinned).
	component string
	// note explains what this selector covers, printed above the command.
	note string
	// command is the exact, already-tested way to apply it.
	command string
	// installer marks a command that re-runs the installer, which the trailer explains.
	installer bool
}

// installerRerun is the tested apply path for every selector Felis installs: re-run the
// installer. It is idempotent, and it is the only path that fetches a newer version --
// `felis setup` skips its host-bootstrap phase on a completed install (all four install
// markers already exist), so there it opens the config console and moves no component,
// and even on the bootstrap path it re-images felis-api from the binary setup is already
// running (FELIS_BOOTSTRAP_BINARY), which looks like an update and changes nothing.
//
// The URL is the one-liner both READMEs hand out, read at a tag rather than main: the
// script's release channel installs the newest release's binary, and main can carry
// installer changes that binary was never tested with. installerRef picks the tag and
// renderApplyGuidance substitutes it for {ref}.
const installerRerun = "curl -fsSL https://raw.githubusercontent.com/FelisMC/Felis/{ref}/deploy/bootstrap.sh | sudo bash"

// installerRerunDeps is the same re-run with FELIS_UPGRADE_DEPS=1, which lets it move an
// installed k3s and cloudflared to the versions the release pins.
const installerRerunDeps = "curl -fsSL https://raw.githubusercontent.com/FelisMC/Felis/{ref}/deploy/bootstrap.sh | sudo FELIS_UPGRADE_DEPS=1 bash"

// updateTargets is the selector table. panel and plugins both resolve to felis-api
// because they are not separately versioned: the panel is compiled into the felis
// binary with //go:embed, and the plugin jars are built from this same repo in the
// same bootstrap run, so all three move together and carry one version.
var updateTargets = []updateTarget{
	{
		selector:  "panel",
		help:      "select the panel + control plane (felis-api)",
		component: "felis-api",
		note:      "the panel is embedded in the felis binary (//go:embed), so updating it means rebuilding the felis image and rolling felis-api",
		command:   installerRerun,
		installer: true,
	},
	{
		selector:  "velocity",
		help:      "select the Velocity proxy",
		component: "velocity",
		note:      "re-runs install_velocity: the build the release pins in deploy/game-stack.lock (FELIS_VELOCITY_VERSION=<minor> takes that minor's newest build instead), sha256-checked, atomic jar install, then restarts felis-velocity only if the jar or its config changed",
		command:   installerRerun,
		installer: true,
	},
	{
		selector:  "plugins",
		help:      "select the Felis plugin jars (velocity/paper/limbo)",
		component: "felis-api",
		note:      "felis-velocity.jar is a host-file swap, but felis-paper.jar and felis-limbo.jar are baked into the lobby/limbo images and need a rebuild + re-mirror into the in-cluster registry (the installer re-run does both)",
		command:   installerRerun,
		installer: true,
	},
	{
		selector:  "k3s",
		help:      "select k3s",
		component: "k3s",
		note:      "FELIS_UPGRADE_DEPS=1 moves k3s to the version the Felis release pins, which can trail the newest upstream; it moves one minor version at a time and refuses a larger jump. Running game servers keep running while k3s restarts",
		command:   installerRerunDeps,
		installer: true,
	},
	{
		selector:  "cloudflared",
		help:      "select cloudflared",
		component: "cloudflared",
		note:      "FELIS_UPGRADE_DEPS=1 swaps the binary for the sha256-pinned build the Felis release names and restarts cloudflared-felis; the panel's tunnel drops for a few seconds",
		command:   installerRerunDeps,
		installer: true,
	},
	{
		selector:  "jre",
		help:      "select the Temurin JRE Velocity runs on",
		component: "jre",
		note:      "the installer installs the Temurin build the Felis release pins (sha256-checked) and restarts felis-velocity when it changed; a newer upstream build reaches the host with a release that pins it",
		command:   installerRerun,
		installer: true,
	},
	{
		selector:  "postgres",
		help:      "select PostgreSQL",
		component: "postgresql",
		note:      "PostgreSQL runs as the felis-postgres Deployment from the image the Felis release pins by digest; a newer minor reaches the host with a release that moves the pin, and the installer re-run restarts the database on it (a few seconds without the API). A new major is a dump and restore: docs/operations.md §4",
		command:   installerRerun,
		installer: true,
	},
	{
		selector:  "mc",
		help:      "select Minecraft (pinned; reported only)",
		component: "", // never tracked: see the pin note below
		note:      "Minecraft is pinned by policy (\"能不动的就别动\") and Felis never proposes a version change for it. A server's version is a property of that server's image — change it on the server, not through a platform update",
		command:   "",
	},
}

// cmdUpdate reports what can be updated and what is already current.
//
// Bare `felis update` prints the status of every tracked component. Selector flags
// (--panel/--velocity/--plugins/--k3s/--cloudflared/--jre/--postgres/--mc/--all) narrow that report to the components
// they name AND print how to apply each one. --force additionally prints the apply
// instruction for a selected component that is already up to date, for the
// reinstall/repair case.
//
// It never applies anything and never mutates the node, so unlike setup/breakGlass
// it needs no root, apart from PostgreSQL's version, which the felis-postgres container
// answers through the cluster's admin kubeconfig. The versions it reads come from this
// host: k3s, cloudflared and PostgreSQL answer `--version`, Velocity's version is read out of the installed jar's
// manifest, the JRE's out of its release file, and felis-api's is this binary's own
// build stamp — the same value `felis version` prints, which is what the user asked
// to be the source of truth.
func cmdUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	flags := map[string]*bool{}
	for _, t := range updateTargets {
		flags[t.selector] = fs.Bool(t.selector, false, t.help)
	}
	all := fs.Bool("all", false, "select every component above")
	force := fs.Bool("force", false, "print the apply command for a selected component even when it is already up to date")
	velocityJar := fs.String("velocity-jar", updater.DefaultVelocityJarPath, "path to the installed Velocity jar to read the current version from")
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml, read for the maintenance window the panel stores")
	record := fs.Bool("record", false, "also store this check for the panel's Updates page (felis-update-check.timer runs it daily)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "felis update: unexpected argument %q (this command takes flags only)\n", fs.Arg(0))
		return 2
	}

	selected := map[string]bool{}
	for sel, on := range flags {
		if *on || *all {
			selected[sel] = true
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()

	src := updater.NewRoutingSource(updater.Topology())
	rn := &updater.Runner{
		Gatherer: updater.NewHostGatherer(resolvedVersion(), *velocityJar),
		Source:   src,
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

	now := time.Now()
	win, winErr := readUpdateWindow(ctx, *cfgPath)
	fmt.Fprint(stdout, renderWindowLine(win, winErr, now))
	fmt.Fprint(stdout, renderUpdateReport(res, selected))
	fmt.Fprint(stdout, renderNotes(src.Notes(), selected))
	if len(selected) > 0 {
		if winErr == nil && !win.Start.IsZero() && !win.Contains(now) {
			fmt.Fprint(stdout, "Warning: this is outside the maintenance window; the commands below take effect as soon as you run them.\n")
		}
		fmt.Fprint(stdout, renderApplyGuidance(res, selected, *force))
	}
	if *record {
		// A fresh context: the discovery pass may have spent most of updateTimeout.
		rctx, rcancel := context.WithTimeout(context.Background(), updateWindowTimeout)
		defer rcancel()
		if err := recordUpdateStatus(rctx, *cfgPath, buildStatusReport(res, src.Notes(), resolvedVersion(), now)); err != nil {
			fmt.Fprintf(stderr, "felis update: record the check for the panel: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, "Recorded this check for the panel's Updates page.\n")
	}
	return 0
}

// buildStatusReport turns one run into the record the panel shows: every planned
// component in plan order, then each component whose installed version could not
// be read, by name. A component the feed could not answer for is StateUnknown with
// the reason, never StateCurrent: the panel must not call a component current when
// nobody could check.
func buildStatusReport(res updater.Result, notes map[string]string, felis string, now time.Time) updates.StatusReport {
	selectorOf := map[string]string{}
	for _, t := range updateTargets {
		if t.component != "" && selectorOf[t.component] == "" {
			selectorOf[t.component] = t.selector
		}
	}
	rep := updates.StatusReport{CheckedAt: now.UTC(), Felis: felis, Components: []updates.ComponentStatus{}}
	for _, a := range res.RunResult.Plan {
		cs := updates.ComponentStatus{
			Name:     a.Component,
			Current:  a.Current.String(),
			Selector: selectorOf[a.Component],
			Note:     notes[a.Component],
		}
		switch {
		case a.Kind == updates.ActionPinned:
			cs.State = updates.StatePinned
		case a.Kind == updates.ActionNotify || a.Kind == updates.ActionApply:
			cs.State = updates.StateAvailable
			cs.Latest = a.Latest.String()
		case a.LatestKnown:
			cs.State = updates.StateCurrent
		default:
			cs.State = updates.StateUnknown
			if err := res.RunResult.SourceErrors[a.Component]; err != nil {
				cs.Error = err.Error()
			}
		}
		rep.Components = append(rep.Components, cs)
	}
	names := make([]string, 0, len(res.GatherErrors))
	for name := range res.GatherErrors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rep.Components = append(rep.Components, updates.ComponentStatus{
			Name:     name,
			State:    updates.StateUnreadable,
			Selector: selectorOf[name],
			Note:     notes[name],
			Error:    res.GatherErrors[name].Error(),
		})
	}
	return rep
}

// recordUpdateStatus upserts rep into platform_settings[updates.StatusKey], the
// row the API serves to the panel's Updates page.
func recordUpdateStatus(ctx context.Context, cfgPath string, rep updates.StatusReport) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	v, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx, `INSERT INTO platform_settings (key, value) VALUES ($1, $2::jsonb)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, updates.StatusKey, string(v))
	return err
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

// renderNotes prints what the release lookups learned beyond the versions (today: a
// PostgreSQL major past its end of life), for the components the selectors show.
func renderNotes(notes map[string]string, selected map[string]bool) string {
	names := make([]string, 0, len(notes))
	for name := range notes {
		if len(selected) == 0 || selectedCovers(selected, name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%-13s note: %s\n", name, notes[name])
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
	var offeredInstaller bool
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
		fmt.Fprintf(&b, "  run: %s\n", strings.ReplaceAll(t.command, "{ref}", installerRef(byComponent)))
		offeredInstaller = offeredInstaller || t.installer
	}
	// Only explain the command when one was actually offered; a --mc-only run has
	// nothing to run and the trailer would be a non-sequitur.
	//
	// One trailer serves every selector now: setup is not an apply path at all on a
	// completed install (shouldRunHostBootstrapBeforeConfig only enters the host
	// bootstrap while an install marker is missing), so the installer re-run is the one
	// worked path for every component Felis installs and there is no per-component exception left
	// to scope. One caveat stays because following the advice without it bites real
	// hosts: the channel is not persisted anywhere (a bare re-run on a main host quietly
	// moves it onto releases).
	if offeredInstaller {
		b.WriteString("\nRe-running the installer applies each installer command above: it fetches the newest version on\nthe channel in effect and re-applies the bundle (release is the default). The channel\nis not persisted, so pass FELIS_VERSION_BOOTSTRAP=dev if this host tracks main.\nfelis setup is not this path: on a completed install it opens the config console and\ninstalls nothing newer. Restart game servers afterwards.\n")
	}
	return b.String()
}

// installerRef is the git ref the installer re-run reads bootstrap.sh from: the newest
// stable felis release when the feed answered, which is the release that script then
// installs; else the release this host runs; main only when neither is a release tag.
func installerRef(byComponent map[string]updates.Action) string {
	a, ok := byComponent["felis-api"]
	if !ok {
		return "main"
	}
	if a.LatestKnown && isReleaseTag(a.Latest) {
		return a.Latest.String()
	}
	if isReleaseTag(a.Current) {
		return a.Current.String()
	}
	return "main"
}

// isReleaseTag reports whether v was read from a stable vX.Y.Z tag, the only refs
// release.yml publishes a binary for. A source build stamps v0.0.0+g<commit>, which
// names no tag, so build metadata disqualifies a version too.
func isReleaseTag(v updates.Version) bool {
	s := v.String()
	if !strings.HasPrefix(s, "v") || v.IsPrerelease() || strings.Contains(s, "+") {
		return false
	}
	_, err := updates.Parse(s)
	return err == nil
}

// updateWindowTimeout bounds the maintenance-window read, so an unreachable
// database costs the report a line and never the report itself.
const updateWindowTimeout = 3 * time.Second

// readUpdateWindow reads the maintenance window the panel stores
// (platform_settings "update_window"). Felis applies nothing on its own: this
// command is the window's consumer, showing it and warning before an apply
// outside it. A missing row is an unset window.
func readUpdateWindow(ctx context.Context, cfgPath string) (updates.Window, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return updates.Window{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, updateWindowTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return updates.Window{}, err
	}
	defer conn.Close(context.Background())
	var raw []byte
	err = conn.QueryRow(ctx, `SELECT value FROM platform_settings WHERE key = 'update_window'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return updates.Window{}, nil
	}
	if err != nil {
		return updates.Window{}, err
	}
	var w updates.Window
	if err := json.Unmarshal(raw, &w); err != nil {
		return updates.Window{}, fmt.Errorf("stored window: %w", err)
	}
	return w, nil
}

// renderWindowLine is the report's first line: where now sits against the
// maintenance window.
func renderWindowLine(w updates.Window, err error, now time.Time) string {
	const layout = "2006-01-02 15:04 MST"
	switch {
	case err != nil:
		return fmt.Sprintf("Maintenance window: unknown (%v).\n", err)
	case w.Start.IsZero() || w.End.IsZero():
		return "Maintenance window: not set; apply whenever suits you.\n"
	case w.Contains(now):
		return fmt.Sprintf("Maintenance window: open now, until %s.\n", w.End.Local().Format(layout))
	case now.Before(w.Start):
		return fmt.Sprintf("Maintenance window: opens %s, until %s. Felis applies nothing on its own; run the apply commands inside it.\n", w.Start.Local().Format(layout), w.End.Local().Format(layout))
	default:
		return fmt.Sprintf("Maintenance window: ended %s; set a new one in the panel before applying.\n", w.End.Local().Format(layout))
	}
}
