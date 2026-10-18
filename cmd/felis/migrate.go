package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/store"
)

// Take a pre-migration bundle unless -no-backup explicitly overrides the safety check.
func cmdMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	backupDir := fs.String("backup-dir", dbbackup.DefaultDir, "where the pre-migration snapshot goes")
	noBackup := fs.Bool("no-backup", false, "apply pending migrations without snapshotting the database first")
	// The "up" verb precedes any flags: Go's flag.Parse stops at the first non-flag token, so the verb is pulled off the front before the rest is parsed.
	if len(args) == 0 || args[0] != "up" {
		fmt.Fprintln(stderr, "usage: felis migrate up [-config path]")
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis migrate: %v\n", err)
		return 1
	}

	ctx := context.Background()
	drv, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		fmt.Fprintf(stderr, "felis migrate: open database: %v\n", err)
		return 1
	}
	defer drv.Close()

	migrations, err := store.LoadMigrations()
	if err != nil {
		fmt.Fprintf(stderr, "felis migrate: load migrations: %v\n", err)
		return 1
	}

	if !*noBackup {
		path, err := preMigrateBackup(ctx, drv, migrations, cfg.Database, *backupDir, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "felis migrate: pre-migration backup failed, nothing applied: %v\n", err)
			fmt.Fprintln(stderr, "  fix the backup, or re-run with -no-backup to migrate without one")
			return 1
		}
		if path != "" {
			fmt.Fprintf(stdout, "felis migrate: database snapshot %s\n", path)
		}
	}

	applied, err := store.Up(ctx, drv, migrations)
	if err != nil {
		fmt.Fprintf(stderr, "felis migrate: %v\n", err)
		return 1
	}
	if len(applied) == 0 {
		fmt.Fprintln(stdout, "felis migrate: database already up to date")
	} else {
		fmt.Fprintf(stdout, "felis migrate: applied %d migration(s): %v\n", len(applied), applied)
	}
	return 0
}

// preMigrateStateDir is the host state a pre-migrate bundle carries; tests
// point it at a directory of their own.
var preMigrateStateDir = dbbackup.DefaultStateDir

// Back up initialized databases with pending migrations; otherwise return no bundle path.
func preMigrateBackup(ctx context.Context, drv store.Driver, migrations []store.Migration, db config.DatabaseConfig, dir string, log io.Writer) (string, error) {
	if err := drv.EnsureVersionTable(ctx); err != nil {
		return "", fmt.Errorf("ensure version table: %w", err)
	}
	done, err := drv.AppliedVersions(ctx)
	if err != nil {
		return "", fmt.Errorf("read applied versions: %w", err)
	}
	if len(done) == 0 || !hasPending(done, migrations) {
		return "", nil
	}
	tools, err := dbTools(db)
	if err != nil {
		return "", err
	}
	path, err := dbbackup.Backup(ctx, dbbackup.BackupOptions{
		DatabaseURL: db.URL, Tools: tools, Dir: dir, Label: dbbackup.LabelPreMigrate,
		Keep: defaultKeep[dbbackup.LabelPreMigrate], StateDir: preMigrateStateDir,
		Version: resolvedVersion(), ExportServers: exportMinecraftServers, Log: log, Record: true,
	})
	if errors.Is(err, dbbackup.ErrServersMissing) {
		// Rolling the migration back needs the database alone. Backup logged
		// the gap, and the panel and the watchdog show it while this is the
		// newest bundle.
		return path, nil
	}
	return path, err
}

func hasPending(done map[int]struct{}, migrations []store.Migration) bool {
	for _, m := range migrations {
		if _, ok := done[m.Version]; !ok {
			return true
		}
	}
	return false
}

// Reject unknown applied migrations and, unless allowPending, pending embedded migrations.
func openStore(ctx context.Context, url string, allowPending bool) (*store.PostgresDriver, error) {
	drv, err := store.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	return checkSchema(ctx, drv, allowPending)
}

// Retry initial database dials while startup NetworkPolicy rules converge.
var (
	podDBWindow   = time.Minute
	podDBInterval = time.Second
)

// openPodStore is openStore for felis-api and the reaper and backup Jobs, whose first
// dial comes milliseconds after their pod starts.
func openPodStore(ctx context.Context, url, prog string, stderr io.Writer) (*store.PostgresDriver, error) {
	drv, err := store.OpenRetrying(ctx, url, podDBWindow, podDBInterval, func(err error) {
		fmt.Fprintf(stderr, "felis %s: %v; retrying (a pod that has just started waits for the network policy to admit it)\n", prog, err)
	})
	if err != nil {
		return nil, err
	}
	return checkSchema(ctx, drv, false)
}

// checkSchema closes drv and fails when its schema is not the one this build was
// written against (see openStore).
func checkSchema(ctx context.Context, drv *store.PostgresDriver, allowPending bool) (*store.PostgresDriver, error) {
	s, err := store.ReadSchema(ctx, drv)
	if err == nil {
		err = s.Err()
		if allowPending {
			err = s.Newer()
		}
	}
	if err != nil {
		drv.Close()
		return nil, err
	}
	return drv, nil
}
