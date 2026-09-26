package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/store"
)

func TestDBUsage(t *testing.T) {
	for _, args := range [][]string{{"db"}, {"db", "frobnicate"}, {"db", "restore"}, {"db", "verify"}, {"db", "backup", "extra"}} {
		var out, errBuf bytes.Buffer
		if code := run(args, &out, &errBuf); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if !strings.Contains(errBuf.String(), "felis db restore") {
			t.Errorf("%v: no usage on stderr: %q", args, errBuf.String())
		}
	}
}

func TestDBRestoreNeedsYes(t *testing.T) {
	// A bundle that does not exist fails verification (1) before -yes matters;
	// the -yes gate itself is exercised against a real bundle in internal/dbbackup
	// and on the VM. Here: the refusal path never reaches the config or database.
	var out, errBuf bytes.Buffer
	if code := run([]string{"db", "restore", "-dir", t.TempDir(), "missing.tar"}, &out, &errBuf); code != 1 {
		t.Fatalf("exit %d, stderr %q", code, errBuf.String())
	}
}

func TestParseWithArg(t *testing.T) {
	for _, args := range [][]string{{"-yes", "b.tar"}, {"b.tar", "-yes"}} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		yes := fs.Bool("yes", false, "")
		arg, ok := parseWithArg(fs, args)
		if !ok || arg != "b.tar" || !*yes {
			t.Errorf("%v -> %q ok=%v yes=%v", args, arg, ok, *yes)
		}
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if _, ok := parseWithArg(fs, []string{"a.tar", "b.tar"}); ok {
		t.Error("two positional arguments accepted")
	}
}

func TestResolveBundle(t *testing.T) {
	if got := resolveBundle("/var/lib/felis/db-backups", "felis-db-x.tar"); got != "/var/lib/felis/db-backups/felis-db-x.tar" {
		t.Errorf("bare name -> %s", got)
	}
	if got := resolveBundle("/var/lib/felis/db-backups", "/root/copy.tar"); got != "/root/copy.tar" {
		t.Errorf("path -> %s", got)
	}
}

func TestCleanServerList(t *testing.T) {
	raw := `{"apiVersion":"v1","kind":"List","metadata":{"resourceVersion":""},"items":[{
		"apiVersion":"felis.lolicon.best/v1alpha1","kind":"MinecraftServer",
		"metadata":{"name":"survival","namespace":"minecraft","uid":"u","resourceVersion":"42","generation":3,
			"creationTimestamp":"2026-09-01T00:00:00Z","managedFields":[{}],"labels":{"a":"b"}},
		"spec":{"desiredState":"Running"},"status":{"phase":"Running"}}]}`
	out, err := cleanServerList([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Kind  string           `json:"kind"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "List" || len(got.Items) != 1 {
		t.Fatalf("got %s", out)
	}
	it := got.Items[0]
	if _, ok := it["status"]; ok {
		t.Error("status kept")
	}
	md := it["metadata"].(map[string]any)
	for _, k := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields"} {
		if _, ok := md[k]; ok {
			t.Errorf("metadata.%s kept", k)
		}
	}
	if md["name"] != "survival" || md["namespace"] != "minecraft" || md["labels"] == nil {
		t.Errorf("identity lost: %v", md)
	}
	if it["spec"].(map[string]any)["desiredState"] != "Running" {
		t.Error("spec lost")
	}

	empty, err := cleanServerList([]byte(`{"items":null}`))
	if err != nil || !strings.Contains(string(empty), `"items": []`) {
		t.Errorf("empty list -> %s, %v", empty, err)
	}
	if _, err := cleanServerList([]byte("Warning: x\n{")); err == nil {
		t.Error("garbage parsed")
	}
}

func TestHasPending(t *testing.T) {
	ms := []store.Migration{{Version: 1}, {Version: 2}, {Version: 3}}
	if hasPending(map[int]struct{}{1: {}, 2: {}, 3: {}}, ms) {
		t.Error("fully applied reported pending")
	}
	if !hasPending(map[int]struct{}{1: {}, 2: {}}, ms) {
		t.Error("missing 3 not reported")
	}
}

type appliedDriver struct {
	store.Driver
	done map[int]struct{}
}

func (d appliedDriver) EnsureVersionTable(context.Context) error { return nil }
func (d appliedDriver) AppliedVersions(context.Context) (map[int]struct{}, error) {
	return d.done, nil
}

func TestPreMigrateBackupOnlyGuardsAPopulatedDatabase(t *testing.T) {
	ms := []store.Migration{{Version: 1}, {Version: 2}}
	// An unusable URL makes an attempted backup observable as an error without
	// any PostgreSQL tooling.
	const badURL = "not-a-url"
	for _, tc := range []struct {
		name    string
		done    map[int]struct{}
		attempt bool
	}{
		{"fresh database", map[int]struct{}{}, false},
		{"up to date", map[int]struct{}{1: {}, 2: {}}, false},
		{"pending on a populated database", map[int]struct{}{1: {}}, true},
	} {
		path, err := preMigrateBackup(context.Background(), appliedDriver{done: tc.done}, ms, config.DatabaseConfig{URL: badURL}, t.TempDir(), io.Discard)
		if attempted := err != nil; attempted != tc.attempt {
			t.Errorf("%s: attempted = %v (err %v), want %v", tc.name, attempted, err, tc.attempt)
		}
		if path != "" {
			t.Errorf("%s: path = %q", tc.name, path)
		}
	}
}

// audit-export takes a day or an RFC 3339 instant for each bound, and refuses a
// malformed or inverted window before it opens the config or the database.
func TestAuditExportBounds(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "0001-01-01T00:00:00Z"},
		{"2026-01-31", "2026-01-31T00:00:00Z"},
		{"2026-01-31T12:30:00+08:00", "2026-01-31T04:30:00Z"},
	} {
		got, err := parseExportBound(tc.in)
		if err != nil || got.Format(time.RFC3339) != tc.want {
			t.Errorf("parseExportBound(%q) = %v, %v; want %s", tc.in, got, err, tc.want)
		}
	}
	if _, err := parseExportBound("31/01/2026"); err == nil || err.Error() != `"31/01/2026" is neither a day (2026-01-31) nor an RFC 3339 instant (2026-01-31T12:00:00Z)` {
		t.Errorf("parseExportBound(31/01/2026) err = %v", err)
	}
	for _, tc := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"db", "audit-export", "-since", "yesterday"}, `felis db audit-export: -since: "yesterday" is neither`},
		{[]string{"db", "audit-export", "-until", "2026-13-01"}, `felis db audit-export: -until: "2026-13-01" is neither`},
		{[]string{"db", "audit-export", "-since", "2026-02-01", "-until", "2026-02-01"}, "felis db audit-export: -until 2026-02-01 is not after -since 2026-02-01"},
		{[]string{"db", "audit-export", "extra"}, "felis db audit-export [-config path]"},
	} {
		var out, errBuf bytes.Buffer
		code := run(append(tc.args, "-config", "/nonexistent/felis.toml"), &out, &errBuf)
		if code != 2 || !strings.Contains(errBuf.String(), tc.wantErr) {
			t.Errorf("%v: exit %d, stderr %q; want 2 and %q", tc.args, code, errBuf.String(), tc.wantErr)
		}
	}
}

// podK3s stands in for `k3s kubectl exec ... --`: it logs its argv and runs the
// command after -- from the "container" directory, which is the only place the
// PostgreSQL tools exist, as on an installed host.
const podK3s = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_DIR/k3s.args"
while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
tool=$1; shift
exec /usr/bin/env -i FAKE_DIR="$FAKE_DIR" PATH=/usr/bin:/bin "$FAKE_DIR/container/$tool" "$@"
`

var podTools = map[string]string{
	"pg_dump": `#!/bin/sh
case "$1" in --version) echo "pg_dump (PostgreSQL) 18.6"; exit 0 ;; esac
printf 'PGDMP-fake-archive'
`,
	"pg_restore": `#!/bin/sh
cat > /dev/null
`,
	"psql": `#!/bin/sh
for a in "$@"; do [ "$a" = "-c" ] && { echo 3; exit 0; }; done
cat > /dev/null
`,
}

const podPassword = "pw-must-stay-on-the-host"

// podDB is the host config's [database] on an installed host.
var podDB = config.DatabaseConfig{
	URL:        "postgres://felis:" + podPassword + "@127.0.0.1:15432/felis?sslmode=disable",
	Deployment: "felis/felis-postgres",
}

const podExecPrefix = "kubectl exec -i -n felis deploy/felis-postgres -c postgres -- "

// newPodRig puts the fake k3s on PATH, alone, and returns the directory its
// k3s.args log lands in.
func newPodRig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	container := filepath.Join(dir, "container")
	for _, d := range []string{bin, container} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(bin, "k3s"), podK3s, 0o755)
	for name, body := range podTools {
		writeTestFile(t, filepath.Join(container, name), body, 0o755)
	}
	t.Setenv("PATH", bin)
	t.Setenv("FAKE_DIR", dir)
	return dir
}

// podRuns returns what the fake k3s ran since the last call, failing on any
// run outside the database container or with the password on its command
// line (visible to every local user in ps).
func podRuns(t *testing.T, dir string) []string {
	t.Helper()
	log := filepath.Join(dir, "k3s.args")
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("nothing ran through k3s: %v", err)
	}
	os.Remove(log)
	var runs []string
	for _, line := range strings.Split(strings.TrimSpace(string(argv)), "\n") {
		if !strings.HasPrefix(line, podExecPrefix) {
			t.Errorf("k3s ran %q, want everything under %q", line, podExecPrefix)
		}
		if strings.Contains(line, podPassword) {
			t.Errorf("the password crossed into the pod on a command line: %q", line)
		}
		runs = append(runs, strings.TrimPrefix(line, podExecPrefix))
	}
	return runs
}

func ranIn(runs []string, prefix string) bool {
	for _, r := range runs {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

// TestDBBackupAndRestoreRunTheToolsInTheDatabasePod: on an installed host the
// database is a k3s Deployment and no PostgreSQL client exists outside it, so
// `felis db backup` and `restore` must reach the tools through kubectl exec,
// over the pod's socket, and without putting the role's password on a command
// line.
func TestDBBackupAndRestoreRunTheToolsInTheDatabasePod(t *testing.T) {
	dir := newPodRig(t)
	toml := strings.Replace(installerTOML("example.com", "127.0.0.1"),
		`url = "postgres://felis:pw@127.0.0.1:5432/felis?sslmode=disable"`,
		`url = "`+podDB.URL+`"
deployment = "`+podDB.Deployment+`"`, 1)
	cfg := filepath.Join(dir, "felis.toml")
	writeTestFile(t, cfg, toml, 0o600)

	var out, errBuf bytes.Buffer
	bundles := filepath.Join(dir, "bundles")
	if code := run([]string{"db", "backup", "-config", cfg, "-dir", bundles, "-state-dir", "", "-no-servers"}, &out, &errBuf); code != 0 {
		t.Fatalf("backup: exit %d: %s", code, errBuf.String())
	}
	bundle := strings.TrimSpace(strings.TrimPrefix(out.String(), "felis db backup: wrote "))
	if _, err := dbbackupVerify(bundle); err != nil {
		t.Fatalf("the bundle does not verify: %v", err)
	}
	runs := podRuns(t, dir)
	if !ranIn(runs, "pg_dump --format=custom --no-password --dbname=host=/var/run/postgresql port=5432 dbname='felis' user='felis'") {
		t.Errorf("pg_dump did not dump over the pod's socket as felis on felis: %q", runs)
	}

	out.Reset()
	errBuf.Reset()
	if code := run([]string{"db", "restore", "-config", cfg, "-dir", bundles, "-yes", "-force", "-no-safety-backup", bundle}, &out, &errBuf); code != 0 {
		t.Fatalf("restore: exit %d: %s", code, errBuf.String())
	}
	runs = podRuns(t, dir)
	if !ranIn(runs, "pg_restore --no-owner --no-privileges --file=-") || !ranIn(runs, "psql -X -q -w -v ON_ERROR_STOP=1 -d host=/var/run/postgresql") {
		t.Errorf("the replay did not run in the pod: %q", runs)
	}
}

// TestPreMigrateBackupRunsInTheDatabasePod: the snapshot in front of an upgrade
// is the one taken most often, by bootstrap on every rerun.
func TestPreMigrateBackupRunsInTheDatabasePod(t *testing.T) {
	dir := newPodRig(t)
	ms := []store.Migration{{Version: 1}, {Version: 2}}
	// The snapshot also bundles /etc/felis, which a test machine may lack; the
	// dump runs first either way.
	_, err := preMigrateBackup(context.Background(), appliedDriver{done: map[int]struct{}{1: {}}}, ms, podDB, filepath.Join(dir, "bundles"), io.Discard)
	if err != nil && !strings.Contains(err.Error(), "read host state") {
		t.Fatalf("snapshot: %v", err)
	}
	if runs := podRuns(t, dir); !ranIn(runs, "pg_dump --format=custom") {
		t.Errorf("pg_dump did not run in the pod: %q", runs)
	}
}

func TestDBToolsNeedTheRoleAndDatabase(t *testing.T) {
	if tools, err := dbTools(config.DatabaseConfig{URL: "postgres://felis:pw@db:5432/felis"}); err != nil || len(tools.Exec) != 0 {
		t.Errorf("no deployment: tools %+v err %v, want the PATH tools", tools, err)
	}
	for _, u := range []string{"postgres://db:5432/felis", "postgres://felis:pw@db:5432/"} {
		if _, err := dbTools(config.DatabaseConfig{URL: u, Deployment: "felis/felis-postgres"}); err == nil {
			t.Errorf("%s: no error, want a refusal (the pod connection needs the role and the database)", u)
		}
	}
	tools, err := dbTools(config.DatabaseConfig{URL: `postgres://o%27brien@db/my%20db`, Deployment: "felis/felis-postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tools.Conn, `dbname='my db' user='o\'brien'`) {
		t.Errorf("Conn = %q, want the values quoted for libpq", tools.Conn)
	}
}

var dbbackupVerify = dbbackup.Verify
