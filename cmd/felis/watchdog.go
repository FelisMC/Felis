package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"felis.lolicon.best/internal/config"
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
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml (the host copy, which reaches PostgreSQL on 127.0.0.1)")
	statePath := fs.String("state", "/var/lib/felis/watchdog/state.json", "state kept between runs (root only: it caches the relay password)")
	smtpPasswordFile := fs.String("smtp-password-file", hostSMTPPasswordPath, "the relay password `felis setup` keeps on the host; the felis-smtp Secret stands in while it is missing")
	quietPath := fs.String("quiet-file", "/run/felis/watchdog-quiet-until", "Unix time before which nothing is mailed; the installer writes it while it restarts things on purpose")
	backupDir := fs.String("backup-dir", "/var/lib/felis/db-backups", `control-plane database backups to check for freshness ("" skips the check)`)
	diskPaths := fs.String("disk-paths", "/,/var/lib/rancher/k3s,/var/lib/felis", "comma-separated paths whose filesystems must keep free space")
	proxyAddr := fs.String("proxy-addr", "", `game proxy address to dial, e.g. 127.0.0.1:25565 ("" skips the check)`)
	nodeIP := fs.String("node-ip", "", `the node address the install was made on, which must stay on this host ("" skips the check)`)
	controlNS := fs.String("control-namespace", platform.DefaultControlNamespace, "namespace of the control plane")
	offsiteStatus := fs.String("offsite-status", offsite.DefaultStatusFile, "the record `felis offsite sync` leaves, checked when [offsite] is configured")
	toolsStatus := fs.String("build-tools-status", defaultBuildToolsStatus, "the record `felis mirror-build-tools` leaves, checked when builds scan against the registry's DB copy")
	dryRun := fs.Bool("dry-run", false, "print every finding and the mail that is due; send nothing and keep the state as it was")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis watchdog: %v\n", err)
		return 1
	}
	state, err := watchdog.LoadState(*statePath)
	if err != nil {
		fmt.Fprintf(stderr, "felis watchdog: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	now := time.Now()

	var report watchdog.Report
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
		found, err = watchdog.Cluster{Client: cl, ControlNamespace: *controlNS, MinecraftNamespace: minecraftNS}.Check(ctx, now)
	}
	if err != nil {
		f := watchdog.KubeAPIDown(err)
		add(&f)
		report.Unknown = append(report.Unknown, watchdog.ClusterPrefixes...)
	} else {
		report.Findings = append(report.Findings, found...)
	}
	if cfg.SMTP.Host != "" {
		var secrets client.Client
		if err == nil {
			secrets = cl
		}
		refreshSMTPPassword(ctx, *smtpPasswordFile, secrets, *controlNS, state, stderr)
	}

	if recipients, err := ownerEmails(ctx, cfg.Database.URL); err != nil {
		f := watchdog.PostgresDown(err)
		add(&f)
	} else {
		state.Recipients = recipients
	}

	if *proxyAddr != "" {
		add(proxyFinding(ctx, *proxyAddr))
	}
	if *backupDir != "" {
		add(watchdog.BackupFinding(*backupDir, now))
	}
	if cfg.Offsite.Enabled() {
		add(watchdog.OffsiteFinding(*offsiteStatus, now))
	}
	if usesMirroredScanDB(cfg) {
		add(watchdog.ScanDBFinding(*toolsStatus, now))
	}
	report.Findings = append(report.Findings, watchdog.DiskFindings(splitList(*diskPaths))...)
	add(watchdog.MemoryFinding("/proc/meminfo"))
	if *nodeIP != "" {
		if held, err := watchdog.HostAddresses(); err == nil {
			add(watchdog.AddressFinding(*nodeIP, held))
		}
	}
	add(watchdog.ClockFinding(watchdog.ClockStatus()))

	if len(report.Findings) == 0 {
		fmt.Fprintln(stdout, "felis watchdog: every check passed")
	}
	for _, f := range report.Findings {
		fmt.Fprintf(stdout, "felis watchdog: [%s] %s: %s\n", f.Severity, f.Key, f.SummaryEN)
	}

	plan := state.Observe(report, now)
	host, _ := os.Hostname()
	subject, body := plan.Message(host, now)
	if *dryRun {
		if plan.Empty() {
			fmt.Fprintln(stdout, "felis watchdog: nothing is due to be mailed")
		} else {
			fmt.Fprintf(stdout, "felis watchdog: due to be mailed to %s:\nSubject: %s\n\n%s", strings.Join(state.Recipients, ", "), subject, strings.ReplaceAll(body, "\r\n", "\n"))
		}
		return 0
	}

	save := func() int {
		if err := watchdog.SaveState(*statePath, state); err != nil {
			fmt.Fprintf(stderr, "felis watchdog: save state: %v\n", err)
			return 1
		}
		return 0
	}
	if plan.Empty() {
		return save()
	}
	if hold := mailHold(*quietPath, cfg.Offsite.Enabled(), *offsiteStatus, now); hold != "" {
		fmt.Fprintf(stdout, "felis watchdog: %s; holding this mail: %s\n", hold, subject)
		return save()
	}
	switch {
	case cfg.SMTP.Host == "":
		fmt.Fprintf(stdout, "felis watchdog: no [smtp] relay configured, so this is logged only: %s\n", subject)
	case len(state.Recipients) == 0:
		fmt.Fprintf(stdout, "felis watchdog: no owner account has a verified email, so this is logged only: %s\n", subject)
	default:
		if err := sendAlert(ctx, cfg, state, subject, body); err != nil {
			// Not committed: the same alerts come due again next run.
			fmt.Fprintf(stderr, "felis watchdog: mail %q: %v\n", subject, err)
			save()
			return 1
		}
		fmt.Fprintf(stdout, "felis watchdog: mailed %s: %s\n", strings.Join(state.Recipients, ", "), subject)
	}
	state.Commit(plan, now)
	return save()
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

// ownerEmails pings PostgreSQL and returns the verified addresses of the
// enabled owner accounts, the people who can act on an alert.
func ownerEmails(ctx context.Context, url string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	drv, err := store.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	defer drv.Close()
	rows, err := drv.DB().QueryContext(ctx,
		`SELECT email FROM users
		 WHERE role = 'owner' AND email_verified AND COALESCE(email, '') <> ''
		   AND NOT disabled AND deleted_at IS NULL
		 ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var email sql.NullString
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		out = append(out, email.String)
	}
	return out, rows.Err()
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

// sendAlert mails subject/body to every recipient; it fails only when no
// recipient got it.
func sendAlert(ctx context.Context, cfg *config.Config, state *watchdog.State, subject, body string) error {
	password := state.SMTPPassword
	if ref := cfg.SMTP.PasswordRef; ref != "" && os.Getenv(ref) != "" {
		password = os.Getenv(ref)
	}
	relay := smtpRelay(cfg.SMTP, password)
	var errs []error
	for _, to := range state.Recipients {
		if err := relay.SendNotice(ctx, to, subject, body); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", to, err))
		}
	}
	if len(errs) == len(state.Recipients) {
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
