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
	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/mail"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/reaper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// cmdReaper runs one pass of the world reaper / retention batch (spec §18). It
// is intentionally run-once-and-exit: a Kubernetes CronJob drives the daily
// cadence, and RunOnce is idempotent and restart-safe, so a missed or retried
// run simply converges. Only the tarLocal archive backend is wired in this
// build; the snapshot backends (§19) are a later integration.
func cmdReaper(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reaper", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	worldsRoot := fs.String("worlds-root", "/worlds", "mount root under which world PVCs are visible (tarLocal: <root>/<pvc>, else the stock local-path <root>/<pv-name>_<ns>_<pvc-name>)")
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

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	cl, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: build k8s client: %v\n", err)
		return 1
	}

	archiver, err := buildArchiver(ctx, cfg, *worldsRoot, cl)
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: %v\n", err)
		return 1
	}

	drv, err := openStore(ctx, cfg.Database.URL, false)
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: open database: %v\n", err)
		return 1
	}
	defer drv.Close()

	r := &reaper.Reaper{
		Cfg:      rcfg,
		Store:    reaper.NewPGStore(drv.DB()),
		Cluster:  reaper.NewK8sCluster(cl, cfg.K8s.Namespace),
		Archiver: archiver,
	}

	// Pre-reap warnings go out by email when [smtp] is configured (the same
	// relay and password_ref convention felis-api uses); without it the channel
	// stays nil and the reaper logs each suppressed warning instead of stamping
	// it, so a later SMTP setup still gets to warn. The owner must have a
	// VERIFIED address — that flag is what proves the mailbox.
	if cfg.SMTP.Host != "" {
		passRef := cfg.SMTP.PasswordRef
		if passRef == "" {
			passRef = platform.SMTPPasswordEnv
		}
		password := os.Getenv(passRef)
		if cfg.SMTP.Username != "" && password == "" {
			fmt.Fprintf(stderr, "felis reaper: warning: [smtp] username is set but credentials env %s is empty — warning emails will fail AUTH\n", passRef)
		}
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
			notifier: &mail.SMTP{
				Host:     cfg.SMTP.Host,
				Port:     cfg.SMTP.Port,
				From:     cfg.SMTP.From,
				Username: cfg.SMTP.Username,
				Password: password,
			},
		}
	} else {
		fmt.Fprintln(stderr, "felis reaper: [smtp] not configured — pre-reap warnings are logged and NOT marked sent")
	}

	sum, err := r.RunOnce(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: %v\n", err)
		return 1
	}
	return reportReaperRun(sum, stdout, stderr)
}

// reportReaperRun prints the run's tally and turns a run that left work undone
// into exit 1, so the Job fails and the watchdog's job-failed check (and the
// FelisWorldJobFailed rule) reach the operator: a world that cannot be archived
// is kept, and without this nobody would learn that it is never reaped.
func reportReaperRun(sum reaper.Summary, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "felis reaper: evaluated=%d reaped=%d awaiting_offsite=%d warned=%d skipped=%d store_full=%d evicted=%d expired=%d expire_failed=%d verified=%d corrupt=%d verify_failed=%d swept=%d orphan_archives=%d\n",
		sum.Evaluated, sum.WorldsReaped, sum.AwaitingOffsite, sum.Warned, sum.Skipped, sum.StoreFull,
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

// mailWarner delivers a pre-reap notice to the owner's verified email — the
// only channel this build can reach. Unowned owners and owners who never proved
// a mailbox yield an error; the reaper retries such notices on its next run and
// never lets them block the reap (red line ⑤).
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

// reaperConfig derives the reaper's retention windows from felis.toml. The 15d
// idle deadline is fixed by §18; only the warning offsets, retention, the
// store soft-cap and the on-demand backup bounds are configurable (§24). The
// backup Job and felis-api read the manual_* bounds through it too.
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
	rc.RequireOffsite = cfg.Offsite.Enabled()
	return rc, nil
}

// buildArchiver constructs the WorldArchiver. Only tarLocal is implemented in
// this build; the resolver maps each world PVC to its directory under worldsRoot
// (resolveWorldDir).
func buildArchiver(ctx context.Context, cfg *config.Config, worldsRoot string, cl client.Client) (backup.WorldArchiver, error) {
	switch cfg.Archive.Store {
	case "tarLocal":
		return &backup.TarLocal{
			BackupRoot: cfg.Archive.LocalPath,
			Resolve:    resolveWorldDir(ctx, cl, cfg.K8s.Namespace, worldsRoot),
		}, nil
	default:
		return nil, fmt.Errorf("[archive] store %q is not implemented in this build (only tarLocal)", cfg.Archive.Store)
	}
}

// resolveWorldDir maps a world PVC to its directory under worldsRoot, supporting
// the two layouts a Felis host actually has:
//
//  1. <root>/<pvc> — the reaper's documented arrangement (worlds exposed by PVC
//     name, e.g. via mounting each volume or a crafted storage class).
//  2. <root>/<pv-name>_<namespace>_<pvc-name> — what a stock k3s install gets:
//     local-path-provisioner stores every volume under its storage root as that
//     exact directory name. Without this arm, retention on a default install could
//     only ever fail to find a world (a no-op reaper, or worse an operator
//     arranging paths by hand).
//
// The second path is derived EXACTLY from the live PVC's spec.volumeName, never
// from a glob: a leftover directory of an old, deleted PV must never be mistaken
// for the world the PVC currently binds, because the reaper archives the resolved
// directory and then deletes that PVC — archiving stale bytes and deleting the
// real world would be data loss. When neither path exists the first is returned,
// so the archive walk fails loudly against the documented path.
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

// parseSpanDuration parses the human spans used in felis.toml's [archive] table:
// "3mo" (months≈30d), "15d" (days), or any time.ParseDuration unit ("12h").
func parseSpanDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasSuffix(s, "mo"):
		n, err := strconv.Atoi(strings.TrimSuffix(s, "mo"))
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 30 * 24 * time.Hour, nil
	case strings.HasSuffix(s, "d"):
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return time.ParseDuration(s)
	}
}

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
