package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/reaper"
	"felis.lolicon.best/internal/store"
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
	worldsRoot := fs.String("worlds-root", "/worlds", "mount root under which world PVCs are visible (tarLocal: <root>/<pvc>)")
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

	archiver, err := buildArchiver(cfg, *worldsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: %v\n", err)
		return 1
	}

	ctx := ctrl.SetupSignalHandler()

	drv, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: open database: %v\n", err)
		return 1
	}
	defer drv.Close()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	cl, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: build k8s client: %v\n", err)
		return 1
	}

	r := &reaper.Reaper{
		Cfg:      rcfg,
		Store:    reaper.NewPGStore(drv.DB()),
		Cluster:  reaper.NewK8sCluster(cl, cfg.K8s.Namespace),
		Archiver: archiver,
	}

	sum, err := r.RunOnce(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "felis reaper: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "felis reaper: evaluated=%d reaped=%d warned=%d skipped=%d evicted=%d expired=%d\n",
		sum.Evaluated, sum.WorldsReaped, sum.Warned, sum.Skipped, sum.EvictedEarly, sum.BackupsExpired)
	return 0
}

// reaperConfig derives the reaper's retention windows from felis.toml. The 15d
// idle deadline is fixed by §18; only the warning offsets, retention, and the
// store soft-cap are configurable (§24).
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
	return rc, nil
}

// buildArchiver constructs the WorldArchiver. Only tarLocal is implemented in
// this build; the resolver maps each world PVC to <worldsRoot>/<pvc>, the mount
// convention the reaper Job is deployed with.
func buildArchiver(cfg *config.Config, worldsRoot string) (backup.WorldArchiver, error) {
	switch cfg.Archive.Store {
	case "tarLocal":
		return &backup.TarLocal{
			BackupRoot: cfg.Archive.LocalPath,
			Resolve: func(pvc string) (string, error) {
				return filepath.Join(worldsRoot, pvc), nil
			},
		}, nil
	default:
		return nil, fmt.Errorf("[archive] store %q is not implemented in this build (only tarLocal)", cfg.Archive.Store)
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
