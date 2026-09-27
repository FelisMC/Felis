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

// PostgresDriver is the production Driver, backed by a database/sql pool using
// the pgx stdlib driver.
type PostgresDriver struct {
	db *sql.DB
	// lock is the one connection holding the migration advisory lock, from Lock
	// to Unlock. The lock belongs to a server session, so it must be released on
	// the connection that took it, never on whichever one the pool hands out.
	lock *sql.Conn
}

// Pool bounds. Every API request that reads the store takes a connection, and
// PostgreSQL's default max_connections of 100 is shared with the reaper and
// backup Jobs, the offsite sync and a break-glass console. Uncapped, a flood of
// public requests or a pile-up behind one lock would open connections until
// those could not get one; capped, the excess waits in database/sql's queue
// until its request context gives up.
const (
	MaxOpenConns    = 25
	maxIdleConns    = 10
	connMaxLifetime = 30 * time.Minute
	connMaxIdleTime = 5 * time.Minute
)

// SessionDefaults are the server-side limits every connection starts with. A
// statement that runs away or waits on a lock is cancelled instead of holding
// its connection forever, and a transaction a stuck caller left open is rolled
// back with its locks. Nothing felis runs per request comes near these; a
// migration lifts the statement limit for its own work (Lock, Apply). A DSN that
// sets either one, as a query parameter or in options=-c, keeps its own value.
var SessionDefaults = map[string]string{
	"statement_timeout":                   "15s",
	"idle_in_transaction_session_timeout": "60s",
}

// connConfig parses dsn and fills in the SessionDefaults it does not set.
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

// newPool builds the bounded pool without dialing.
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

// Open dials dsn and returns a PostgresDriver. The caller owns Close.
func Open(ctx context.Context, dsn string) (*PostgresDriver, error) {
	return OpenRetrying(ctx, dsn, 0, 0, nil)
}

// OpenRetrying is Open for a pod that has only just started. Until window has passed
// it retries, every interval, a ping the server never answered (a refused, reset or
// timed-out dial), reporting each retry to onRetry. k3s admits a new pod's address to
// a NetworkPolicy's allow set a moment after the pod starts: a drill measured about
// 70ms of refused dials, and the reaper, which dials within milliseconds of starting,
// failed every pod of every run on `connection refused` against a healthy database.
// An error the server answered with is final, except 57P03 (cannot_connect_now: it is
// starting up or shutting down).
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

// unansweredDial reports whether a ping failed before the server said anything, or
// with the one answer that means "try again shortly".
func unansweredDial(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "57P03" // cannot_connect_now
	}
	return true
}

// DB exposes the underlying pool for the access layer.
func (d *PostgresDriver) DB() *sql.DB { return d.db }

// Close releases the pool.
func (d *PostgresDriver) Close() error { return d.db.Close() }

// Lock takes the session-level advisory lock that serializes migrations, on a
// connection it keeps out of the pool until Unlock. Waiting behind another
// migrator is not a runaway statement, so that session has no statement limit.
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

// Unlock releases the advisory lock on the connection that took it, then
// closes that connection rather than returning it to the pool, so neither a
// lock that failed to release nor the lifted statement limit outlives the run.
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

// discard closes conn's server session instead of pooling it.
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
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

// AppliedVersions reads the set of recorded versions. A database that has never been
// migrated has no schema_migrations table yet, which is an empty set.
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

// Apply runs the migration body and records it in one transaction, so a failure
// never leaves a half-applied version marked as done.
func (d *PostgresDriver) Apply(ctx context.Context, m Migration) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after a successful commit is a no-op

	// A schema change on a grown table may rightly take longer than any request.
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
