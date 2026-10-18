package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/heartbeatsettings"
	"felis.lolicon.best/internal/mailsettings"
	"felis.lolicon.best/internal/watchdog"
)

// systemdUnitDir is where the installer writes its units.
const systemdUnitDir = "/etc/systemd/system"

// Preserve tool stdout on failure, including systemctl's inactive response with exit 3.
func hostCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Check installed long-running units only; full and nano hosts depend on different services.
var hostServices = []string{"k3s.service", "felis-velocity.service", "felis-nano.service"}

// It mails nothing, pings no heartbeat and leaves watchdog state alone, so it is safe to run at any time.
func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	unitDir := fs.String("systemd-dir", systemdUnitDir, "where the installer's systemd units are")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis doctor: run as root (sudo felis doctor): the checks read root-only state under /etc/felis and /var/lib/felis")
		return 1
	}
	host, _ := os.Hostname()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return runDoctor(ctx, doctorEnv{unitDir: *unitDir, run: hostCommand, now: time.Now(), host: host, panel: panelSettings}, stdout)
}

// doctorEnv is what one doctor run reads the host through.
type doctorEnv struct {
	unitDir string
	run     func(ctx context.Context, name string, args ...string) ([]byte, error)
	now     time.Time
	host    string
	// panel reads the relay and the heartbeat the Owner saved in the panel, which win over
	// felis.toml's [smtp] and the heartbeat file; nil reads none.
	panel func(ctx context.Context, url string) (*mailsettings.Settings, *heartbeatsettings.Settings, error)
}

// doctorAreas are the report's headings in order, by the area findingArea
// puts a finding under.
var doctorAreas = []struct{ key, title string }{
	{key: "config", title: "configuration"},
	{key: "cluster", title: "Kubernetes cluster"},
	{key: "postgres", title: "PostgreSQL"},
	{key: "proxy", title: "game proxy"},
	{key: "db-backup", title: "database backups"},
	{key: "offsite", title: "off-site copy"},
	{key: "scan-db", title: "build scan database"},
	{key: "disk", title: "disk space"},
	{key: "memory", title: "memory"},
	{key: "k3s-certs", title: "k3s certificates"},
	{key: "host-address", title: "node address"},
	{key: "clock", title: "clock"},
	{key: "systemd", title: "systemd units and timers"},
	{key: "alerts", title: "alerting"},
}

func runDoctor(ctx context.Context, env doctorEnv, stdout io.Writer) int {
	return printDoctor(stdout, collectDoctor(ctx, env))
}

// doctorReport is one doctor run: what it found, the areas it could not check and why,
// and the notes that are not problems.
type doctorReport struct {
	host     string
	at       time.Time
	source   string // how the checks were run, the report's second line
	findings []watchdog.Finding
	skip     map[string]string
	notes    []string
}

// doctorView is the report as the panel shows it: every area in order, with its findings
// or why it was not checked.
type doctorView struct {
	Host     string           `json:"host"`
	At       time.Time        `json:"at"`
	Source   string           `json:"source"`
	Areas    []doctorAreaView `json:"areas"`
	Notes    []string         `json:"notes,omitempty"`
	Critical int              `json:"critical"`
	Warning  int              `json:"warning"`
}

type doctorAreaView struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	// State is ok, problem or skipped; Skipped says why an area was not checked.
	State    string             `json:"state"`
	Skipped  string             `json:"skipped,omitempty"`
	Findings []watchdog.Finding `json:"findings,omitempty"`
}

func (r doctorReport) view() doctorView {
	v := doctorView{Host: r.host, At: r.at.UTC(), Source: r.source, Notes: r.notes}
	byArea := map[string][]watchdog.Finding{}
	for _, f := range r.findings {
		byArea[findingArea(f.Key)] = append(byArea[findingArea(f.Key)], f)
	}
	for _, a := range doctorAreas {
		area := doctorAreaView{Key: a.key, Title: a.title, State: "ok"}
		switch fs := byArea[a.key]; {
		case len(fs) > 0:
			area.State, area.Findings = "problem", fs
			for _, f := range fs {
				if f.Severity == watchdog.Critical {
					v.Critical++
				} else {
					v.Warning++
				}
			}
		case r.skip[a.key] != "":
			area.State, area.Skipped = "skipped", r.skip[a.key]
		}
		v.Areas = append(v.Areas, area)
	}
	return v
}

// printDoctor prints the report and returns the exit status: 1 when anything was found.
func printDoctor(stdout io.Writer, r doctorReport) int {
	fmt.Fprintf(stdout, "felis doctor on %s at %s\n", r.host, r.at.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintln(stdout, r.source)
	fmt.Fprintln(stdout)
	return printDoctorReport(stdout, r.findings, r.skip, r.notes)
}

func collectDoctor(ctx context.Context, env doctorEnv) doctorReport {
	unit := filepath.Join(env.unitDir, "felis-watchdog.service")
	w, found, unitErr := watchdogUnitFlags(unit)
	var report watchdog.Report
	var notes []string
	out := doctorReport{host: env.host, at: env.now}
	switch {
	case unitErr != nil:
		report.Findings = append(report.Findings, watchdog.Finding{
			Key: "watchdog/unit", Severity: watchdog.Critical,
			SummaryEN: fmt.Sprintf("cannot read the watchdog's settings: %v", unitErr),
			Hint:      "rerun the installer (deploy/bootstrap.sh) to rewrite felis-watchdog.service",
		})
		out.source = fmt.Sprintf("checks run with the watchdog's defaults (config %s)", w.cfgPath)
	case !found:
		report.Findings = append(report.Findings, watchdog.Finding{
			Key: "watchdog/unit", Severity: watchdog.Critical,
			SummaryEN: fmt.Sprintf("%s is not installed: nothing checks this host or mails anyone when it breaks", unit),
			Hint:      "rerun the installer (deploy/bootstrap.sh), which installs felis-watchdog.timer",
		})
		out.source = fmt.Sprintf("checks run with the watchdog's defaults (config %s)", w.cfgPath)
	default:
		out.source = fmt.Sprintf("checks run as %s runs them (config %s)", unit, w.cfgPath)
	}

	skip := map[string]string{}
	panelRelay, panelBeat := false, false
	cfg, cfgErr := config.Load(w.cfgPath)
	if cfgErr != nil {
		report.Findings = append(report.Findings, watchdog.Finding{
			Key: "config", Severity: watchdog.Critical,
			SummaryEN: cfgErr.Error(),
			Hint:      "the installer writes it (deploy/bootstrap.sh); felis watchdog fails on every run until it loads",
		})
		// The host's units are read without it; whether alerts reach anyone
		// is not known without its relay.
		for _, a := range doctorAreas {
			if a.key != "config" && a.key != "systemd" {
				skip[a.key] = "the configuration did not load"
			}
		}
	} else {
		// What the database does not yield leaves felis.toml and the heartbeat file to judge
		// by; PostgreSQL's own finding says why it could not be read.
		if env.panel != nil {
			relay, beat, _ := env.panel(ctx, cfg.Database.URL)
			panelRelay, panelBeat = relay != nil, beat != nil
		}
		_, owners, ownersErr := watchdogProbes(ctx, w, cfg, env.now, &report)
		report.Findings = append(report.Findings, alertReachFindings(cfg, panelRelay, owners, ownersErr)...)
		if w.proxyAddr == "" {
			skip["proxy"] = "no -proxy-addr"
		}
		if w.backupDir == "" {
			skip["db-backup"] = "no -backup-dir"
		}
		if !cfg.Offsite.Enabled() {
			skip["offsite"] = "not configured"
		}
		if !usesMirroredScanDB(cfg) {
			skip["scan-db"] = "builds do not scan against the registry's copy"
		}
		if len(splitList(w.certDirs)) == 0 {
			skip["k3s-certs"] = "no -k3s-cert-dirs"
		}
		if w.nodeIP == "" {
			skip["host-address"] = "no -node-ip"
		}
	}
	report.Findings = append(report.Findings, unitFindings(ctx, env)...)

	switch url, err := readHeartbeatURL(w.heartbeatFile); {
	case panelBeat:
		// The panel's heartbeat wins: the file is not read by the watchdog then.
	case err != nil:
		report.Findings = append(report.Findings, watchdog.Finding{
			Key: "watchdog/heartbeat", Severity: watchdog.Warning,
			SummaryEN: fmt.Sprintf("the heartbeat URL is unusable, so no run pings it: %v", err),
			Hint:      "set one in the panel (Platform settings → Monitoring heartbeat), or rerun the installer with FELIS_WATCHDOG_HEARTBEAT_URL (https://docs.felismc.com/en/operations/monitoring#heartbeat)",
		})
	case url == "":
		notes = append(notes, "no heartbeat URL is set: a host that goes down entirely, or a watchdog that stops running, alerts no one. "+
			"Set one in the panel (Platform settings → Monitoring heartbeat), or rerun the installer with FELIS_WATCHDOG_HEARTBEAT_URL (https://docs.felismc.com/en/operations/monitoring#heartbeat)")
	}
	if until := watchdog.QuietUntil(w.quietPath); env.now.Before(until) {
		notes = append(notes, fmt.Sprintf("the watchdog mails nothing until %s (%s): the installer holds it while it restarts things on purpose, "+
			"and a marker an installer killed mid-run left behind holds it until then",
			until.UTC().Format("2006-01-02 15:04 UTC"), w.quietPath))
	}
	out.findings, out.skip, out.notes = report.Findings, skip, notes
	return out
}

func printDoctorReport(stdout io.Writer, findings []watchdog.Finding, skip map[string]string, notes []string) int {
	byArea := map[string][]watchdog.Finding{}
	for _, f := range findings {
		a := findingArea(f.Key)
		byArea[a] = append(byArea[a], f)
	}
	var critical, warning int
	for _, a := range doctorAreas {
		fs := byArea[a.key]
		switch {
		case len(fs) > 0:
		case skip[a.key] != "":
			fmt.Fprintf(stdout, "-  %s: not checked, %s\n", a.title, skip[a.key])
			continue
		default:
			fmt.Fprintf(stdout, "✓  %s\n", a.title)
			continue
		}
		mark := "!"
		if slices.ContainsFunc(fs, func(f watchdog.Finding) bool { return f.Severity == watchdog.Critical }) {
			mark = "✗"
		}
		fmt.Fprintf(stdout, "%s  %s\n", mark, a.title)
		for _, f := range fs {
			if f.Severity == watchdog.Critical {
				critical++
			} else {
				warning++
			}
			fmt.Fprintf(stdout, "     %-8s %s: %s\n", f.Severity, f.Key, f.SummaryEN)
			if f.Hint != "" {
				fmt.Fprintf(stdout, "              → %s\n", f.Hint)
			}
		}
	}
	for _, n := range notes {
		fmt.Fprintf(stdout, "\nnote: %s\n", n)
	}
	fmt.Fprintln(stdout)
	if critical+warning == 0 {
		fmt.Fprintln(stdout, "no problems found")
		return 0
	}
	fmt.Fprintf(stdout, "%d problem(s): %d critical, %d warning(s)\n", critical+warning, critical, warning)
	return 1
}

// findingArea is the report heading a finding key goes under.
func findingArea(key string) string {
	head, _, _ := strings.Cut(key, "/")
	switch head {
	case "kube-api", "deployment", "system-server", "server-failed", "job-failed", "reaper-stale", "node":
		return "cluster"
	case "db-backup", "db-backup-servers":
		return "db-backup"
	case "unit", "timer":
		return "systemd"
	case "watchdog":
		return "alerts"
	}
	return head
}

// alertReachFindings is why the watchdog's alerts would reach no one, which
// its own runs only log: no relay (in the panel or in felis.toml), or no owner with a
// verified address.
func alertReachFindings(cfg *config.Config, panelRelay bool, owners []string, ownersErr error) []watchdog.Finding {
	var out []watchdog.Finding
	if cfg.SMTP.Host == "" && !panelRelay {
		out = append(out, watchdog.Finding{
			Key: "alerts/relay", Severity: watchdog.Warning,
			SummaryEN: "no mail relay is configured, in the panel or in [smtp]: the watchdog logs its alerts to the journal and mails no one",
			Hint:      "the panel's Platform settings → Mail delivery, or sudo felis setup, step SMTP",
		})
	}
	if ownersErr == nil && len(owners) == 0 {
		out = append(out, watchdog.Finding{
			Key: "alerts/recipients", Severity: watchdog.Warning,
			SummaryEN: "no owner account has a verified email: the watchdog's alerts reach no one",
			Hint:      "an owner verifies an address in the panel's account settings",
		})
	}
	return out
}

// unitFindings reports the installer's systemd units that failed, the
// long-running ones that are not running, and timers that no longer fire.
func unitFindings(ctx context.Context, env doctorEnv) []watchdog.Finding {
	var out []watchdog.Finding
	seen := map[string]bool{}
	failed, err := env.run(ctx, "systemctl", "list-units", "--all", "--plain", "--no-legend", "--no-pager", "--state=failed", "felis-*", "k3s.service")
	if err != nil && len(failed) == 0 {
		return []watchdog.Finding{{
			Key: "unit/systemctl", Severity: watchdog.Warning,
			SummaryEN: fmt.Sprintf("systemctl list-units failed, so no unit was checked: %v", err),
		}}
	}
	for _, line := range strings.Split(string(failed), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		seen[name] = true
		out = append(out, watchdog.Finding{
			Key: "unit/" + name, Severity: watchdog.Critical,
			SummaryEN: name + " failed",
			Hint:      fmt.Sprintf("journalctl -u %s -n 100 --no-pager; once fixed, sudo systemctl reset-failed %s (a timer's job clears on its next good run)", name, name),
		})
	}
	for _, name := range hostServices {
		if seen[name] {
			continue
		}
		if _, err := os.Stat(filepath.Join(env.unitDir, name)); err != nil {
			continue
		}
		if state := unitActiveState(ctx, env, name); state != "active" {
			out = append(out, watchdog.Finding{
				Key: "unit/" + name, Severity: watchdog.Critical,
				SummaryEN: fmt.Sprintf("%s is %s", name, state),
				Hint:      fmt.Sprintf("sudo systemctl start %s; journalctl -u %s -n 100 --no-pager", name, name),
			})
		}
	}
	timers, _ := filepath.Glob(filepath.Join(env.unitDir, "felis-*.timer"))
	for _, path := range timers {
		name := filepath.Base(path)
		if state := unitActiveState(ctx, env, name); state != "active" {
			out = append(out, watchdog.Finding{
				Key: "timer/" + name, Severity: watchdog.Warning,
				SummaryEN: fmt.Sprintf("%s is %s: the job it starts no longer runs", name, state),
				Hint:      fmt.Sprintf("sudo systemctl enable --now %s", name),
			})
		}
	}
	return out
}

// unitActiveState is what `systemctl is-active` says of unit.
func unitActiveState(ctx context.Context, env doctorEnv, unit string) string {
	out, err := env.run(ctx, "systemctl", "is-active", unit)
	if state := strings.TrimSpace(string(out)); state != "" {
		return state
	}
	return fmt.Sprintf("in an unknown state (systemctl is-active: %v)", err)
}

// watchdogUnitFlags reads the flags felis-watchdog.service runs felis
// watchdog with. found is false when there is no such unit; w is then the
// watchdog's defaults.
func watchdogUnitFlags(path string) (w watchdogFlags, found bool, err error) {
	fs := flag.NewFlagSet("watchdog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	w.register(fs)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return w, false, nil
	}
	if err != nil {
		return w, false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		cmd, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		fields := execArgs(cmd)
		i := slices.Index(fields, "watchdog")
		if i < 0 {
			continue
		}
		if err := fs.Parse(fields[i+1:]); err != nil {
			return w, true, fmt.Errorf("%s: %w", path, err)
		}
		return w, true, nil
	}
	return w, true, fmt.Errorf("%s runs no `felis watchdog`", path)
}

// execArgs splits an ExecStart= command line into its words. A word may be
// quoted with " or ', as systemd allows, which is how an empty value is
// written.
func execArgs(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}
