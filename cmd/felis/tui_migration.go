package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/store"
)

// applyMigrations runs any pending schema migrations and returns the resulting
// applied count. Used by the preflight stage to self-heal a freshly bootstrapped
// (or upgraded) database.
func applyMigrations(dbURL string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	drv, err := store.Open(ctx, dbURL)
	if err != nil {
		return 0, fmt.Errorf("open db: %w", err)
	}
	defer drv.Close()
	migrations, err := store.LoadMigrations()
	if err != nil {
		return 0, err
	}
	// Same guard as `felis migrate up`: never roll a populated database forward
	// without a snapshot to roll back to.
	if _, err := preMigrateBackup(ctx, drv, migrations, dbURL, dbbackup.DefaultDir, io.Discard); err != nil {
		return 0, fmt.Errorf("pre-migration backup: %w", err)
	}
	if _, err := store.Up(ctx, drv, migrations); err != nil {
		return 0, err
	}
	applied, err := drv.AppliedVersions(ctx)
	if err != nil {
		return 0, err
	}
	return len(applied), nil
}

func totalMigrations() (int, error) {
	migrations, err := store.LoadMigrations()
	if err != nil {
		return 0, err
	}
	return len(migrations), nil
}
