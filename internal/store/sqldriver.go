package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver
)

// PostgresDriver is the production Driver, backed by a database/sql pool using
// the pgx stdlib driver.
type PostgresDriver struct {
	db *sql.DB
}

// Open dials dsn and returns a PostgresDriver. The caller owns Close.
func Open(ctx context.Context, dsn string) (*PostgresDriver, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &PostgresDriver{db: db}, nil
}

// DB exposes the underlying pool for the access layer.
func (d *PostgresDriver) DB() *sql.DB { return d.db }

// Close releases the pool.
func (d *PostgresDriver) Close() error { return d.db.Close() }

// Lock takes the session-level advisory lock that serializes migrations.
func (d *PostgresDriver) Lock(ctx context.Context) error {
	_, err := d.db.ExecContext(ctx, "SELECT pg_advisory_lock($1)", AdvisoryLockKey)
	return err
}

// Unlock releases the advisory lock.
func (d *PostgresDriver) Unlock(ctx context.Context) error {
	_, err := d.db.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", AdvisoryLockKey)
	return err
}

// EnsureVersionTable creates the bookkeeping table if absent.
func (d *PostgresDriver) EnsureVersionTable(ctx context.Context) error {
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version int PRIMARY KEY,
		name text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`
	_, err := d.db.ExecContext(ctx, ddl)
	return err
}

// AppliedVersions reads the set of recorded versions.
func (d *PostgresDriver) AppliedVersions(ctx context.Context) (map[int]struct{}, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]struct{}{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = struct{}{}
	}
	return out, rows.Err()
}

// Apply runs the migration body and records it in one transaction, so a failure
// never leaves a half-applied version marked as done.
func (d *PostgresDriver) Apply(ctx context.Context, m Migration) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after a successful commit is a no-op

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("exec body: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.Version, m.Name); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	return tx.Commit()
}
