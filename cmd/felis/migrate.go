package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/store"
)

// cmdMigrate implements `felis migrate up`: load config, open the database, and
// apply every pending embedded migration under the advisory lock (spec §6).
//
// Migrations only roll forward, and some drop data (0017_drop_password), so a
// database that already holds a schema and has migrations pending is bundled
// first (internal/dbbackup, label pre-migrate). A failed snapshot stops the
// upgrade; -no-backup is the explicit way past it, e.g. for an external
// database (no [database] deployment, so the host's own pg_dump runs) whose
// server is newer than that pg_dump.
func cmdMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	backupDir := fs.String("backup-dir", dbbackup.DefaultDir, "where the pre-migration snapshot goes")
	noBackup := fs.Bool("no-backup", false, "apply pending migrations without snapshotting the database first")
	// The "up" verb precedes any flags (felis migrate up -config path). Go's
	// flag.Parse stops at the first non-flag token and would never see a flag
	// placed after "up", silently falling back to the default -config. Pull the
	// verb off the front, then parse the remaining flags.
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

// preMigrateBackup bundles the database when it already carries a schema and
// some of migrations are not applied yet, and returns the bundle's path ("" when
// there was nothing to protect: a fresh database, or nothing pending).
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
	return dbbackup.Backup(ctx, dbbackup.BackupOptions{
		DatabaseURL: db.URL, Tools: tools, Dir: dir, Label: dbbackup.LabelPreMigrate,
		Keep: defaultKeep[dbbackup.LabelPreMigrate], StateDir: dbbackup.DefaultStateDir,
		Version: resolvedVersion(), Log: log, Record: true,
	})
}

func hasPending(done map[int]struct{}, migrations []store.Migration) bool {
	for _, m := range migrations {
		if _, ok := done[m.Version]; !ok {
			return true
		}
	}
	return false
}

// openStore opens the business database for a command that reads and writes its
// tables, and refuses one whose schema this build was not written against: a newer
// Felis migrated it (a rolled-back binary), or, unless allowPending, migrations this
// build embeds have not run yet (a binary swapped in ahead of `felis migrate up`).
func openStore(ctx context.Context, url string, allowPending bool) (*store.PostgresDriver, error) {
	drv, err := store.Open(ctx, url)
	if err != nil {
		return nil, err
	}
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
