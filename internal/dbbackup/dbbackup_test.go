package dbbackup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// The PostgreSQL client tools are faked with shell scripts over a one-file
// "database" ($FAKE_DIR/db). The fake psql only writes the replayed rows back
// when its input ends in COMMIT, which is how a real server treats an open
// transaction at disconnect, so rollback-on-failure is observable.

const fakePGDump = `#!/bin/sh
D="$FAKE_DIR"
if [ "$1" = "--version" ]; then echo "pg_dump (PostgreSQL) 13.23"; exit 0; fi
printf '%s\n' "$*" > "$D/pg_dump.args"
printf '%s' "$PGPASSWORD" > "$D/pg_dump.password"
[ -f "$D/dump_fail" ] && { echo "pg_dump: error: connection refused" >&2; exit 1; }
[ -f "$D/dump_denied" ] && { printf 'pg_dump: error: query failed: ERROR:  permission denied for table servers_preserve\npg_dump: error: query was: LOCK TABLE public.servers_preserve IN ACCESS SHARE MODE\n' >&2; exit 1; }
for a in "$@"; do case "$a" in --file=*|-f) echo "pg_dump: the archive must come back on stdout" >&2; exit 2;; esac; done
if [ -f "$D/dump_garbage" ]; then echo garbage; exit 0; fi
printf 'PGDMP\n'; cat "$D/db"
`

const fakePGRestore = `#!/bin/sh
D="$FAKE_DIR"
list=0
for a in "$@"; do case "$a" in --list) list=1;; -*) ;; *) echo "pg_restore: the archive must arrive on stdin, got $a" >&2; exit 2;; esac; done
in="$D/restore.in.$$"
cat > "$in"
head -n 1 "$in" | grep -q '^PGDMP$' || { rm -f "$in"; echo "pg_restore: error: input file does not appear to be a valid archive" >&2; exit 1; }
[ $list = 1 ] && { rm -f "$in"; echo "; Archive created"; exit 0; }
echo "-- restore script"
tail -n +2 "$in" | sed 's/^/DATA /'
rm -f "$in"
[ -f "$D/restore_fail" ] && { echo "pg_restore: error: could not read input" >&2; exit 1; }
exit 0
`

const fakePSQL = `#!/bin/sh
D="$FAKE_DIR"
printf '%s\n' "$@" > "$D/psql.args"
q=""
while [ $# -gt 0 ]; do case "$1" in -c) q="$2"; shift;; esac; shift; done
if [ -n "$q" ]; then
  case "$q" in
    *schema_migrations*) echo 21;;
    *pg_stat_activity*) cat "$D/clients" 2>/dev/null || echo 0;;
    *"FROM users"*) cat "$D/counts" 2>/dev/null || echo "3|2";;
  esac
  exit 0
fi
cat > "$D/psql.in"
# The freshness record is its own psql call; keep it apart from the replay.
if grep -q platform_settings "$D/psql.in"; then
  mv "$D/psql.in" "$D/record.stdin"; cp "$D/psql.args" "$D/record.args"; exit 0
fi
mv "$D/psql.in" "$D/psql.stdin"
[ -f "$D/psql_fail" ] && { echo 'ERROR:  relation "x" already exists' >&2; exit 3; }
tail -n 1 "$D/psql.stdin" | grep -q '^COMMIT;$' || exit 0
grep '^DATA ' "$D/psql.stdin" | sed 's/^DATA //' > "$D/db"
`

// fakeExec stands in for kubectl exec: the argv up to -- is its own, and none
// of the caller's environment crosses into the "container".
const fakeExec = `#!/bin/sh
D="$FAKE_DIR"
pre=""
while [ $# -gt 0 ] && [ "$1" != "--" ]; do pre="$pre $1"; shift; done
shift
printf '%s\n' "$pre" >> "$D/exec.args"
exec env -i FAKE_DIR="$D" PATH="$PATH" "$@"
`

const testURL = "postgres://felis:s3cret-pw@127.0.0.1:5432/felis?sslmode=disable"

// podConn is the in-container connection the Exec tests hand the tools.
const podConn = "host=/var/run/postgresql port=5432 dbname=felis user=felis"

type fakePG struct {
	dir   string
	tools Tools
}

func newFakePG(t *testing.T, db string) *fakePG {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f := &fakePG{dir: dir, tools: Tools{
		PGDump: write("pg_dump", fakePGDump), PGRestore: write("pg_restore", fakePGRestore), PSQL: write("psql", fakePSQL),
	}}
	f.setDB(t, db)
	t.Setenv("FAKE_DIR", dir)
	return f
}

// newFakePod is newFakePG with the tools behind a fake kubectl exec.
func newFakePod(t *testing.T, db string) *fakePG {
	t.Helper()
	f := newFakePG(t, db)
	ex := filepath.Join(f.dir, "kubectl")
	if err := os.WriteFile(ex, []byte(fakeExec), 0o755); err != nil {
		t.Fatal(err)
	}
	f.tools.Exec = []string{ex, "exec", "-i", "-n", "felis", "deploy/felis-postgres", "-c", "postgres", "--"}
	f.tools.Conn = podConn
	return f
}

// execRuns is how many tool runs went through the fake kubectl exec.
func (f *fakePG) execRuns(t *testing.T) int {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(f.dir, "exec.args"))
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "deploy/felis-postgres -c postgres") {
			n++
		}
	}
	return n
}

func (f *fakePG) setDB(t *testing.T, s string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "db"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fakePG) db(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f *fakePG) flag(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stateDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for name, body := range map[string]string{
		"secrets.env":     "DB_PASSWORD=s3cret-pw\n",
		"felis.host.toml": "[database]\n",
		"bootstrap.done":  "2026-09-24T00:00:00Z\n",
	} {
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(d, "felis.host.toml"), filepath.Join(d, "felis.toml")); err != nil {
		t.Fatal(err)
	}
	return d
}

var t0 = time.Date(2026, 9, 24, 3, 30, 0, 0, time.UTC)

// recorded is the Status of the last freshness record, or the zero Status.
func (pg *fakePG) recorded(t *testing.T) Status {
	t.Helper()
	args, _ := os.ReadFile(filepath.Join(pg.dir, "record.args"))
	var st Status
	for _, a := range strings.Split(string(args), "\n") {
		if v, ok := strings.CutPrefix(a, "v="); ok {
			if err := json.Unmarshal([]byte(v), &st); err != nil {
				t.Fatal(err)
			}
		}
	}
	return st
}

func at(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestBackupWritesAVerifiableBundle(t *testing.T) {
	pg := newFakePG(t, "users: alice\n")
	dir := filepath.Join(t.TempDir(), "db-backups")
	state := stateDir(t)
	path, err := Backup(context.Background(), BackupOptions{
		DatabaseURL: testURL, Dir: dir, Label: LabelDaily, StateDir: state, Version: "v1.2.3",
		Tools: pg.tools, Now: at(t0),
		ExportServers: func(context.Context) ([]byte, error) { return []byte(`{"kind":"List","items":[]}`), nil },
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if want := filepath.Join(dir, "felis-db-20260924T033000Z-daily.tar"); path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
	for p, mode := range map[string]os.FileMode{dir: 0o700, path: 0o600, path + ".sha256": 0o600} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s mode = %v, want %v", p, info.Mode().Perm(), mode)
		}
	}

	// The password reaches pg_dump through the environment, never argv.
	args, _ := os.ReadFile(filepath.Join(pg.dir, "pg_dump.args"))
	if strings.Contains(string(args), "s3cret-pw") {
		t.Errorf("password on the pg_dump command line: %s", args)
	}
	if pw, _ := os.ReadFile(filepath.Join(pg.dir, "pg_dump.password")); string(pw) != "s3cret-pw" {
		t.Errorf("PGPASSWORD = %q", pw)
	}

	m, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if m.Label != LabelDaily || m.FelisVersion != "v1.2.3" || m.SchemaVersion != 21 || !m.CreatedAt.Equal(t0) {
		t.Errorf("manifest = %+v", m)
	}
	if m.Counts == nil || *m.Counts != (Counts{Users: 3, Servers: 2}) {
		t.Errorf("counts = %v, want the 3 accounts and 2 servers psql answered", m.Counts)
	}
	if m.Database != (DatabaseInfo{Host: "127.0.0.1", Port: "5432", Name: "felis", User: "felis"}) {
		t.Errorf("database = %+v", m.Database)
	}
	if !strings.Contains(m.PGDumpVersion, "13.23") {
		t.Errorf("pg_dump version = %q", m.PGDumpVersion)
	}
	var names []string
	for _, f := range m.Files {
		names = append(names, f.Name)
	}
	abs, _ := filepath.Abs(state)
	prefix := "state" + filepath.ToSlash(abs) + "/"
	want := []string{"db.dump", prefix + "felis.host.toml", prefix + "felis.toml", prefix + "secrets.env", "k8s/minecraftservers.json"}
	if !slices.Equal(names, want) {
		t.Errorf("members = %v, want %v (bootstrap.done left out)", names, want)
	}
	for _, f := range m.Files {
		if f.Name == prefix+"felis.toml" && f.Link != filepath.Join(state, "felis.host.toml") {
			t.Errorf("symlink recorded as %+v", f)
		}
	}

	// No scratch or partial files survive a successful run.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".partial") || strings.HasSuffix(e.Name(), ".dump") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestBackupRecordsFreshness(t *testing.T) {
	pg := newFakePG(t, "x\n")
	dir := t.TempDir()
	metrics := filepath.Join(t.TempDir(), "textfile", "felis_db_backup.prom")
	path, err := Backup(context.Background(), BackupOptions{
		DatabaseURL: testURL, Dir: dir, Label: LabelDaily, Version: "v9", Tools: pg.tools, Now: at(t0),
		Record: true, MetricsFile: metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	stdin, _ := os.ReadFile(filepath.Join(pg.dir, "record.stdin"))
	if !strings.Contains(string(stdin), "INSERT INTO platform_settings") || !strings.Contains(string(stdin), ":'v'::jsonb") {
		t.Fatalf("record SQL = %q", stdin)
	}
	st := pg.recorded(t)
	info, _ := os.Stat(path)
	if st.Name != filepath.Base(path) || st.Label != LabelDaily || !st.At.Equal(t0) || st.SizeBytes != info.Size() || st.SchemaVersion != 21 {
		t.Fatalf("recorded %+v", st)
	}

	prom, err := os.ReadFile(metrics)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("felis_db_backup_last_success_timestamp_seconds{label=\"daily\"} %d\n", t0.Unix())
	if !strings.Contains(string(prom), want) {
		t.Fatalf("metrics = %s, want %s", prom, want)
	}
	if fi, _ := os.Stat(metrics); fi.Mode().Perm() != 0o644 {
		t.Errorf("metrics mode %v", fi.Mode().Perm())
	}
}

func TestBackupRecordsAClusterThatDidNotAnswer(t *testing.T) {
	pg := newFakePG(t, "x\n")
	dir := t.TempDir()
	path, err := Backup(context.Background(), BackupOptions{
		DatabaseURL: testURL, Dir: dir, Label: LabelDaily, Tools: pg.tools, Now: at(t0),
		ExportServers: func(context.Context) ([]byte, error) { return nil, errors.New("connection refused") },
	})
	if err != nil {
		t.Fatalf("a cluster outage must not fail the database backup: %v", err)
	}
	m, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.ServersError != "connection refused" || len(m.Files) != 1 {
		t.Errorf("manifest = %+v", m)
	}
}

func TestBackupCounts(t *testing.T) {
	for _, tc := range []struct {
		reply string
		want  *Counts
		fresh bool
	}{
		{"0|0", &Counts{}, true},
		{"1|0", &Counts{Users: 1}, true},
		{"2|0", &Counts{Users: 2}, false},
		{"1|1", &Counts{Users: 1, Servers: 1}, false},
		{"", nil, false},    // the query failed: psql printed nothing
		{"17", nil, false},  // not the two columns asked for
		{"x|2", nil, false}, // not numbers
	} {
		pg := newFakePG(t, "x\n")
		if err := os.WriteFile(filepath.Join(pg.dir, "counts"), []byte(tc.reply+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path, err := Backup(context.Background(), BackupOptions{DatabaseURL: testURL, Dir: t.TempDir(), Label: LabelManual, Tools: pg.tools, Now: at(t0)})
		if err != nil {
			t.Fatal(err)
		}
		m, err := Verify(path)
		if err != nil {
			t.Fatal(err)
		}
		if (m.Counts == nil) != (tc.want == nil) || (m.Counts != nil && *m.Counts != *tc.want) {
			t.Errorf("reply %q: counts = %v, want %v", tc.reply, m.Counts, tc.want)
		}
		if m.Counts.Fresh() != tc.fresh {
			t.Errorf("reply %q: Fresh = %v, want %v", tc.reply, m.Counts.Fresh(), tc.fresh)
		}
	}
	if got := (*Counts)(nil).String(); got != "not recorded" {
		t.Errorf("nil counts print %q", got)
	}
	if got := (&Counts{Users: 3, Servers: 2}).String(); got != "3 accounts, 2 servers" {
		t.Errorf("counts print %q", got)
	}
	if got := (&Counts{Users: 1, Servers: 1}).String(); got != "1 account, 1 server" {
		t.Errorf("counts print %q", got)
	}
}

func TestReadManifestStopsAtTheManifest(t *testing.T) {
	pg := newFakePG(t, strings.Repeat("row\n", 64))
	path, err := Backup(context.Background(), BackupOptions{DatabaseURL: testURL, Dir: t.TempDir(), Label: LabelDaily, Version: "v1.2.3", Tools: pg.tools, Now: at(t0)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Everything after the manifest is withheld: the reader fails past it.
	cut := strings.Index(string(raw), "PGDMP")
	cutErr := errors.New("the rest has not arrived")
	m, err := ReadManifest(io.MultiReader(bytes.NewReader(raw[:cut]), iotest.ErrReader(cutErr)))
	if err != nil {
		t.Fatalf("ReadManifest read past the manifest: %v", err)
	}
	if m.Label != LabelDaily || m.FelisVersion != "v1.2.3" || !m.CreatedAt.Equal(t0) || m.Counts == nil || m.Counts.Users != 3 {
		t.Errorf("manifest = %+v", m)
	}

	// A stream that fails before the manifest is whole says why.
	if _, err := ReadManifest(io.MultiReader(bytes.NewReader(raw[:100]), iotest.ErrReader(cutErr))); !errors.Is(err, cutErr) {
		t.Errorf("stream failing in the header: err = %v, want %v", err, cutErr)
	}
	for what, r := range map[string]io.Reader{
		"empty":   bytes.NewReader(nil),
		"not tar": strings.NewReader(strings.Repeat("junk", 200)),
		"cut tar": bytes.NewReader(raw[:100]),
	} {
		if _, err := ReadManifest(r); !errors.Is(err, errNotBundle) {
			t.Errorf("%s: err = %v, want errNotBundle", what, err)
		}
	}
}

func TestBackupsWithinOneSecondGetDistinctNames(t *testing.T) {
	pg := newFakePG(t, "x\n")
	dir := t.TempDir()
	o := BackupOptions{DatabaseURL: testURL, Dir: dir, Label: LabelPreRestore, Tools: pg.tools, Now: at(t0)}
	first, err := Backup(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Backup(context.Background(), o)
	if err != nil {
		t.Fatalf("second backup in the same second: %v", err)
	}
	if first == second {
		t.Fatalf("both backups wrote %s", first)
	}
	all, err := List(dir)
	if err != nil || len(all) != 2 || !all[0].Created.After(all[1].Created) {
		t.Fatalf("list = %+v, %v", all, err)
	}
	if _, err := Verify(second); err != nil {
		t.Fatal(err)
	}
}

func TestBackupFailures(t *testing.T) {
	for _, tc := range []struct {
		flag, want string
	}{
		{"dump_fail", "connection refused"},
		{"dump_garbage", "does not read back"},
		// An object another role created in the database: the error names the fix.
		{"dump_denied", "sudo -u postgres psql -d felis -c 'ALTER TABLE servers_preserve OWNER TO felis'"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			pg := newFakePG(t, "x\n")
			pg.flag(t, tc.flag, "")
			dir := t.TempDir()
			_, err := Backup(context.Background(), BackupOptions{DatabaseURL: testURL, Dir: dir, Label: LabelDaily, Tools: pg.tools, Now: at(t0)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if e.Name() != ".lock" {
					t.Errorf("a failed backup left %s behind", e.Name())
				}
			}
		})
	}

	t.Run("bad label and url", func(t *testing.T) {
		pg := newFakePG(t, "x\n")
		if _, err := Backup(context.Background(), BackupOptions{DatabaseURL: testURL, Dir: t.TempDir(), Label: "../x", Tools: pg.tools}); err == nil {
			t.Error("label with a path separator accepted")
		}
		if _, err := Backup(context.Background(), BackupOptions{DatabaseURL: "host=x dbname=y", Dir: t.TempDir(), Label: "daily", Tools: pg.tools}); err == nil {
			t.Error("non-URL database accepted")
		}
	})

	t.Run("stale partials from a crashed run are cleared", func(t *testing.T) {
		pg := newFakePG(t, "x\n")
		dir := t.TempDir()
		stale := filepath.Join(dir, ".felis-db-20260101T000000Z-daily.tar.partial")
		if err := os.WriteFile(stale, []byte("half"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Backup(context.Background(), BackupOptions{DatabaseURL: testURL, Dir: dir, Label: LabelDaily, Tools: pg.tools, Now: at(t0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
			t.Error("stale partial survived")
		}
	})
}

// The installer's database is a k3s pod and the host has no client tools: every
// tool runs behind kubectl exec, reaches the database through the container's
// socket, and moves the archive over stdout and stdin.
func TestBackupAndRestoreThroughThePod(t *testing.T) {
	pg := newFakePod(t, "users: alice\n")
	dir := t.TempDir()
	path, err := Backup(context.Background(), BackupOptions{
		DatabaseURL: testURL, Dir: dir, Label: LabelDaily, Tools: pg.tools, Now: at(t0), Record: true,
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	m, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != 21 || !strings.Contains(m.PGDumpVersion, "13.23") || m.Counts == nil || m.Counts.Servers != 2 {
		t.Errorf("manifest = %+v", m)
	}
	args, _ := os.ReadFile(filepath.Join(pg.dir, "pg_dump.args"))
	if !strings.Contains(string(args), "--dbname="+podConn) || strings.Contains(string(args), "127.0.0.1") {
		t.Errorf("pg_dump args = %s, want the container's socket", args)
	}
	if pw, _ := os.ReadFile(filepath.Join(pg.dir, "pg_dump.password")); len(pw) != 0 {
		t.Errorf("PGPASSWORD reached the container: %q", pw)
	}
	if rec, _ := os.ReadFile(filepath.Join(pg.dir, "record.args")); !strings.Contains(string(rec), podConn) {
		t.Errorf("freshness record args = %s", rec)
	}
	// dump, list, --version, schema version, counts, record
	if n := pg.execRuns(t); n != 6 {
		t.Errorf("%d tool runs went through kubectl exec, want 6", n)
	}

	pg.setDB(t, "users: alice\nusers: bob\n")
	if _, err := restore(pg, dir, path, nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := pg.db(t); got != "users: alice\n" {
		t.Fatalf("db after restore = %q", got)
	}
	if args, _ := os.ReadFile(filepath.Join(pg.dir, "psql.args")); !strings.Contains(string(args), podConn) {
		t.Errorf("replay psql args = %s", args)
	}

	t.Run("a failed replay still rolls back", func(t *testing.T) {
		pg.setDB(t, "current\n")
		pg.flag(t, "restore_fail", "")
		defer os.Remove(filepath.Join(pg.dir, "restore_fail"))
		if _, err := restore(pg, dir, path, func(o *RestoreOptions) { o.SkipSafetyBackup = true }); err == nil || !strings.Contains(err.Error(), "rolled back") {
			t.Fatalf("err = %v", err)
		}
		if got := pg.db(t); got != "current\n" {
			t.Fatalf("db = %q, want it untouched", got)
		}
	})

	t.Run("the ownership hint uses the container's superuser", func(t *testing.T) {
		pg.flag(t, "dump_denied", "")
		defer os.Remove(filepath.Join(pg.dir, "dump_denied"))
		_, err := Backup(context.Background(), BackupOptions{DatabaseURL: testURL, Dir: t.TempDir(), Label: LabelDaily, Tools: pg.tools, Now: at(t0)})
		want := "sudo " + strings.Join(pg.tools.Exec, " ") + " psql -U postgres -d felis -c 'ALTER TABLE servers_preserve OWNER TO felis'"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})
}

func TestVerifyCatchesCorruption(t *testing.T) {
	pg := newFakePG(t, strings.Repeat("row\n", 64))
	dir := t.TempDir()
	path, err := Backup(context.Background(), BackupOptions{DatabaseURL: testURL, Dir: dir, Label: LabelManual, Tools: pg.tools, Now: at(t0)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	i := strings.Index(string(raw), "PGDMP")
	raw[i+10] ^= 0x20
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("flipped dump byte: err = %v, want member corrupt", err)
	}

	// A bundle intact inside but not the one the sidecar vouches for.
	raw[i+10] ^= 0x20
	raw = append(raw, make([]byte, 512)...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path); err == nil || !strings.Contains(err.Error(), ".sha256") {
		t.Fatalf("sidecar mismatch: err = %v", err)
	}

	junk := filepath.Join(dir, "junk.tar")
	if err := os.WriteFile(junk, []byte("not a tar"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(junk); err == nil {
		t.Fatal("junk verified")
	}
}

func TestVerifyRejectsUnlistedMember(t *testing.T) {
	pg := newFakePG(t, "x\n")
	dir := t.TempDir()
	path, err := Backup(context.Background(), BackupOptions{DatabaseURL: testURL, Dir: dir, Label: LabelManual, Tools: pg.tools, Now: at(t0)})
	if err != nil {
		t.Fatal(err)
	}
	// Re-pack with an extra member the manifest does not list.
	src, _ := os.Open(path)
	defer src.Close()
	out := filepath.Join(dir, "felis-db-20260924T040000Z-manual.tar")
	dst, _ := os.Create(out)
	tr, tw := tar.NewReader(src), tar.NewWriter(dst)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = tw.WriteHeader(h)
		_, _ = io.Copy(tw, tr)
	}
	_ = tw.WriteHeader(&tar.Header{Name: "state/etc/cron.d/evil", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = dst.Close()
	if _, err := Verify(out); err == nil || !strings.Contains(err.Error(), "not in the manifest") {
		t.Fatalf("err = %v", err)
	}
}

func TestListPruneCheck(t *testing.T) {
	pg := newFakePG(t, "x\n")
	dir := t.TempDir()
	backup := func(label string, when time.Time, keep int) {
		t.Helper()
		if _, err := Backup(context.Background(), BackupOptions{DatabaseURL: testURL, Dir: dir, Label: label, Keep: keep, Tools: pg.tools, Now: at(when)}); err != nil {
			t.Fatal(err)
		}
	}
	backup(LabelManual, t0.Add(-100*time.Hour), 0)
	for i := range 5 {
		backup(LabelDaily, t0.Add(time.Duration(i-5)*24*time.Hour), 3)
	}
	backup(LabelPreMigrate, t0.Add(-time.Hour), 3)

	all, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range all {
		got = append(got, b.Label+"@"+b.Created.Format("0102T15"))
	}
	want := []string{"pre-migrate@0924T02", "daily@0923T03", "daily@0922T03", "daily@0921T03", "manual@0919T23"}
	if !slices.Equal(got, want) {
		t.Fatalf("List = %v, want %v (dailies pruned to 3, others untouched)", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, BundleName(t0.Add(-5*24*time.Hour), LabelDaily)+".sha256")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a pruned bundle's sidecar survived")
	}

	if b, err := Check(dir, 26*time.Hour, t0); err != nil || b.Label != LabelPreMigrate {
		t.Errorf("Check fresh = %v, %v", b, err)
	}
	if _, err := Check(dir, 26*time.Hour, t0.Add(30*time.Hour)); err == nil {
		t.Error("Check accepted a 31h-old newest bundle")
	}
	if _, err := Check(filepath.Join(dir, "none"), time.Hour, t0); err == nil {
		t.Error("Check accepted an empty directory")
	}
}

func TestParseBundleName(t *testing.T) {
	for name, ok := range map[string]bool{
		"felis-db-20260924T033000Z-daily.tar":        true,
		"felis-db-20260924T033000Z-pre-migrate.tar":  true,
		"felis-db-20260924T033000Z-.tar":             false,
		"felis-db-20260924T033000Z-Daily.tar":        false,
		"felis-db-2026-daily.tar":                    false,
		"felis-db-20260924T033000Z-daily.tar.sha256": false,
		"other.tar": false,
	} {
		if _, _, got := ParseBundleName(name); got != ok {
			t.Errorf("ParseBundleName(%q) ok = %v, want %v", name, got, ok)
		}
	}
}

func TestParseConnStripsPassword(t *testing.T) {
	c, err := parseConn(testURL)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.uri, "s3cret") || c.password != "s3cret-pw" {
		t.Fatalf("conn = %+v", c)
	}
	if !strings.Contains(c.uri, "sslmode=disable") || !strings.HasPrefix(c.uri, "postgres://felis@127.0.0.1:5432/felis") {
		t.Fatalf("uri = %s", c.uri)
	}
	if _, err := parseConn("postgres://127.0.0.1/"); err == nil {
		t.Fatal("URL without a database accepted")
	}
}

func TestAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second:                 "0s",
		44 * time.Second:             "44s",
		90 * time.Second:             "1m",
		26*time.Hour + 5*time.Minute: "26h5m",
		3*24*time.Hour + 4*time.Hour: "3d4h",
		2*time.Hour + 30*time.Second: "2h0m",
	} {
		if got := Age(d); got != want {
			t.Errorf("Age(%s) = %q, want %q", d, got, want)
		}
	}
}
