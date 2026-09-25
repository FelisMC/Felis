//go:build pgint

package pgint

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"felis.lolicon.best/internal/store"

	"github.com/jackc/pgx/v5/pgconn"
)

// Every pooled connection starts with the server-side limits, so a runaway
// statement or an abandoned transaction cannot hold a connection forever.
func TestPoolSessionsCarryTheTimeouts(t *testing.T) {
	ctx := context.Background()
	for setting, want := range map[string]string{
		"statement_timeout":                   "15s",
		"idle_in_transaction_session_timeout": "1min",
	} {
		var got string
		if err := db.QueryRowContext(ctx, "SHOW "+setting).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", setting, got, want)
		}
	}
}

// The limit is enforced by the server: a DSN that tightens it gets its
// statements cancelled at that bound.
func TestStatementTimeoutCancelsARunawayStatement(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("FELIS_TEST_PG_URL")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	drv, err := store.Open(ctx, dsn+sep+"statement_timeout=200")
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close()
	_, err = drv.DB().ExecContext(ctx, "SELECT pg_sleep(2)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" { // query_canceled
		t.Fatalf("pg_sleep(2) under a 200ms limit: err = %v, want query_canceled", err)
	}
}

// Unlock releases the lock on the connection that took it, even when the pool
// would hand that connection to someone else: another migrator can take it next.
func TestMigrationLockIsReleasedOnItsOwnConnection(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("FELIS_TEST_PG_URL")
	drv, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close()
	other, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	if err := drv.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	// Hold the pool's idle connection, so a release that went through the pool
	// would land on a different session from the lock's.
	busy, err := drv.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	var held bool
	if err := other.DB().QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", store.AdvisoryLockKey).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("a second migrator took the lock while the first held it")
	}

	if err := drv.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	conn, err := other.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", store.AdvisoryLockKey).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("the lock is still held after Unlock")
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", store.AdvisoryLockKey); err != nil {
		t.Fatal(err)
	}
}

// A migration body runs without the request-sized statement limit.
func TestMigrationBodyHasNoStatementLimit(t *testing.T) {
	ctx := context.Background()
	drv, err := store.Open(ctx, os.Getenv("FELIS_TEST_PG_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close()
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS timeout_probe")
		_, _ = db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = 999999")
	})

	probe := store.Migration{Version: 999999, Name: "timeout_probe",
		SQL: "CREATE TABLE timeout_probe AS SELECT current_setting('statement_timeout') AS v"}
	if err := drv.Apply(ctx, probe); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := db.QueryRowContext(ctx, "SELECT v FROM timeout_probe").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != "0" {
		t.Fatalf("statement_timeout inside a migration = %q, want 0", v)
	}
}
