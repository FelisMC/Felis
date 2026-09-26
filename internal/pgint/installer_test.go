//go:build pgint

package pgint

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The row-count query deploy/bootstrap.sh runs on the host PostgreSQL and on felis-postgres
// when it moves the database into k3s, comparing the two answers byte for byte: every public
// table with its exact row count, one per line, in byte order of the table names. The shell
// tests stub psql, so this is the only place the SQL itself runs before a real host does.
func TestInstallerTableCounts(t *testing.T) {
	// bash reads the assignment the way the installer does, quoting and escapes included.
	out, err := exec.Command("bash", "-c", `eval "$(grep '^PG_TABLE_COUNTS=' "$1")" && printf %s "$PG_TABLE_COUNTS"`,
		"bash", "../../deploy/bootstrap.sh").Output()
	if err != nil || len(out) == 0 {
		t.Fatalf("reading PG_TABLE_COUNTS from deploy/bootstrap.sh: %v (%q)", err, out)
	}
	ctx := context.Background()
	// Tables the schema does not have yet but a migration may add: a name needing quotes,
	// which byte order puts before every lowercase one and a locale's order does not, and a
	// partitioned table.
	for _, stmt := range []string{
		`CREATE TABLE public."Pgint_Counts" (id int)`,
		`INSERT INTO public."Pgint_Counts" VALUES (1), (2), (3)`,
		`CREATE TABLE public.pgint_counts_parted (id int) PARTITION BY RANGE (id)`,
		`CREATE TABLE public.pgint_counts_parted_all PARTITION OF public.pgint_counts_parted FOR VALUES FROM (0) TO (100)`,
		`INSERT INTO public.pgint_counts_parted VALUES (1), (2)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS public."Pgint_Counts", public.pgint_counts_parted`)
	})
	// One snapshot for the query and the counts it is checked against.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, string(out))
	if err != nil {
		t.Fatalf("PG_TABLE_COUNTS: %v", err)
	}
	var got []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		got = append(got, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("PG_TABLE_COUNTS: %v", err)
	}

	names, err := publicTables(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names) // byte order, what COLLATE "C" sorts by
	var want []string
	for _, name := range names {
		var n int64
		q := `SELECT count(*) FROM public."` + strings.ReplaceAll(name, `"`, `""`) + `"`
		if err := tx.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		want = append(want, fmt.Sprintf("%s %d", name, n))
	}
	if !slices.Contains(want, "Pgint_Counts 3") || !slices.Contains(want, "pgint_counts_parted 2") {
		t.Fatalf("the tables this test made are not what it counts: %q", want)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("PG_TABLE_COUNTS returned\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func publicTables(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT tablename FROM pg_tables WHERE schemaname = 'public'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
