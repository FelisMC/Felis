//go:build pgint

package pgint

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"felis.lolicon.best/internal/store"
)

// The schema guard the api, reaper and offsite copy open the database through: the
// migrated test database passes, a never-migrated one reads as behind (the missing
// schema_migrations table is an empty set, not an error), and one that records a
// version this build does not embed reads as newer.
func TestSchemaGuard(t *testing.T) {
	ctx := context.Background()
	main, err := store.Open(ctx, os.Getenv("FELIS_TEST_PG_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	if err := store.CheckSchema(ctx, main); err != nil {
		t.Fatalf("CheckSchema on the migrated database: %v", err)
	}

	const schema = "pgint_schema_guard"
	if _, err := db.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE; CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	dsn := os.Getenv("FELIS_TEST_PG_URL")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	other, err := store.Open(ctx, dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	if err := store.CheckSchema(ctx, other); !errors.Is(err, store.ErrSchemaBehind) {
		t.Fatalf("CheckSchema on an unmigrated schema = %v, want ErrSchemaBehind", err)
	}

	ms, err := store.LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if err := other.EnsureVersionTable(ctx); err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if _, err := other.DB().ExecContext(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.Version, m.Name); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CheckSchema(ctx, other); err != nil {
		t.Fatalf("CheckSchema with every version recorded: %v", err)
	}
	if _, err := other.DB().ExecContext(ctx, "INSERT INTO schema_migrations (version, name) VALUES (9999, 'from_a_newer_release')"); err != nil {
		t.Fatal(err)
	}
	err = store.CheckSchema(ctx, other)
	if !errors.Is(err, store.ErrSchemaNewer) || !strings.Contains(err.Error(), "9999") {
		t.Fatalf("CheckSchema with a newer version recorded = %v, want ErrSchemaNewer naming 9999", err)
	}
	if _, err := store.Up(ctx, other, ms); !errors.Is(err, store.ErrSchemaNewer) {
		t.Fatalf("Up on a newer schema = %v, want ErrSchemaNewer", err)
	}
}
