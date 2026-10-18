package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

type PostgresDriver struct {
	db *sql.DB
	// Advisory locks belong to the session, so Unlock must use this connection.
	lock *sql.Conn
}

// Bound the pool to leave connections available for background jobs and administration.
const (
	MaxOpenConns    = 25
	maxIdleConns    = 10
	connMaxLifetime = 30 * time.Minute
	connMaxIdleTime = 5 * time.Minute
)

// SessionDefaults bound statement and transaction lifetimes unless overridden by the DSN.
var SessionDefaults = map[string]string{
	"statement_timeout":                   "15s",
	"idle_in_transaction_session_timeout": "60s",
}

func connConfig(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	for k, v := range SessionDefaults {
		if _, set := cfg.RuntimeParams[k]; set || strings.Contains(cfg.RuntimeParams["options"], k) {
			continue
		}
		cfg.RuntimeParams[k] = v
	}
	return cfg, nil
}

func newPool(dsn string) (*sql.DB, error) {
	cfg, err := connConfig(dsn)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(MaxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)
	return db, nil
}

// Open connects to dsn; the caller must close the driver.
func Open(ctx context.Context, dsn string) (*PostgresDriver, error) {
	return OpenRetrying(ctx, dsn, 0, 0, nil)
}

// OpenRetrying retries unanswered dials and PostgreSQL 57P03 until window expires.
func OpenRetrying(ctx context.Context, dsn string, window, interval time.Duration, onRetry func(error)) (*PostgresDriver, error) {
	db, err := newPool(dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	deadline := time.Now().Add(window)
	for {
		err := db.PingContext(ctx)
		if err == nil {
			return &PostgresDriver{db: db}, nil
		}
		if !unansweredDial(err) || !time.Now().Before(deadline) || ctx.Err() != nil {
			db.Close()
			return nil, fmt.Errorf("ping postgres: %w", err)
		}
		onRetry(err)
		select {
		case <-ctx.Done():
			db.Close()
			return nil, fmt.Errorf("ping postgres: %w", err)
		case <-time.After(interval):
		}
	}
}

func unansweredDial(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "57P03" // cannot_connect_now
	}
	return true
}

func (d *PostgresDriver) DB() *sql.DB { return d.db }

func (d *PostgresDriver) Close() error { return d.db.Close() }

// Lock reserves a connection and disables its statement timeout while waiting for the advisory lock.
func (d *PostgresDriver) Lock(ctx context.Context) error {
	if d.lock != nil {
		return errors.New("migration lock already held")
	}
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "SET statement_timeout = 0"); err != nil {
		discard(conn)
		return err
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", AdvisoryLockKey); err != nil {
		discard(conn)
		return err
	}
	d.lock = conn
	return nil
}

// Unlock discards the session so unreleased locks and lifted timeouts cannot return to the pool.
func (d *PostgresDriver) Unlock(ctx context.Context) error {
	conn := d.lock
	if conn == nil {
		return errors.New("migration lock not held")
	}
	d.lock = nil
	defer discard(conn)
	var released bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", AdvisoryLockKey).Scan(&released); err != nil {
		return err
	}
	if !released {
		return errors.New("migration lock was not held by its connection")
	}
	return nil
}

// discard closes the server session instead of pooling it.
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

func (d *PostgresDriver) EnsureVersionTable(ctx context.Context) error {
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version int PRIMARY KEY,
		name text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`
	_, err := d.db.ExecContext(ctx, ddl)
	return err
}

// AppliedVersions returns an empty set if schema_migrations does not exist.
func (d *PostgresDriver) AppliedVersions(ctx context.Context) (map[int]struct{}, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
			return map[int]struct{}{}, nil
		}
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

func (d *PostgresDriver) Apply(ctx context.Context, m Migration) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after a successful commit is a no-op

	// Migrations may exceed the request statement timeout.
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return fmt.Errorf("lift statement timeout: %w", err)
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("exec body: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.Version, m.Name); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	return tx.Commit()
}
