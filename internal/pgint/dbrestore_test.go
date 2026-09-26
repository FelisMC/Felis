//go:build pgint

package pgint

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/store"
)

// ---- felis db backup / restore against the real tools and server ----------------
//
// internal/dbbackup's suite fakes pg_dump, pg_restore and psql with scripts that
// encode what the server is assumed to do: DROP OWNED clears the role's objects,
// and a transaction psql leaves open at EOF is rolled back. These tests run the
// real tools against a real server, as the role production uses (a plain LOGIN
// role owning its database; the superuser is someone else), on a database of
// their own so the rest of the suite is untouched.
//
// FELIS_TEST_PG_EXEC, when set, is the argv prefix the tools run under, the way
// production runs them under `k3s kubectl exec ... --`: CI sets
// `docker exec -i <service container>`, which also keeps the tools at the
// server's major version. Unset, the tools come from PATH and connect over the
// URL, so they must match the server.

const (
	restoreRole = "felis_restore_pgint"
	restoreDB   = "felis_pgint_restore"
)

type restoreRig struct {
	url   string // the role on its database, as felis.toml carries it
	suURL string // the suite's superuser on the same database
	tools dbbackup.Tools
	dir   string // bundle directory
}

func newRestoreRig(t *testing.T) *restoreRig {
	t.Helper()
	ctx := context.Background()
	base, err := url.Parse(os.Getenv("FELIS_TEST_PG_URL"))
	if err != nil {
		t.Fatal(err)
	}
	dropAll := func() {
		for _, stmt := range []string{
			"DROP DATABASE IF EXISTS " + restoreDB + " WITH (FORCE)",
			"DROP ROLE IF EXISTS " + restoreRole,
		} {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}
	dropAll()
	mustExec(t, "CREATE ROLE "+restoreRole+" LOGIN PASSWORD 'pgint'")
	mustExec(t, "CREATE DATABASE "+restoreDB+" OWNER "+restoreRole)
	t.Cleanup(dropAll)

	role, su := *base, *base
	role.User = url.UserPassword(restoreRole, "pgint")
	role.Path, su.Path = "/"+restoreDB, "/"+restoreDB
	r := &restoreRig{url: role.String(), suURL: su.String(), dir: t.TempDir()}
	if pre := strings.Fields(os.Getenv("FELIS_TEST_PG_EXEC")); len(pre) > 0 {
		r.tools = dbbackup.Tools{Exec: pre, Conn: "host=/var/run/postgresql dbname=" + restoreDB + " user=" + restoreRole}
	}

	// The schema `felis migrate up` produces, owned by the role.
	r.with(t, r.url, func(rdb *store.PostgresDriver) {
		ms, err := store.LoadMigrations()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Up(ctx, rdb, ms); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	})
	return r
}

// with runs fn on a pool of its own and closes it before returning: a restore
// refuses to run while any other client is connected.
func (r *restoreRig) with(t *testing.T, dsn string, fn func(*store.PostgresDriver)) {
	t.Helper()
	drv, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	fn(drv)
	drv.Close()
	r.waitIdle(t)
}

// waitIdle waits for the database's last session to end: the server ends a
// backend just after its client leaves.
func (r *restoreRig) waitIdle(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND backend_type = 'client backend'`, restoreDB).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions still on %s", n, restoreDB)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *restoreRig) exec(t *testing.T, dsn string, stmts ...string) {
	t.Helper()
	r.with(t, dsn, func(d *store.PostgresDriver) {
		for _, s := range stmts {
			if _, err := d.DB().Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	})
}

// seed fills the database the way a running install does, plus a table of
// incompressible rows sorted last, so the dump is mostly data.
func (r *restoreRig) seed(t *testing.T) {
	t.Helper()
	r.with(t, r.url, func(d *store.PostgresDriver) {
		rp := api.NewPGRepo(d.DB())
		for _, name := range []string{"alice", "bob"} {
			u, err := rp.CreateUser(context.Background(), api.CreateUserInput{Username: name, Role: "user"}, "pgint")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.DB().Exec(`INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb, owner_id)
				VALUES ($1, 500, 1024, 2048, $2)`, name+"-smp", u.ID); err != nil {
				t.Fatal(err)
			}
		}
		for _, s := range []string{
			`CREATE TABLE zz_pgint_bulk (id int PRIMARY KEY, pad text NOT NULL)`,
			`INSERT INTO zz_pgint_bulk SELECT g, md5(random()::text) || md5(random()::text) FROM generate_series(1, 20000) g`,
		} {
			if _, err := d.DB().Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	})
}

// fingerprint is every table's rows and every sequence's position, read as the
// superuser so objects of any owner count. The freshness record a restore
// rewrites is left out.
func (r *restoreRig) fingerprint(t *testing.T) map[string]string {
	t.Helper()
	fp := map[string]string{}
	r.with(t, r.suURL, func(d *store.PostgresDriver) {
		rows, err := d.DB().Query(`SELECT tablename, tableowner FROM pg_tables WHERE schemaname = 'public' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		var tables, owners []string
		for rows.Next() {
			var name, owner string
			if err := rows.Scan(&name, &owner); err != nil {
				t.Fatal(err)
			}
			tables, owners = append(tables, name), append(owners, owner)
		}
		rows.Close()
		for i, tbl := range tables {
			where := ""
			if tbl == "platform_settings" {
				where = " WHERE key <> '" + dbbackup.StatusKey + "'"
			}
			var v string
			q := fmt.Sprintf(`SELECT count(*)::text || ' ' || coalesce(md5(string_agg(t::text, E'\n' ORDER BY t::text)), '') FROM public.%q t%s`, tbl, where)
			if err := d.DB().QueryRow(q).Scan(&v); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			fp["table "+tbl] = owners[i] + " " + v
		}
		srows, err := d.DB().Query(`SELECT sequencename, coalesce(last_value::text, '-') FROM pg_sequences WHERE schemaname = 'public'`)
		if err != nil {
			t.Fatal(err)
		}
		for srows.Next() {
			var name, v string
			if err := srows.Scan(&name, &v); err != nil {
				t.Fatal(err)
			}
			fp["sequence "+name] = v
		}
		srows.Close()
	})
	return fp
}

// diff names what differs between two fingerprints.
func diff(a, b map[string]string) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(a)) {
		if a[k] != b[k] {
			out = append(out, fmt.Sprintf("%s: %q vs %q", k, a[k], b[k]))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(b)) {
		if _, ok := a[k]; !ok {
			out = append(out, fmt.Sprintf("%s: only in the second (%q)", k, b[k]))
		}
	}
	return out
}

// change makes every kind of difference a restore must undo: rows removed,
// added and edited, a sequence advanced, a table dropped, one created.
func (r *restoreRig) change(t *testing.T) {
	t.Helper()
	r.exec(t, r.url,
		`DELETE FROM servers WHERE name = 'bob-smp'`,
		`UPDATE servers SET cached_memory_mb = 4096 WHERE name = 'alice-smp'`,
		`INSERT INTO platform_settings (key, value) VALUES ('pgint_after', '{"x": 1}')`,
		`DROP TABLE zz_pgint_bulk`,
		`CREATE TABLE pgint_after (id serial PRIMARY KEY)`,
		`INSERT INTO pgint_after DEFAULT VALUES`,
	)
}

func (r *restoreRig) backup(t *testing.T) string {
	t.Helper()
	bundle, err := dbbackup.Backup(context.Background(), dbbackup.BackupOptions{
		DatabaseURL: r.url, Dir: r.dir, Label: dbbackup.LabelManual, Tools: r.tools})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	return bundle
}

func (r *restoreRig) restore(bundle string, mut func(*dbbackup.RestoreOptions)) (string, error) {
	o := dbbackup.RestoreOptions{DatabaseURL: r.url, Bundle: bundle, Dir: r.dir, Tools: r.tools}
	if mut != nil {
		mut(&o)
	}
	_, safety, err := dbbackup.Restore(context.Background(), o)
	return safety, err
}

// A bundle taken of a live schema restores it exactly, over rows and objects
// changed since, refuses to while a client is connected, and leaves a safety
// bundle that undoes the restore.
func TestDBRestoreRoundTrip(t *testing.T) {
	r := newRestoreRig(t)
	r.seed(t)
	want := r.fingerprint(t)
	bundle := r.backup(t)
	m, err := dbbackup.Verify(bundle)
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := store.LoadMigrations()
	if m.SchemaVersion != ms[len(ms)-1].Version || !strings.HasPrefix(m.PGDumpVersion, "pg_dump (PostgreSQL) ") {
		t.Errorf("manifest schema %d pg_dump %q, want schema %d and pg_dump's version", m.SchemaVersion, m.PGDumpVersion, ms[len(ms)-1].Version)
	}

	r.change(t)
	changed := r.fingerprint(t)
	if len(diff(want, changed)) == 0 {
		t.Fatal("the change changed nothing")
	}

	// A client on the database: refused before anything is taken or replayed.
	drv, err := store.Open(context.Background(), r.url)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.restore(bundle, nil)
	drv.Close()
	if !errors.Is(err, dbbackup.ErrClientsConnected) {
		t.Fatalf("restore under a connected client: %v, want ErrClientsConnected", err)
	}
	if d := diff(changed, r.fingerprint(t)); len(d) > 0 {
		t.Fatalf("a refused restore changed the database: %v", d)
	}
	if all, _ := dbbackup.List(r.dir); len(all) != 1 {
		t.Fatalf("bundles after the refusal = %v, want only the one taken", all)
	}
	r.waitIdle(t)

	safety, err := r.restore(bundle, nil)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if d := diff(want, r.fingerprint(t)); len(d) > 0 {
		t.Fatalf("restored database differs from the bundle's: %v", d)
	}
	// The dump's own freshness record predates its bundle; the restore points
	// the panel at the newest bundle on disk, the safety one.
	r.with(t, r.suURL, func(d *store.PostgresDriver) {
		var raw []byte
		if err := d.DB().QueryRow(`SELECT value FROM platform_settings WHERE key = $1`, dbbackup.StatusKey).Scan(&raw); err != nil {
			t.Fatalf("freshness record: %v", err)
		}
		var st dbbackup.Status
		if err := json.Unmarshal(raw, &st); err != nil || st.Name != filepath.Base(safety) || st.Label != dbbackup.LabelPreRestore {
			t.Errorf("freshness record = %s (%v), want %s", raw, err, filepath.Base(safety))
		}
	})

	if _, err := r.restore(safety, func(o *dbbackup.RestoreOptions) { o.SkipSafetyBackup = true }); err != nil {
		t.Fatalf("restore the safety bundle: %v", err)
	}
	if d := diff(changed, r.fingerprint(t)); len(d) > 0 {
		t.Fatalf("the safety bundle did not bring back what the restore replaced: %v", d)
	}
}

// A replay that fails part way, in the server or in pg_restore, leaves the
// database as it was: DROP OWNED and every statement after it roll back.
func TestDBRestoreFailureRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		// prepare returns the bundle to restore, after the database changed.
		prepare func(t *testing.T, r *restoreRig, bundle string) string
		want    string
	}{
		{"the server refuses a statement", func(t *testing.T, r *restoreRig, bundle string) string {
			// Another role's table where the dump creates the role's own: DROP
			// OWNED leaves it, and the dump's CREATE TABLE fails after every
			// type, function and earlier table was already made again.
			r.exec(t, r.suURL, `CREATE TABLE zz_pgint_bulk (id int)`)
			return bundle
		}, `ERROR:  relation "zz_pgint_bulk" already exists`},
		{"the dump ends early", func(t *testing.T, r *restoreRig, bundle string) string {
			return truncatedDump(t, bundle)
		}, "pg_restore: "},
		{"pg_restore dies between statements", func(t *testing.T, r *restoreRig, bundle string) string {
			// Cut in the data, psql is inside a COPY and fails the stream itself.
			// Killed (a timeout, the OOM killer) while psql works through the
			// indexes, pg_restore leaves whole statements behind, and only the
			// COMMIT it never earned keeps them out.
			r.tools = killedBeforeIndexes(t, r)
			return bundle
		}, "exit status 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRestoreRig(t)
			r.seed(t)
			bundle := r.backup(t)
			r.change(t)
			bundle = tc.prepare(t, r, bundle)
			before := r.fingerprint(t)

			_, err := r.restore(bundle, func(o *dbbackup.RestoreOptions) { o.SkipSafetyBackup = true })
			if err == nil || !strings.Contains(err.Error(), "rolled back, the database is unchanged") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("restore: %v, want a rolled back replay and %q", err, tc.want)
			}
			if d := diff(before, r.fingerprint(t)); len(d) > 0 {
				t.Fatalf("the failed replay changed the database: %v", d)
			}
		})
	}
}

// killedBeforeIndexes wraps the tools so the replay's pg_restore streams its
// script up to the first CREATE INDEX and then exits 1. Everything else is the
// real tools and server.
func killedBeforeIndexes(t *testing.T, r *restoreRig) dbbackup.Tools {
	t.Helper()
	wrapper := filepath.Join(t.TempDir(), "killed")
	if err := os.WriteFile(wrapper, []byte(`#!/bin/sh
case " $* " in
*" pg_restore "*" --file=- "*) "$@" | sed '/^CREATE INDEX/,$d'; exit 1 ;;
esac
exec "$@"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	tools := r.tools
	if len(tools.Exec) == 0 {
		// Under an exec prefix the tools connect as Conn.
		tools.Conn = r.url
	}
	tools.Exec = append([]string{wrapper}, tools.Exec...)
	return tools
}

// truncatedDump copies bundle with db.dump cut to three quarters, inside the
// rows of the last table, and a manifest that vouches for the cut dump: the
// bundle verifies and pg_restore reads its table of contents, then dies part
// way through the data.
func truncatedDump(t *testing.T, bundle string) string {
	t.Helper()
	f, err := os.Open(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	type entry struct {
		hdr  *tar.Header
		data []byte
	}
	var entries []entry
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry{h, data})
	}
	var m dbbackup.Manifest
	if entries[0].hdr.Name != "MANIFEST.json" || json.Unmarshal(entries[0].data, &m) != nil {
		t.Fatalf("%s does not start with its manifest", bundle)
	}
	for i := range entries {
		if entries[i].hdr.Name != "db.dump" {
			continue
		}
		entries[i].data = entries[i].data[:len(entries[i].data)*3/4]
		sum := sha256.Sum256(entries[i].data)
		for j := range m.Files {
			if m.Files[j].Name == "db.dump" {
				m.Files[j].Size, m.Files[j].SHA256 = int64(len(entries[i].data)), hex.EncodeToString(sum[:])
			}
		}
	}
	if entries[0].data, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		e.hdr.Size = int64(len(e.data))
		if err := tw.WriteHeader(e.hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), filepath.Base(bundle))
	if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := dbbackup.Verify(out); err != nil {
		t.Fatalf("the cut bundle does not verify: %v", err)
	}
	return out
}
