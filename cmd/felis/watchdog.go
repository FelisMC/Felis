package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/mail"
	"felis.lolicon.best/internal/offsite"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/store"
	"felis.lolicon.best/internal/watchdog"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// proxyFor is how long the game proxy may refuse connections before it is
// mailed: a restart takes seconds.
const proxyFor = 3 * time.Minute

// cmdWatchdog runs one pass of the platform watchdog (internal/watchdog): it
// checks the cluster, PostgreSQL, the game proxy, the database backups and the
// host (disks, memory, its address and clock), prints every finding, and mails
// the platform owners what came due.
// deploy/bootstrap.sh runs it every two minutes from felis-watchdog.timer.
func cmdWatchdog(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("watchdog", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var w watchdogFlags
	w.register(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	now := time.Now()
	if w.unitFailed {
		return watchdogUnitFailed(unitFailedRun{
			cfgPath: w.cfgPath, statePath: w.statePath, fallbackPath: w.fallbackState, quietPath: w.quietPath,
			offsiteStatus: w.offsiteStatus, heartbeatFile: w.heartbeatFile,
			result: os.Getenv("MONITOR_SERVICE_RESULT"), exitStatus: os.Getenv("MONITOR_EXIT_STATUS"),
			send: watchdogSender, client: http.DefaultClient, now: now,
		}, stdout, stderr)
	}
	cfg, err := config.Load(w.cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis watchdog: %v\n", err)
		return 1
	}
	loadPath := watchdog.NewestState(w.statePath, w.fallbackState)
	state, aside, err := watchdog.RecoverState(loadPath, now)
	if err != nil {
		fmt.Fprintf(stderr, "felis watchdog: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var report watchdog.Report
	if aside != "" {
		fmt.Fprintf(stderr, "felis watchdog: %s was unreadable; moved it to %s and started over\n", loadPath, aside)
		report.Findings = append(report.Findings, watchdog.StateSetAside(aside))
	}
	cl, owners, ownersErr := watchdogProbes(ctx, w, cfg, now, &report)
	if cfg.SMTP.Host != "" {
		refreshSMTPPassword(ctx, w.smtpPasswordFile, cl, w.controlNS, state, stderr)
	}
	state.Relay = cachedRelay(cfg.SMTP)
	if ownersErr == nil {
		state.Recipients = owners
	}

	if len(report.Findings) == 0 {
		fmt.Fprintln(stdout, "felis watchdog: every check passed")
	}
	for _, f := range report.Findings {
		fmt.Fprintf(stdout, "felis watchdog: [%s] %s: %s\n", f.Severity, f.Key, f.SummaryEN)
	}

	plan := state.Observe(report, now)
	host, _ := os.Hostname()
	subject, body := plan.Message(host, now)
	beat := heartbeat{
		standby: standsBy(cfg.Offsite.Enabled(), w.offsiteStatus),
		quiet:   now.Before(watchdog.QuietUntil(w.quietPath)),
	}
	if beat.url, err = readHeartbeatURL(w.heartbeatFile); err != nil {
		fmt.Fprintf(stderr, "felis watchdog: %v; pinging no heartbeat\n", err)
	}
	if w.dryRun {
		if plan.Empty() {
			fmt.Fprintln(stdout, "felis watchdog: nothing is due to be mailed")
		} else {
			fmt.Fprintf(stdout, "felis watchdog: due to be mailed to %s:\nSubject: %s\n\n%s", strings.Join(state.Recipients, ", "), subject, strings.ReplaceAll(body, "\r\n", "\n"))
		}
		if beat.url != "" {
			fmt.Fprintf(stdout, "felis watchdog: a run pings the heartbeat at %s\n", redactURL(beat.url))
		}
		return 0
	}

	m := configMailer(cfg.SMTP, state.SMTPPassword, watchdogSender)
	unheard, mailFailed := m.deliver(ctx, state, plan, subject, body, mailHold(w.quietPath, cfg.Offsite.Enabled(), w.offsiteStatus, now), now, stdout, stderr)
	saveErr := watchdog.SaveStateOr(w.statePath, w.fallbackState, state)
	if saveErr != nil {
		fmt.Fprintf(stderr, "felis watchdog: save state: %v\n", saveErr)
	}
	beat.report = failureReport(unheard, mailFailed, saveErr, state.Open(), report)
	beat.fail = beat.report != ""
	beat.send(http.DefaultClient, stdout, stderr)
	if mailFailed || saveErr != nil {
		return 1
	}
	return 0
}

// watchdogFlags are felis watchdog's flags. felis doctor reads them back from
// the ExecStart= line of felis-watchdog.service, so it checks what the timer's
// runs check, with the same paths.
type watchdogFlags struct {
	cfgPath, statePath, fallbackState, smtpPasswordFile, quietPath string
	backupDir, diskPaths, certDirs, proxyAddr, nodeIP, controlNS   string
	offsiteStatus, toolsStatus, heartbeatFile                      string
	dryRun, unitFailed                                             bool
}

func (w *watchdogFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&w.cfgPath, "config", "/etc/felis/felis.toml", "path to felis.toml (the host copy, which reaches PostgreSQL on 127.0.0.1)")
	fs.StringVar(&w.statePath, "state", "/var/lib/felis/watchdog/state.json", "state kept between runs (root only: it caches the relay password)")
	fs.StringVar(&w.fallbackState, "fallback-state", watchdog.FallbackStatePath, "where a run keeps its state while -state cannot be written, so what it mailed is not mailed again (tmpfs: until the host restarts; \"\" keeps none)")
	fs.StringVar(&w.smtpPasswordFile, "smtp-password-file", hostSMTPPasswordPath, "the relay password `felis setup` keeps on the host; the felis-smtp Secret stands in while it is missing")
	fs.StringVar(&w.quietPath, "quiet-file", "/run/felis/watchdog-quiet-until", "Unix time before which nothing is mailed; the installer writes it while it restarts things on purpose")
	fs.StringVar(&w.backupDir, "backup-dir", "/var/lib/felis/db-backups", `control-plane database backups to check for freshness ("" skips the check)`)
	fs.StringVar(&w.diskPaths, "disk-paths", "/,/var/lib/rancher/k3s,/var/lib/felis", "comma-separated paths whose filesystems must keep free space")
	fs.StringVar(&w.certDirs, "k3s-cert-dirs", strings.Join(watchdog.K3sCertDirs, ","), `k3s certificate directories whose *.crt files must not be near expiry ("" skips the check)`)
	fs.StringVar(&w.proxyAddr, "proxy-addr", "", `game proxy address to dial, e.g. 127.0.0.1:25565 ("" skips the check)`)
	fs.StringVar(&w.nodeIP, "node-ip", "", `the node address the install was made on, which must stay on this host ("" skips the check)`)
	fs.StringVar(&w.controlNS, "control-namespace", platform.DefaultControlNamespace, "namespace of the control plane")
	fs.StringVar(&w.offsiteStatus, "offsite-status", offsite.DefaultStatusFile, "the record `felis offsite sync` leaves, checked when [offsite] is configured")
	fs.StringVar(&w.toolsStatus, "build-tools-status", defaultBuildToolsStatus, "the record `felis mirror-build-tools` leaves, checked when builds scan against the registry's DB copy")
	fs.BoolVar(&w.dryRun, "dry-run", false, "print every finding and the mail that is due; send nothing and keep the state as it was")
	fs.StringVar(&w.heartbeatFile, "heartbeat-file", defaultHeartbeatFile, "file holding the heartbeat URL each run pings, a dead man's switch at a monitoring service that alerts when the pings stop (no file pings nothing)")
	fs.BoolVar(&w.unitFailed, "unit-failed", false, "report a failed run of felis-watchdog.service instead of checking; felis-watchdog-failed.service runs this through OnFailure=")
}

// watchdogProbes is one pass of every check, appended to report. cl is the
// cluster client, nil while the API server is unreachable; owners is who the
// alerts go to, and ownersErr why PostgreSQL did not say.
func watchdogProbes(ctx context.Context, w watchdogFlags, cfg *config.Config, now time.Time, report *watchdog.Report) (cl client.Client, owners []string, ownersErr error) {
	add := func(f *watchdog.Finding) {
		if f != nil {
			report.Findings = append(report.Findings, *f)
		}
	}

	// The cluster: one unreachable API server stands in for every check behind it.
	minecraftNS := cfg.K8s.Namespace
	if minecraftNS == "" {
		minecraftNS = platform.DefaultMinecraftNamespace
	}
	cl, err := buildSystemServerClient()
	var found []watchdog.Finding
	if err == nil {
		found, err = watchdog.Cluster{Client: cl, ControlNamespace: w.controlNS, MinecraftNamespace: minecraftNS}.Check(ctx, now)
	}
	if err != nil {
		cl = nil
		f := watchdog.KubeAPIDown(err)
		add(&f)
		report.Unknown = append(report.Unknown, watchdog.ClusterPrefixes...)
	} else {
		report.Findings = append(report.Findings, found...)
	}

	if owners, ownersErr = ownerEmails(ctx, cfg.Database.URL); ownersErr != nil {
		f := watchdog.PostgresDown(ownersErr)
		add(&f)
	}

	if w.proxyAddr != "" {
		add(proxyFinding(ctx, w.proxyAddr))
	}
	if w.backupDir != "" {
		add(watchdog.BackupFinding(w.backupDir, now))
	}
	if cfg.Offsite.Enabled() {
		add(watchdog.OffsiteFinding(w.offsiteStatus, now))
	}
	if usesMirroredScanDB(cfg) {
		add(watchdog.ScanDBFinding(w.toolsStatus, now))
	}
	report.Findings = append(report.Findings, watchdog.DiskFindings(splitList(w.diskPaths))...)
	add(watchdog.MemoryFinding("/proc/meminfo"))
	add(watchdog.CertFinding(splitList(w.certDirs), now))
	if w.nodeIP != "" {
		if held, err := watchdog.HostAddresses(); err == nil {
			add(watchdog.AddressFinding(w.nodeIP, held))
		}
	}
	add(watchdog.ClockFinding(watchdog.ClockStatus()))
	return cl, owners, ownersErr
}

// failureReport is what the heartbeat's failure ping carries, "" when the run
// pings success: the alerts this run knows of reach no one (a mail that
// failed, or no relay or recipient while something is open), or the state did
// not save to its file: kept on tmpfs it holds until the host restarts, which
// forgets what was mailed, and with nowhere to keep it the next run mails the
// same alerts again.
func failureReport(unheard string, mailFailed bool, saveErr error, open bool, r watchdog.Report) string {
	var why []string
	if unheard != "" && (mailFailed || open) {
		why = append(why, "the alerts reach no one: "+unheard)
	}
	if saveErr != nil {
		why = append(why, "the watchdog state did not save: "+saveErr.Error())
	}
	if len(why) == 0 {
		return ""
	}
	return strings.Join(why, "\n") + "\n\n" + findingLines(r)
}

// findingLines is the report as the journal shows it.
func findingLines(r watchdog.Report) string {
	var b strings.Builder
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "[%s] %s: %s\n", f.Severity, f.Key, f.SummaryEN)
	}
	return b.String()
}

// mailer is how a run reaches the owners.
type mailer struct {
	relay    *watchdog.Relay // nil: no [smtp] relay
	password string
	send     func(*mail.SMTP) alertSender
}

// deliver mails plan to the owners unless hold says why it waits, and commits
// it once it reached them, or once it is logged because nothing can reach them.
// unheard is why the owners hear nothing of this run's alerts, "" when they do;
// mailFailed is a mail that did not go out, left uncommitted so the same
// alerts come due again next run.
func (m mailer) deliver(ctx context.Context, state *watchdog.State, plan watchdog.Plan, subject, body, hold string, now time.Time, stdout, stderr io.Writer) (unheard string, mailFailed bool) {
	switch {
	case m.relay == nil:
		unheard = "no [smtp] relay is configured"
	case len(state.Recipients) == 0:
		unheard = "no owner account has a verified email"
	}
	switch {
	case plan.Empty():
	case hold != "":
		fmt.Fprintf(stdout, "felis watchdog: %s; holding this mail: %s\n", hold, subject)
	case unheard != "":
		fmt.Fprintf(stdout, "felis watchdog: %s, so this is logged only: %s\n", unheard, subject)
		state.Commit(plan, now)
	default:
		relay := &mail.SMTP{Host: m.relay.Host, Port: m.relay.Port, From: m.relay.From, Username: m.relay.Username, Password: m.password, RequireTLS: m.relay.RequireTLS}
		if err := sendAlert(ctx, m.send(relay), state.Recipients, subject, body); err != nil {
			fmt.Fprintf(stderr, "felis watchdog: mail %q: %v\n", subject, err)
			return "the alert mail failed: " + err.Error(), true
		}
		fmt.Fprintf(stdout, "felis watchdog: mailed %s: %s\n", strings.Join(state.Recipients, ", "), subject)
		state.Commit(plan, now)
	}
	return unheard, false
}

// configMailer reaches the owners through the relay felis.toml configures.
func configMailer(c config.SMTPConfig, cachedPassword string, send func(*mail.SMTP) alertSender) mailer {
	return mailer{relay: cachedRelay(c), password: smtpPassword(c, cachedPassword), send: send}
}

// cachedRelay is what State.Relay keeps of c, nil when no relay is configured.
func cachedRelay(c config.SMTPConfig) *watchdog.Relay {
	if c.Host == "" {
		return nil
	}
	return &watchdog.Relay{Host: c.Host, Port: c.Port, From: c.From, Username: c.Username, RequireTLS: c.TLSRequired()}
}

// smtpPassword is the relay password a mail signs in with: the env var [smtp]
// password_ref names when it is set, else the one the state caches.
func smtpPassword(c config.SMTPConfig, cached string) string {
	if ref := c.PasswordRef; ref != "" && os.Getenv(ref) != "" {
		return os.Getenv(ref)
	}
	return cached
}

// unitFailedRun is one run of felis-watchdog-failed.service.
type unitFailedRun struct {
	cfgPath, statePath, fallbackPath, quietPath, offsiteStatus, heartbeatFile string
	// result and exitStatus are what systemd hands an OnFailure= unit
	// (MONITOR_SERVICE_RESULT, MONITOR_EXIT_STATUS; systemd 251 and later).
	result, exitStatus string
	send               func(*mail.SMTP) alertSender
	client             *http.Client
	now                time.Time
}

// watchdogUnitFailed is felis-watchdog-failed.service, which systemd starts
// through OnFailure= when a run of felis-watchdog.service fails: a crash, a
// felis.toml that no longer loads, a hang past the unit's timeout, a mail
// that did not go out. Such a run checks and mails nothing, so this records
// the failure as the alert watchdog/run, due after five failed runs in a row
// and cleared by the next run that succeeds; mails it through the relay the
// last good run cached when felis.toml does not load; and pings the
// heartbeat's failure endpoint. Every other alert keeps its state.
func watchdogUnitFailed(r unitFailedRun, stdout, stderr io.Writer) int {
	detail := failureDetail(r.result, r.exitStatus)
	cfg, cfgErr := config.Load(r.cfgPath)
	if cfgErr != nil {
		detail += "; " + cfgErr.Error()
	}
	fmt.Fprintf(stdout, "felis watchdog: felis-watchdog.service failed: %s\n", detail)
	offsiteOn := cfgErr != nil || cfg.Offsite.Enabled()
	beat := heartbeat{
		fail:    true,
		report:  "felis-watchdog.service failed: " + detail,
		standby: standsBy(offsiteOn, r.offsiteStatus),
		quiet:   r.now.Before(watchdog.QuietUntil(r.quietPath)),
	}
	var err error
	if beat.url, err = readHeartbeatURL(r.heartbeatFile); err != nil {
		fmt.Fprintf(stderr, "felis watchdog: %v; pinging no heartbeat\n", err)
	}
	state, err := watchdog.LoadState(watchdog.NewestState(r.statePath, r.fallbackPath))
	if err != nil {
		// The next run that gets that far moves a state that does not parse
		// aside (watchdog.RecoverState).
		fmt.Fprintf(stderr, "felis watchdog: %v; mailing nothing\n", err)
		beat.report += "\nThe watchdog state does not load either, so nothing was mailed: " + err.Error()
		beat.send(r.client, stdout, stderr)
		return 1
	}
	plan := state.Observe(watchdog.Report{Findings: []watchdog.Finding{watchdog.WatchdogFailed(detail)}, Unknown: []string{""}}, r.now)
	host, _ := os.Hostname()
	subject, body := plan.Message(host, r.now)
	m := mailer{relay: state.Relay, password: state.SMTPPassword, send: r.send}
	if cfgErr == nil {
		m = configMailer(cfg.SMTP, state.SMTPPassword, r.send)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	unheard, mailFailed := m.deliver(ctx, state, plan, subject, body, mailHold(r.quietPath, offsiteOn, r.offsiteStatus, r.now), r.now, stdout, stderr)
	code := 0
	if mailFailed {
		code = 1
	}
	if err := watchdog.SaveStateOr(r.statePath, r.fallbackPath, state); err != nil {
		fmt.Fprintf(stderr, "felis watchdog: save state: %v\n", err)
		code = 1
	}
	if unheard != "" {
		beat.report += "\nThe alerts reach no one: " + unheard
	}
	beat.send(r.client, stdout, stderr)
	return code
}

// failureDetail names how felis-watchdog.service failed.
func failureDetail(result, exitStatus string) string {
	switch {
	case result == "":
		return "systemd named no cause (journalctl -u felis-watchdog -n 50)"
	case exitStatus == "":
		return "result " + result
	}
	return "result " + result + ", exit status " + exitStatus
}

// mailHold is why this run's mail waits, "" when it goes out: the installer's
// quiet window, or this host standing by for another host's off-site bucket
// (offsite.Status.StandsBy). A standby host is a rehearsal, or a rebuild not
// yet taken over, and the owners its restored database names are that host's,
// which mails them itself.
func mailHold(quietPath string, offsiteOn bool, offsiteStatus string, now time.Time) string {
	if until := watchdog.QuietUntil(quietPath); now.Before(until) {
		return fmt.Sprintf("quiet until %s (installer running)", until.UTC().Format(time.RFC3339))
	}
	if !offsiteOn {
		return ""
	}
	st, err := offsite.ReadStatus(offsiteStatus)
	if err != nil {
		return ""
	}
	if w := st.StandsBy(now); w != nil {
		return fmt.Sprintf("this host stands by for %s, which wrote the off-site bucket at %s and mails its owners itself", w, w.At.UTC().Format(time.RFC3339))
	}
	return ""
}

// usesMirroredScanDB reports whether build scans read the vulnerability DB copy
// felis mirror-build-tools keeps in the platform registry: the default, or an
// explicit trivy_db_repository under the registry's mirror/.
func usesMirroredScanDB(cfg *config.Config) bool {
	if cfg.Registry.URL == "" {
		return false
	}
	repo := cfg.Registry.TrivyDBRepository
	return repo == "" || strings.HasPrefix(repo, cfg.Registry.URL+"/mirror/")
}

// refreshSMTPPassword caches the relay password (relayPassword: the host copy at
// path, else the felis-smtp Secret), or forgets it when the Secret is gone (a
// relay without AUTH). The host copy is read even while the cluster is down, the
// time an alert matters most; without one, a down cluster keeps the cached
// password. An env var named by [smtp] password_ref, when set, wins at send time
// instead.
func refreshSMTPPassword(ctx context.Context, path string, cl client.Client, ns string, state *watchdog.State, stderr io.Writer) {
	password, err := relayPassword(ctx, path, cl, ns)
	switch {
	case errors.Is(err, errClusterUnreachable):
		return
	case err != nil:
		fmt.Fprintf(stderr, "felis watchdog: read the relay password (keeping the cached one): %v\n", err)
		return
	}
	state.SMTPPassword = password
}

// smtpSecretPassword reads the relay password from the felis-smtp Secret. A missing
// Secret is a relay without AUTH and reads as "".
func smtpSecretPassword(ctx context.Context, cl client.Client, ns string) (string, error) {
	var sec corev1.Secret
	err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: platform.SMTPSecretName}, &sec)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(sec.Data[platform.SMTPSecretPasswordKey]), nil
}

// ownerEmails pings PostgreSQL and returns the owners an alert goes to
// (watchdog.OwnerEmails).
func ownerEmails(ctx context.Context, url string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	drv, err := store.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	defer drv.Close()
	return watchdog.OwnerEmails(ctx, drv.DB())
}

// proxyFinding dials the game proxy; players reach every server through it.
func proxyFinding(ctx context.Context, addr string) *watchdog.Finding {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err == nil {
		conn.Close()
		return nil
	}
	return &watchdog.Finding{
		Key: "proxy", Severity: watchdog.Critical, For: proxyFor,
		Summary:   fmt.Sprintf("游戏代理 %s 无法连接：玩家进不了任何服务器", addr),
		SummaryEN: fmt.Sprintf("the game proxy at %s refuses connections: players cannot reach any server", addr),
		Hint:      fmt.Sprintf("systemctl status felis-velocity; journalctl -u felis-velocity -n 200 (%v)", err),
	}
}

// alertSender mails one alert; smtpSender is the real one.
type alertSender func(ctx context.Context, to, subject, body string) error

func smtpSender(relay *mail.SMTP) alertSender { return relay.SendNotice }

// watchdogSender is what a run mails through; tests stand a recorder in.
var watchdogSender = smtpSender

// sendAlert mails subject/body to every recipient; it fails only when no
// recipient got it.
func sendAlert(ctx context.Context, send alertSender, recipients []string, subject, body string) error {
	var errs []error
	for _, to := range recipients {
		if err := send(ctx, to, subject, body); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", to, err))
		}
	}
	if len(errs) == len(recipients) {
		return errors.Join(errs...)
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
