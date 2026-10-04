package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
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

// updateTarget maps report selectors onto the components tracked by the planner.
// Application always reuses bootstrap.sh for the compatible platform bundle.
type updateTarget struct {
	selector  string
	help      string
	component string
	note      string // explanation for the untracked Minecraft selector
}

// updateTargets is the selector table. panel and plugins both resolve to felis-api
// because they are not separately versioned: the panel is compiled into the felis
// binary with //go:embed, and the plugin jars are built from this same repo in the
// same bootstrap run, so all three move together and carry one version.
var updateTargets = []updateTarget{
	{
		selector:  "panel",
		help:      "select the panel + control plane (felis-api)",
		component: "felis-api",
	},
	{
		selector:  "self",
		help:      "select the Felis binary and its core services",
		component: "felis-api",
	},
	{
		selector:  "velocity",
		help:      "select the Velocity proxy",
		component: "velocity",
	},
	{
		selector:  "plugins",
		help:      "select the Felis plugin jars (velocity/paper/limbo)",
		component: "felis-api",
	},
	{
		selector:  "k3s",
		help:      "select k3s",
		component: "k3s",
	},
	{
		selector:  "cloudflared",
		help:      "select cloudflared",
		component: "cloudflared",
	},
	{
		selector:  "jre",
		help:      "select the Temurin JRE Velocity runs on",
		component: "jre",
	},
	{
		selector:  "postgres",
		help:      "select PostgreSQL",
		component: "postgresql",
	},
	{
		selector:  "mc",
		help:      "select Minecraft (pinned; reported only)",
		component: "", // never tracked: see the pin note below
		note:      "Minecraft is pinned by policy (\"能不动的就别动\") and Felis never proposes a version change for it. A server's version is a property of that server's image — change it on the server, not through a platform update",
	},
}

// cmdUpdate checks by default; only --apply changes the host. --record remains
// read-only so the daily timer cannot start an unattended upgrade.
func cmdUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	flags := map[string]*bool{}
	for _, t := range updateTargets {
		flags[t.selector] = fs.Bool(t.selector, false, t.help)
	}
	var opts updateOptions
	fs.BoolVar(&opts.all, "all", false, "include the release-pinned k3s and cloudflared updates")
	fs.BoolVar(&opts.force, "force", false, "reinstall even at the same version; allow an explicitly requested Felis downgrade (does not bypass maintenance or dependency guards)")
	fs.BoolVar(&opts.apply, "apply", false, "apply the inspected target after checking maintenance and taking a database/state backup")
	fs.BoolVar(&opts.now, "now", false, "explicitly start manual maintenance now instead of using the configured window (requires --apply)")
	fs.StringVar(&opts.release, "version", "", "install a published Felis release, e.g. v0.2.0")
	fs.StringVar(&opts.expectedCommit, "expect-commit", "", "refuse application if the target differs from the full SHA printed by the check")
	fs.StringVar(&opts.ref, "ref", "", "build an exact commit, tag or branch from source")
	dev := fs.Bool("dev", false, "build the newest main commit (the check prints its full SHA)")
	check := fs.Bool("check", false, "check and print the apply command without changing the host (default)")
	velocityJar := fs.String("velocity-jar", updater.DefaultVelocityJarPath, "path to the installed Velocity jar")
	fs.StringVar(&opts.cfgPath, "config", "/etc/felis/felis.toml", "host config for the maintenance window and pre-update backup")
	record := fs.Bool("record", false, "store this read-only check for the panel (used by the daily timer)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "felis update: unexpected argument %q (this command takes flags only)\n", fs.Arg(0))
		return 2
	}
	if err := opts.validate(*dev, *check, *record); err != nil {
		fmt.Fprintf(stderr, "felis update: %v\n", err)
		return 2
	}
	if *dev {
		opts.ref = "main"
	}
	opts.selected = map[string]bool{}
	for sel, on := range flags {
		if *on || opts.all {
			opts.selected[sel] = true
		}
	}
	if len(opts.selected) == 1 && opts.selected["mc"] {
		if opts.apply {
			fmt.Fprintln(stderr, "felis update: Minecraft is managed through each server's image, not a platform update")
			return 2
		}
		for _, t := range updateTargets {
			if t.selector == "mc" {
				fmt.Fprintln(stdout, t.note)
			}
		}
		return 0
	}
	if opts.apply && os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis update: --apply must run as root (use sudo)")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !opts.apply {
		if code := checkUpdates(ctx, opts, *velocityJar, *record, stdout, stderr); code != 0 {
			return code
		}
		if *record {
			return 0
		}
	}
	source, err := updater.NewInstallerSource(os.Getenv("FELIS_REPO_URL"))
	if err != nil {
		fmt.Fprintf(stderr, "felis update: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Resolving the Felis target and downloading its matching installer...")
	lookup, cancel := context.WithTimeout(ctx, updateTimeout)
	target, err := source.Prepare(lookup, opts.release, opts.ref)
	cancel()
	if err != nil {
		fmt.Fprintf(stderr, "felis update: cannot prepare an update: %v\n", err)
		return 1
	}
	if opts.expectedCommit != "" && target.Revision != opts.expectedCommit {
		fmt.Fprintf(stderr, "felis update: target moved since the check: expected %s, got %s; check again before applying\n", opts.expectedCommit, target.Revision)
		return 1
	}
	syntax := exec.CommandContext(ctx, "bash", "-n")
	syntax.Stdin, syntax.Stderr = strings.NewReader(target.Script), stderr
	if err := syntax.Run(); err != nil {
		fmt.Fprintf(stderr, "felis update: invalid installer: %v\n", err)
		return 1
	}
	if os.Getenv("FELIS_REPO_URL") != "" {
		opts.repoURL = source.RepoURL()
	}
	opts.preserveToken = os.Getenv("FELIS_GITHUB_TOKEN") != ""
	fmt.Fprint(stdout, renderInstallPlan(target, opts))
	if !opts.apply {
		return 0
	}
	if err := applyHostUpdate(ctx, target, opts, source.RepoURL(), stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "felis update: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Felis binary and core components updated and verified.")
	return 0
}

func checkUpdates(ctx context.Context, opts updateOptions, velocityJar string, record bool, stdout, stderr io.Writer) int {
	lookup, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	src := updater.NewRoutingSource(updater.Topology())
	rn := &updater.Runner{Gatherer: updater.NewHostGatherer(resolvedVersion(), velocityJar), Source: src}
	fmt.Fprintln(stdout, "Checking installed components and upstream versions (read-only)...")
	res, err := rn.Run(lookup, time.Now(), updates.Window{})
	if err != nil {
		fmt.Fprintf(stderr, "felis update: %v\n", err)
		return 1
	}
	now := time.Now()
	win, winErr := readUpdateWindow(ctx, opts.cfgPath)
	fmt.Fprint(stdout, renderWindowLine(win, winErr, now))
	fmt.Fprint(stdout, renderUpdateReport(res, opts.selected))
	fmt.Fprint(stdout, renderNotes(src.Notes(), opts.selected))
	if record {
		rctx, rcancel := context.WithTimeout(ctx, updateWindowTimeout)
		defer rcancel()
		if err := recordUpdateStatus(rctx, opts.cfgPath, buildStatusReport(res, src.Notes(), resolvedVersion(), now)); err != nil {
			fmt.Fprintf(stderr, "felis update: record the check for the panel: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "Recorded this check for the panel's Updates page.")
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

// updateWindowTimeout bounds the maintenance-window read, so an unreachable
// database costs the report a line and never the report itself.
const updateWindowTimeout = 3 * time.Second

// readUpdateWindow reads the maintenance window the panel stores
// (platform_settings "update_window"). Felis applies nothing on its own: this
// command consumes it for both reporting and admission to an explicit apply.
// A missing row is an unset window.
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
		return "Maintenance window: not set; configure it in the panel, or explicitly use --apply --now for manual maintenance.\n"
	case w.Contains(now):
		return fmt.Sprintf("Maintenance window: open now, until %s.\n", w.End.Local().Format(layout))
	case now.Before(w.Start):
		return fmt.Sprintf("Maintenance window: opens %s, until %s. Apply is blocked until this window opens (unless --now explicitly starts manual maintenance).\n", w.Start.Local().Format(layout), w.End.Local().Format(layout))
	default:
		return fmt.Sprintf("Maintenance window: ended %s; apply is blocked; set a new window in the panel or explicitly use --now.\n", w.End.Local().Format(layout))
	}
}
