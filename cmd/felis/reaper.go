package main

import (
	"context"
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
	"felis.lolicon.best/internal/reaper"
	"felis.lolicon.best/internal/store"
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

	drv, err := store.Open(ctx, cfg.Database.URL)
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
