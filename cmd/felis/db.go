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
	"path/filepath"
	"strings"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/retention"
)

const dbUsage = `usage:
  felis db backup  [-config path] [-dir dir] [-label daily|manual|...] [-keep n] [-state-dir dir]
                   [-no-servers] [-metrics-file path]
  felis db restore [-config path] [-dir dir] [-yes] [-force] [-no-safety-backup] <bundle>
  felis db verify  [-dir dir] <bundle>
  felis db list    [-dir dir]
  felis db check   [-dir dir] [-max-age 26h]
  felis db audit-export [-config path] [-since date] [-until date] [-out file]
`

// defaultKeep is how many bundles of a label a backup leaves behind. Manual
// bundles are the operator's own and are never pruned.
var defaultKeep = map[string]int{
	dbbackup.LabelDaily:      14,
	dbbackup.LabelPreMigrate: 10,
	dbbackup.LabelPreRestore: 5,
}

// cmdDB implements `felis db`: logical backups of the control-plane database
// together with the host state a rebuild needs (internal/dbbackup). The verb
// comes first for the same reason as `felis migrate up`.
func cmdDB(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, dbUsage)
		return 2
	}
	verb, rest := args[0], args[1:]
	fs := flag.NewFlagSet("db "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, dbUsage) }
	dir := fs.String("dir", dbbackup.DefaultDir, "bundle directory")
	switch verb {
	case "backup":
		return dbBackup(fs, dir, rest, stdout, stderr)
	case "restore":
		return dbRestore(fs, dir, rest, stdout, stderr)
	case "verify":
		return dbVerify(fs, dir, rest, stdout, stderr)
	case "list":
		return dbList(fs, dir, rest, stdout, stderr)
	case "check":
		return dbCheck(fs, dir, rest, stdout, stderr)
	case "audit-export":
		return dbAuditExport(fs, rest, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, dbUsage)
		return 0
	}
	fmt.Fprintf(stderr, "felis db: unknown verb %q\n%s", verb, dbUsage)
	return 2
}

// parseWithArg parses flags that may sit on either side of one positional
// argument (`restore -yes x.tar` and `restore x.tar -yes` both work) and
// returns that argument.
func parseWithArg(fs *flag.FlagSet, args []string) (string, bool) {
	if err := fs.Parse(args); err != nil {
		return "", false
	}
	if fs.NArg() == 0 {
		return "", true
	}
	arg := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return "", false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "felis db: unexpected argument %q\n", fs.Arg(0))
		return "", false
	}
	return arg, true
}

func dbDatabaseURL(path string) (string, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return "", err
	}
	return cfg.Database.URL, nil
}

func dbBackup(fs *flag.FlagSet, dir *string, args []string, stdout, stderr io.Writer) int {
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	label := fs.String("label", dbbackup.LabelManual, "bundle label; daily/pre-migrate/pre-restore bundles are pruned, manual ones never")
	keep := fs.Int("keep", -1, "bundles of this label to keep (default: daily 14, pre-migrate 10, pre-restore 5, manual all)")
	stateDir := fs.String("state-dir", dbbackup.DefaultStateDir, `host state directory to bundle ("" for none)`)
	noServers := fs.Bool("no-servers", false, "leave the MinecraftServer objects out of the bundle")
	metrics := fs.String("metrics-file", "", "node-exporter textfile to rewrite on success (e.g. /var/lib/node_exporter/textfile_collector/felis_db_backup.prom)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprint(stderr, dbUsage)
		return 2
	}
	url, err := dbDatabaseURL(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis db backup: %v\n", err)
		return 1
	}
	if *keep < 0 {
		*keep = defaultKeep[*label]
	}
	o := dbbackup.BackupOptions{
		DatabaseURL: url, Dir: *dir, Label: *label, Keep: *keep,
		StateDir: *stateDir, Version: resolvedVersion(), Log: stderr,
		MetricsFile: *metrics, Record: true,
	}
	if !*noServers {
		o.ExportServers = exportMinecraftServers
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	path, err := dbbackup.Backup(ctx, o)
	if err != nil {
		fmt.Fprintf(stderr, "felis db backup: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "felis db backup: wrote %s\n", path)
	return 0
}

// resolveBundle accepts a path, or a bare bundle name looked up in dir.
func resolveBundle(dir, arg string) string {
	if strings.ContainsRune(arg, os.PathSeparator) {
		return arg
	}
	if _, err := os.Stat(arg); err == nil {
		return arg
	}
	return filepath.Join(dir, arg)
}

func dbRestore(fs *flag.FlagSet, dir *string, args []string, stdout, stderr io.Writer) int {
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	yes := fs.Bool("yes", false, "replace the database's contents (required)")
	force := fs.Bool("force", false, "restore even while other clients are connected")
	noSafety := fs.Bool("no-safety-backup", false, "skip the bundle of the current database taken first")
	stateDir := fs.String("state-dir", dbbackup.DefaultStateDir, "host state directory for the safety bundle")
	arg, ok := parseWithArg(fs, args)
	if !ok {
		return 2
	}
	if arg == "" {
		fmt.Fprint(stderr, dbUsage)
		return 2
	}
	bundle := resolveBundle(*dir, arg)
	m, err := dbbackup.Verify(bundle)
	if err != nil {
		fmt.Fprintf(stderr, "felis db restore: %v\n", err)
		return 1
	}
	if !*yes {
		fmt.Fprintf(stderr, "felis db restore: this replaces every table in the felis database with %s (%s, taken %s, schema %d).\n",
			filepath.Base(bundle), m.Label, m.CreatedAt.Format(time.RFC3339), m.SchemaVersion)
		fmt.Fprintln(stderr, "Scale felis-api and felis-operator to 0 first, then re-run with -yes.")
		return 2
	}
	url, err := dbDatabaseURL(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis db restore: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	_, safety, err := dbbackup.Restore(ctx, dbbackup.RestoreOptions{
		DatabaseURL: url, Bundle: bundle, Dir: *dir, Force: *force, SkipSafetyBackup: *noSafety,
		Safety: dbbackup.BackupOptions{Keep: defaultKeep[dbbackup.LabelPreRestore], StateDir: *stateDir,
			Version: resolvedVersion(), ExportServers: exportMinecraftServers},
		Log: stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "felis db restore: %v\n", err)
		if errors.Is(err, dbbackup.ErrClientsConnected) {
			fmt.Fprintln(stderr, "  kubectl -n felis scale deployment felis-api felis-operator --replicas=0")
		}
		return 1
	}
	fmt.Fprintf(stdout, "felis db restore: restored %s (schema %d)\n", filepath.Base(bundle), m.SchemaVersion)
	if safety != "" {
		fmt.Fprintf(stdout, "  the database as it was before is in %s\n", safety)
	}
	// Nothing migrates at startup, so a control plane newer than the bundle needs
	// its migrations re-applied; rolling back to the release that wrote the bundle
	// must skip that, or the rollback is undone.
	fmt.Fprintf(stdout, "  next: felis migrate up -config %s (skip it when rolling back to felis %s, which wrote this bundle)\n", *cfgPath, orUnknown(m.FelisVersion))
	fmt.Fprintln(stdout, "        kubectl -n felis scale deployment felis-api felis-operator --replicas=1")
	return 0
}

func dbVerify(fs *flag.FlagSet, dir *string, args []string, stdout, stderr io.Writer) int {
	arg, ok := parseWithArg(fs, args)
	if !ok {
		return 2
	}
	if arg == "" {
		fmt.Fprint(stderr, dbUsage)
		return 2
	}
	bundle := resolveBundle(*dir, arg)
	m, err := dbbackup.Verify(bundle)
	if err != nil {
		fmt.Fprintf(stderr, "felis db verify: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: ok\n  taken   %s (%s)\n  felis   %s\n  schema  %d\n  %s\n",
		filepath.Base(bundle), m.CreatedAt.Format(time.RFC3339), m.Label, orUnknown(m.FelisVersion), m.SchemaVersion, orUnknown(m.PGDumpVersion))
	for _, f := range m.Files {
		if f.Link != "" {
			fmt.Fprintf(stdout, "  %-40s -> %s\n", f.Name, f.Link)
			continue
		}
		fmt.Fprintf(stdout, "  %-40s %d bytes\n", f.Name, f.Size)
	}
	if m.ServersError != "" {
		fmt.Fprintf(stdout, "  (no MinecraftServer objects: %s)\n", m.ServersError)
	}
	return 0
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func dbList(fs *flag.FlagSet, dir *string, args []string, stdout, stderr io.Writer) int {
	if err := fs.Parse(args); err != nil {
		return 2
	}
	all, err := dbbackup.List(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "felis db list: %v\n", err)
		return 1
	}
	if len(all) == 0 {
		fmt.Fprintf(stdout, "no database backups in %s\n", *dir)
		return 0
	}
	now := time.Now()
	for _, b := range all {
		fmt.Fprintf(stdout, "%-50s %-12s %10s  %s ago\n", b.Name, b.Label, humanBytes(b.Size), dbbackup.Age(now.Sub(b.Created)))
	}
	return 0
}

// dbAuditExport writes audit rows to a file (or stdout) as JSON lines, so an
// install can keep them past [audit] retention, after which felis-api deletes them.
func dbAuditExport(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	sinceFlag := fs.String("since", "", "first day (or RFC 3339 instant) to export, inclusive; empty starts at the oldest row")
	untilFlag := fs.String("until", "", "day (or RFC 3339 instant) to stop before, exclusive; empty runs to the newest row")
	out := fs.String("out", "", "file to write (created 0600, never overwritten); empty writes to stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprint(stderr, dbUsage)
		return 2
	}
	since, err := parseExportBound(*sinceFlag)
	if err != nil {
		fmt.Fprintf(stderr, "felis db audit-export: -since: %v\n", err)
		return 2
	}
	until, err := parseExportBound(*untilFlag)
	if err != nil {
		fmt.Fprintf(stderr, "felis db audit-export: -until: %v\n", err)
		return 2
	}
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		fmt.Fprintf(stderr, "felis db audit-export: -until %s is not after -since %s\n", *untilFlag, *sinceFlag)
		return 2
	}
	url, err := dbDatabaseURL(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis db audit-export: %v\n", err)
		return 1
	}
	w := stdout
	var f *os.File
	if *out != "" {
		if f, err = os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
			fmt.Fprintf(stderr, "felis db audit-export: %v\n", err)
			return 1
		}
		w = f
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	n, err := exportAudit(ctx, url, since, until, w)
	if f != nil {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "felis db audit-export: %v (%d rows written)\n", err, n)
		return 1
	}
	fmt.Fprintf(stderr, "felis db audit-export: %d audit rows written\n", n)
	return 0
}

func exportAudit(ctx context.Context, url string, since, until time.Time, w io.Writer) (int, error) {
	drv, err := openStore(ctx, url, false)
	if err != nil {
		return 0, fmt.Errorf("open database: %w", err)
	}
	defer drv.Close()
	return retention.ExportAudit(ctx, drv.DB(), since, until, w)
}

// parseExportBound reads a -since/-until value: a day (midnight UTC) or an
// RFC 3339 instant; empty is an open bound.
func parseExportBound(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%q is neither a day (2026-01-31) nor an RFC 3339 instant (2026-01-31T12:00:00Z)", v)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// dbCheck is the freshness probe: exit 1 when the newest bundle is missing or
// older than -max-age, for a monitor or the break-glass console to act on.
func dbCheck(fs *flag.FlagSet, dir *string, args []string, stdout, stderr io.Writer) int {
	maxAge := fs.Duration("max-age", dbbackup.StaleAfter, "oldest acceptable newest bundle")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	b, err := dbbackup.Check(*dir, *maxAge, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "felis db check: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "felis db check: ok, newest backup %s (%s ago)\n", b.Name, dbbackup.Age(time.Since(b.Created)))
	return 0
}

// exportMinecraftServers reads every MinecraftServer through the host's k3s
// kubectl and strips what the API server owns, so the result can be fed back
// with `kubectl apply -f` on a rebuilt cluster.
func exportMinecraftServers(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Output, not the CombinedOutput kubectlOutput uses: a deprecation warning
	// on stderr must not end up inside the JSON.
	cmd := exec.CommandContext(ctx, "k3s", "kubectl", "get", "minecraftservers.felis.lolicon.best", "-A", "-o", "json")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+hostBootstrapKubeconfigPath)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("k3s kubectl get minecraftservers: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return cleanServerList(out)
}

// cleanServerList drops status and the server-assigned metadata from a
// `kubectl get -o json` List.
func cleanServerList(raw []byte) ([]byte, error) {
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("parse MinecraftServer list: %w", err)
	}
	for _, it := range list.Items {
		delete(it, "status")
		if md, ok := it["metadata"].(map[string]any); ok {
			for _, k := range []string{"resourceVersion", "uid", "creationTimestamp", "generation", "managedFields", "selfLink"} {
				delete(md, k)
			}
		}
	}
	if list.Items == nil {
		list.Items = []map[string]any{}
	}
	return json.MarshalIndent(map[string]any{"apiVersion": "v1", "kind": "List", "items": list.Items}, "", "  ")
}
