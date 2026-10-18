package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/archivepolicy"
	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/distributed"
	"felis.lolicon.best/internal/mailsettings"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/reaper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Retention-only runs prune archives without resolving world volumes.
func cmdReaper(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reaper", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	worldsRoot := fs.String("worlds-root", "/worlds", "mount root under which world PVCs are visible (tarLocal: <root>/<pvc>, else the stock local-path <root>/<pv-name>_<ns>_<pvc-name>)")
	retentionOnly := fs.Bool("retention-only", false, "only expire, read back and sweep the archive store: no server is evaluated and no world is read or deleted, so neither the worlds root nor the Kubernetes API is needed")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: %v\n", err)
		return 1
	}

	rcfg, err := reaperConfig(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: %v\n", err)
		return 1
	}

	ctx := ctrl.SetupSignalHandler()

	var cl client.Client
	if !*retentionOnly {
		scheme := runtime.NewScheme()
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		utilruntime.Must(v1alpha1.AddToScheme(scheme))
		cl, err = client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
		if err != nil {
			fmt.Fprintf(stderr, "felis reaper: build k8s client: %v\n", err)
			return 1
		}
	}

	archiver, err := buildArchiver(ctx, cfg, *worldsRoot, cl)
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: %v\n", err)
		return 1
	}

	if os.Getenv("FELIS_DISTRIBUTED") == "true" && !*retentionOnly {
		m, err := distributionManager(cl, cfg, os.Getenv("FELIS_IMAGE"))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		local, ok := archiver.(*backup.TarLocal)
		if !ok {
			fmt.Fprintln(stderr, "remote reaper requires tarLocal")
			return 1
		}
		archiver = &distributed.RemoteArchiver{TarLocal: local, Manager: m}
	}
	drv, err := openPodStore(ctx, cfg.Database.URL, "reaper", stderr)
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: open database: %v\n", err)
		return 1
	}
	defer drv.Close()

	// The Owner's retention policy, when the panel holds one, overrides [archive].
	if err := archivepolicy.ApplyFrom(ctx, drv.DB(), &rcfg); err != nil {
		fmt.Fprintf(stderr, "felis reaper: the panel's retention policy is unreadable, [archive] stands: %v\n", err)
	}

	r := &reaper.Reaper{
		Cfg:      rcfg,
		Store:    reaper.NewPGStore(drv.DB()),
		Archiver: archiver,
	}
	if *retentionOnly {
		fmt.Fprintln(stderr, "felis reaper: retention only — no worlds root is configured, so idle worlds are neither archived nor released")
		return reportReaperRun(r.RunRetention(ctx), stdout, stderr)
	}
	r.Cluster = reaper.NewK8sCluster(cl, cfg.K8s.Namespace).WithDistributed(os.Getenv("FELIS_DISTRIBUTED") == "true")

	// Without SMTP, leave warnings unstamped so configuring mail later still sends them.
	if notifier := reaperNotifier(ctx, drv.DB(), cfg.SMTP, stderr); notifier != nil {
		db := drv.DB()
		r.Warner = &mailWarner{
			lookupEmail: func(ctx context.Context, ownerID string) (string, error) {
				var email string
				switch err := db.QueryRowContext(ctx,
					`SELECT email FROM users
					 WHERE id = $1 AND email_verified = true AND COALESCE(email, '') <> ''`,
					ownerID).Scan(&email); {
				case errors.Is(err, sql.ErrNoRows):
					return "", fmt.Errorf("owner %s has no verified email", ownerID)
				case err != nil:
					return "", err
				}
				return email, nil
			},
			notifier: notifier,
		}
	} else {
		fmt.Fprintln(stderr, "felis reaper: no mail relay (panel or [smtp]) — pre-reap warnings are logged and NOT marked sent")
	}

	sum, err := r.RunOnce(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: %v\n", err)
		return 1
	}
	return reportReaperRun(sum, stdout, stderr)
}

// Exit unsuccessfully on incomplete work so Job failure alerts expose unreaped worlds.
func reportReaperRun(sum reaper.Summary, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "felis reaper: evaluated=%d reaped=%d released=%d deleted=%d awaiting_offsite=%d awaiting_stop=%d warned=%d skipped=%d store_full=%d evicted=%d expired=%d expire_failed=%d verified=%d corrupt=%d verify_failed=%d swept=%d orphan_archives=%d\n",
		sum.Evaluated, sum.WorldsReaped, sum.Released, sum.ServersDeleted, sum.AwaitingOffsite, sum.AwaitingStop, sum.Warned, sum.Skipped, sum.StoreFull,
		sum.EvictedEarly, sum.BackupsExpired, sum.ExpireFailed,
		sum.Verified, sum.Corrupt, sum.VerifyFailed, sum.Swept, sum.OrphanArchives)
	if !sum.Failed() {
		return 0
	}
	if sum.Skipped > 0 || sum.ExpireFailed > 0 {
		fmt.Fprintf(stderr, "felis reaper: %d servers failed (%d kept because the backup store is full) and %d expired backups were not removed; the errors are above, and each is retried next run\n",
			sum.Skipped, sum.StoreFull, sum.ExpireFailed)
	}
	if sum.Corrupt > 0 {
		fmt.Fprintf(stderr, "felis reaper: %d archives did not read back and are marked corrupt; they are no longer offered for restore (the errors are above)\n", sum.Corrupt)
	}
	if sum.VerifyFailed > 0 || sum.SweepFailed {
		fmt.Fprintf(stderr, "felis reaper: %d archives could not be read back and the store sweep completed=%t; both are retried next run\n",
			sum.VerifyFailed, !sum.SweepFailed)
	}
	return 1
}

// Missing verified email prevents a notice but does not block reaping.
type mailWarner struct {
	lookupEmail func(ctx context.Context, ownerID string) (string, error)
	notifier    noticeNotifier
}

// noticeNotifier is the slice of mail.SMTP the warner needs (injected in tests).
type noticeNotifier interface {
	SendNotice(ctx context.Context, email, subject, body string) error
}

func (w *mailWarner) Warn(ctx context.Context, ownerID, server, remaining string) error {
	email, err := w.lookupEmail(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("resolve owner email: %w", err)
	}
	subject := fmt.Sprintf("Felis: 服务器 %s 将在 %s 后回收 · server reaped in %s", server, remaining, remaining)
	body := fmt.Sprintf(
		"Felis 世界回收提醒 / world-reaper notice\r\n"+
			"\r\n"+
			"服务器 / Server: %s\r\n"+
			"距回收 / Time left: %s\r\n"+
			"\r\n"+
			"闲置的服务器会先自动备份，再释放世界；有人加入游戏即可重置倒计时。\r\n"+
			"Idle servers are backed up and then released; any join resets the countdown.\r\n",
		server, remaining)
	return w.notifier.SendNotice(ctx, email, subject, body)
}

// The idle deadline is fixed at 15 days; configured windows control warning and backup retention.
func reaperConfig(cfg *config.Config) (reaper.Config, error) {
	rc := reaper.DefaultConfig()
	if v := cfg.Archive.Retention; v != "" {
		d, err := parseSpanDuration(v)
		if err != nil {
			return rc, fmt.Errorf("[archive] retention %q: %w", v, err)
		}
		rc.Retention = d
	}
	if len(cfg.Archive.WarnBefore) > 0 {
		offs := make([]time.Duration, 0, len(cfg.Archive.WarnBefore))
		for _, w := range cfg.Archive.WarnBefore {
			d, err := parseSpanDuration(w)
			if err != nil {
				return rc, fmt.Errorf("[archive] warn_before %q: %w", w, err)
			}
			offs = append(offs, d)
		}
		rc.WarnBefore = offs
	}
	if v := cfg.Archive.MaxLocalBytes; v != "" {
		b, err := parseByteSize(v)
		if err != nil {
			return rc, fmt.Errorf("[archive] max_local_bytes %q: %w", v, err)
		}
		rc.MaxLocalBytes = b
	}
	if v := cfg.Archive.ManualRetention; v != "" {
		d, err := parseSpanDuration(v)
		if err != nil || d <= 0 {
			return rc, fmt.Errorf("[archive] manual_retention %q: want a positive span such as 30d", v)
		}
		rc.ManualRetention = d
	}
	switch n := cfg.Archive.ManualKeep; {
	case n < 0:
		return rc, fmt.Errorf("[archive] manual_keep %d: want 1 or more", n)
	case n > 0:
		rc.ManualKeep = n
	}
	if v := cfg.Archive.ManualCooldown; v != "" {
		d, err := parseSpanDuration(v)
		if err != nil || d < 0 {
			return rc, fmt.Errorf("[archive] manual_cooldown %q: want a span such as 10m (0s for none)", v)
		}
		rc.ManualCooldown = d
	}
	if v := cfg.Archive.ScheduledEvery; v != "" {
		d, err := parseSpanDuration(v)
		if err != nil || d < 0 {
			return rc, fmt.Errorf("[archive] scheduled_every %q: want a span such as 1d (0s for none)", v)
		}
		rc.ScheduledEvery = d
	}
	switch n := cfg.Archive.ScheduledKeep; {
	case n < 0:
		return rc, fmt.Errorf("[archive] scheduled_keep %d: want 1 or more", n)
	case n > 0:
		rc.ScheduledKeep = n
	}
	if v := cfg.Archive.ScheduledRetention; v != "" {
		d, err := parseSpanDuration(v)
		if err != nil || d <= 0 {
			return rc, fmt.Errorf("[archive] scheduled_retention %q: want a positive span such as 90d", v)
		}
		rc.ScheduledRetention = d
	}
	rc.RequireOffsite = cfg.Offsite.Enabled()
	return rc, nil
}

// A nil cluster client supports retention-only mode and resolves no worlds.
func buildArchiver(ctx context.Context, cfg *config.Config, worldsRoot string, cl client.Client) (backup.WorldArchiver, error) {
	switch cfg.Archive.Store {
	case "tarLocal":
		t := &backup.TarLocal{BackupRoot: cfg.Archive.LocalPath}
		if cl != nil {
			t.Resolve = resolveWorldDir(ctx, cl, cfg.K8s.Namespace, worldsRoot)
		} else {
			t.Resolve = func(pvc string) (string, error) {
				return "", fmt.Errorf("resolve world PVC %s: this run has no worlds root (retention only)", pvc)
			}
		}
		return t, nil
	default:
		return nil, fmt.Errorf("[archive] store %q is not implemented in this build (only tarLocal)", cfg.Archive.Store)
	}
}

// Resolve only the bound PVC's explicit storage path; missing worlds fail rather than matching stale volumes.
func resolveWorldDir(ctx context.Context, cl client.Client, namespace, worldsRoot string) backup.PVCResolver {
	return func(pvc string) (string, error) {
		direct := filepath.Join(worldsRoot, pvc)
		if _, err := os.Stat(direct); err == nil {
			return direct, nil
		}
		var claim corev1.PersistentVolumeClaim
		if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pvc}, &claim); err != nil {
			return "", fmt.Errorf("resolve world PVC %s: %w", pvc, err)
		}
		if pv := claim.Spec.VolumeName; pv != "" {
			volDir := filepath.Join(worldsRoot, fmt.Sprintf("%s_%s_%s", pv, claim.Namespace, claim.Name))
			if _, err := os.Stat(volDir); err == nil {
				return volDir, nil
			}
		}
		return direct, nil
	}
}

// parseSpanDuration parses the human spans used in felis.toml's [archive] table
// (config.ParseSpan).
func parseSpanDuration(s string) (time.Duration, error) { return config.ParseSpan(s) }

// parseByteSize parses a Kubernetes-style quantity ("200Gi", "10G") into bytes.
// An empty string means unlimited (0).
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	units := []struct {
		suffix string
		mul    int64
	}{
		{"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
		{"G", 1_000_000_000}, {"M", 1_000_000}, {"K", 1_000},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			if err != nil {
				return 0, err
			}
			return int64(n * float64(u.mul)), nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// Prefer the panel relay, then TOML; return nil when neither is configured.
func reaperNotifier(ctx context.Context, db *sql.DB, c config.SMTPConfig, stderr io.Writer) noticeNotifier {
	panel, err := mailsettings.Read(ctx, db)
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: the panel's mail settings are unreadable (%v); using [smtp]\n", err)
	}
	if panel != nil {
		return panel.SMTP()
	}
	if c.Host == "" {
		return nil
	}
	passRef := c.PasswordRef
	if passRef == "" {
		passRef = platform.SMTPPasswordEnv
	}
	password := os.Getenv(passRef)
	if c.Username != "" && password == "" {
		fmt.Fprintf(stderr, "felis reaper: warning: [smtp] username is set but credentials env %s is empty — warning emails will fail AUTH\n", passRef)
	}
	return smtpRelay(c, password)
}
