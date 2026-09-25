package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"strings"
	"testing"
	"time"

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
		path, err := preMigrateBackup(context.Background(), appliedDriver{done: tc.done}, ms, badURL, t.TempDir(), io.Discard)
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
