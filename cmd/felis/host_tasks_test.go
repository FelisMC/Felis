package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/nodecontrol"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/watchdog"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// stubFelis writes a felis stand-in that records each call's arguments in log, and fails
// a call whose arguments contain $STUB_FAIL.
func stubFelis(t *testing.T, dir, log string) string {
	t.Helper()
	path := filepath.Join(dir, "felis")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(log) + "\n" +
		"case \"$*\" in *\"${STUB_FAIL:-@none@}\"*) echo \"stub refused: $*\"; exit 1 ;; esac\n" +
		"if [ \"$1\" = support-bundle ] && [ -z \"$STUB_NO_BUNDLE\" ]; then : > \"$3/felis-support-20261012.tar.gz\"; fi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestHostTasksRunTheirCommands(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	e := nodeExecutor{binary: stubFelis(t, dir, log), config: "/etc/felis/felis.host.toml"}
	bundle := "felis-db-20261001T033000Z-daily.tar"
	for _, r := range []nodecontrol.Request{
		{Action: "db-backup"},
		{Action: "db-verify", Params: map[string]string{"bundle": bundle}},
		{Action: "db-delete", Params: map[string]string{"bundle": bundle}},
		{Action: "offsite-list"},
		{Action: "offsite-check-key"},
		{Action: "restore-host", Params: map[string]string{"bundle": bundle, "takeOver": "true", "force": "false"}},
		{Action: "backup-now", Params: map[string]string{"stop": "true", "servers": "survival,creative"}},
		{Action: "backup-now"},
		{Action: "rotate-token", Params: map[string]string{"kind": "registry"}},
		{Action: "domain-check"},
		{Action: "domain-set", Params: map[string]string{"domain": "play.example.com"}},
		{Action: "converge", Params: map[string]string{"userRcon": "true"}},
	} {
		if err := e.hostTask(context.Background(), r, func(string) error { return nil }, io.Discard); err != nil {
			t.Fatalf("%s: %v", r.Action, err)
		}
	}
	want := []string{
		"db backup -config /etc/felis/felis.host.toml -dir /var/lib/felis/db-backups -label manual",
		"db verify -dir /var/lib/felis/db-backups " + bundle,
		"db delete -dir /var/lib/felis/db-backups " + bundle,
		"offsite list -config /etc/felis/felis.host.toml",
		"offsite check-key -config /etc/felis/felis.host.toml",
		"restore-host -config /etc/felis/felis.host.toml -yes -bundle " + bundle + " -take-over",
		"backup-now -config /etc/felis/felis.host.toml -yes -stop survival creative",
		"backup-now -config /etc/felis/felis.host.toml -yes",
		"rotate-token -config /etc/felis/felis.host.toml -yes registry",
		"domain check",
		"domain set -yes play.example.com",
		"converge -config /etc/felis/felis.host.toml -user-rcon",
	}
	if got := readLines(t, log); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	t.Setenv("STUB_FAIL", "rotate-token")
	if err := e.hostTask(context.Background(), nodecontrol.Request{Action: "rotate-token", Params: map[string]string{"kind": "db"}}, func(string) error { return nil }, io.Discard); err == nil {
		t.Fatal("a failing command reported success")
	}
}

// controlPlane is a fake cluster whose deployments report as ready as many replicas as
// they ask for, the way the real controller would settle.
func controlPlane(replicas map[string]int32) *fake.Clientset {
	var objs []runtime.Object
	for name, n := range replicas {
		n := n
		objs = append(objs, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "felis"}, Spec: appsv1.DeploymentSpec{Replicas: &n}})
	}
	cs := fake.NewSimpleClientset(objs...)
	cs.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get := action.(k8stesting.GetAction)
		obj, err := cs.Tracker().Get(appsv1.SchemeGroupVersion.WithResource("deployments"), get.GetNamespace(), get.GetName())
		if err != nil {
			return true, nil, err
		}
		d := obj.(*appsv1.Deployment).DeepCopy()
		d.Status.Replicas, d.Status.ReadyReplicas = *d.Spec.Replicas, *d.Spec.Replicas
		return true, d, nil
	})
	return cs
}

func replicasOf(t *testing.T, cs *fake.Clientset, name string) int32 {
	t.Helper()
	d, err := cs.AppsV1().Deployments("felis").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return *d.Spec.Replicas
}

func TestRestoreDatabaseStopsAndRestartsTheControlPlane(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	cs := controlPlane(map[string]int32{platform.SAAPI: 2, platform.SAOperator: 1})
	e := nodeExecutor{cs: cs, binary: stubFelis(t, dir, log), config: "/etc/felis/felis.host.toml", controlNamespace: "felis"}
	var stages []string
	stage := func(s string) error {
		stages = append(stages, s)
		if s == "restore_database" && (replicasOf(t, cs, platform.SAAPI) != 0 || replicasOf(t, cs, platform.SAOperator) != 0) {
			t.Fatal("the database was restored under a running control plane")
		}
		return nil
	}
	bundle := "felis-db-20261001T033000Z-daily.tar"
	if err := e.restoreDatabase(context.Background(), bundle, stage, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stages, ","); got != "verify,stop_control_plane,restore_database,migrate,start_control_plane" {
		t.Fatalf("stages %s", got)
	}
	if got := readLines(t, log); len(got) != 3 || !strings.HasPrefix(got[1], "db restore -config /etc/felis/felis.host.toml -dir /var/lib/felis/db-backups -yes "+bundle) || got[2] != "migrate up -config /etc/felis/felis.host.toml" {
		t.Fatalf("calls %q", got)
	}
	if replicasOf(t, cs, platform.SAAPI) != 2 || replicasOf(t, cs, platform.SAOperator) != 1 {
		t.Fatal("the control plane did not get its replicas back")
	}

	t.Setenv("STUB_FAIL", "db restore")
	if err := e.restoreDatabase(context.Background(), bundle, func(string) error { return nil }, io.Discard); err == nil {
		t.Fatal("a failed restore reported success")
	}
	if replicasOf(t, cs, platform.SAAPI) != 2 || replicasOf(t, cs, platform.SAOperator) != 1 {
		t.Fatal("a failed restore left the panel stopped")
	}
}

func TestHostSettingsRecordAndReconfigure(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "host-settings.env")
	if err := os.WriteFile(settings, []byte("FELIS_DB_BACKUP_KEEP='9'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envLog := filepath.Join(dir, "env")
	shell := filepath.Join(dir, "bash")
	os.WriteFile(shell, []byte("#!/bin/sh\ncat > /dev/null\nprintf '%s %s\\n' \"$FELIS_INSTALL_MODE\" \"$FELIS_RECONFIGURE\" >> "+shellQuote(envLog)+"\n"), 0o755)
	e := nodeExecutor{shell: shell, settingsFile: settings}
	if err := e.hostSettings(context.Background(), map[string]string{"dbBackupTime": "04:15", "velocityXmx": "768M"}, func(string) error { return nil }, io.Discard); err != nil {
		t.Fatal(err)
	}
	vals, err := readEnvFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if vals["FELIS_DB_BACKUP_TIME"] != "*-*-* 04:15:00" || vals["FELIS_VELOCITY_XMX"] != "768M" || vals["FELIS_DB_BACKUP_KEEP"] != "9" {
		t.Fatalf("settings %v", vals)
	}
	if got := readLines(t, envLog); len(got) != 1 || got[0] != "reconfigure db-backup,velocity" {
		t.Fatalf("installer runs %q", got)
	}
	if info, _ := os.Stat(settings); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

// TestEnvFileRoundTrip pins the KEY='value' format the installer sources, and that a
// value that would break out of its quotes is refused.
func TestEnvFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.env")
	if err := writeEnvFile(path, "# header\n", map[string]string{"B": "two words", "A": "a/b+c="}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "# header\nA='a/b+c='\nB='two words'\n" {
		t.Fatalf("%q", raw)
	}
	vals, err := readEnvFile(path)
	if err != nil || vals["A"] != "a/b+c=" || vals["B"] != "two words" {
		t.Fatalf("%v %v", vals, err)
	}
	if err := writeEnvFile(path, "", map[string]string{"A": "it's"}); err == nil {
		t.Fatal("a quote was written")
	}
	if vals, err := readEnvFile(filepath.Join(t.TempDir(), "missing")); err != nil || len(vals) != 0 {
		t.Fatal(vals, err)
	}
}

func TestSupportBundleIsOfferedForDownload(t *testing.T) {
	dir := t.TempDir()
	support := filepath.Join(dir, "support")
	os.MkdirAll(support, 0o700)
	os.WriteFile(filepath.Join(support, "felis-support-old.tar.gz"), nil, 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(support, "felis-support-old.tar.gz"), old, old)
	e := nodeExecutor{binary: stubFelis(t, dir, filepath.Join(dir, "calls")), supportDir: support}
	manager, err := nodecontrol.Open(context.Background(), filepath.Join(dir, "state"), e.run)
	if err != nil {
		t.Fatal(err)
	}
	task, err := manager.Start(nodecontrol.Request{Action: "support-bundle", Params: map[string]string{"serverLogs": "true"}}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	got, _ := manager.Get(task.ID)
	if got.State != "succeeded" || got.Artifact != filepath.Join(support, "felis-support-20261012.tar.gz") {
		t.Fatalf("task %+v", got)
	}
	if calls := readLines(t, filepath.Join(dir, "calls")); calls[0] != "support-bundle -o "+support+" -server-logs" {
		t.Fatalf("calls %q", calls)
	}

	// A run that writes no bundle fails, whatever older bundles the directory holds.
	os.Remove(got.Artifact)
	t.Setenv("STUB_NO_BUNDLE", "1")
	task, err = manager.Start(nodecontrol.Request{Action: "support-bundle"}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	if got, _ := manager.Get(task.ID); got.State != "failed" || got.Artifact != "" {
		t.Fatalf("a run that wrote nothing offered %+v", got)
	}
}

func TestCurrentHostSettingsReadTheUnits(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "felis-db-backup.timer"), []byte("[Timer]\nOnCalendar=*-*-* 04:15:00\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "felis-db-backup.service"), []byte("[Service]\nExecStart=/usr/local/bin/felis db backup -label daily -keep 21 -metrics-file x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "felis-velocity.service"), []byte("ExecStart=/opt/jre/bin/java -Xms16M -Xmx768M -jar velocity.jar\n"), 0o644)
	if v := currentHostSettings(dir); v.DBBackupTime != "04:15" || v.DBBackupKeep != 21 || v.VelocityXmx != "768M" {
		t.Fatalf("%+v", v)
	}
	for _, cal := range []string{"Mon *-*-* 04:15:00", "*-*-* 04:15:30"} {
		os.WriteFile(filepath.Join(dir, "felis-db-backup.timer"), []byte("[Timer]\nOnCalendar="+cal+"\n"), 0o644)
		if v := currentHostSettings(dir); v.DBBackupTime != "" {
			t.Fatalf("a calendar the panel cannot show (%s) read as %q", cal, v.DBBackupTime)
		}
	}
}

func TestConnectionAndStorageOf(t *testing.T) {
	var cfg config.Config
	cfg.Auth.PanelHostname, cfg.Auth.AdminHostname = "console.example.com", "op.console.example.com"
	if c := connectionOf(&cfg); c.Mode != "local" || c.PanelHost != "console.example.com" {
		t.Fatalf("%+v", c)
	}
	cfg.Auth.ClientIPHeader = "X-Forwarded-For"
	if c := connectionOf(&cfg); c.Mode != "reverse-proxy" {
		t.Fatalf("%+v", c)
	}
	cfg.Auth.AccessJWTAud = "aud"
	if c := connectionOf(&cfg); c.Mode != "cloudflare" {
		t.Fatalf("%+v", c)
	}
	if s := storageOf(&cfg); s.Mode != "local" {
		t.Fatalf("%+v", s)
	}
	cfg.Registry.UserUploadsContext = "s3://uploads/ctx"
	cfg.Registry.S3.Endpoint, cfg.Registry.S3.Region = "https://s3.example.com", "auto"
	if s := storageOf(&cfg); s.Mode != "s3" || s.Bucket != "uploads" || s.Endpoint != "https://s3.example.com" || s.Region != "auto" {
		t.Fatalf("%+v", s)
	}
}

func TestHostQuietMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "watchdog-quiet-until")
	quiet := hostQuiet(path)
	until := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if err := quiet(until); err != nil {
		t.Fatal(err)
	}
	if got := watchdog.QuietUntil(path); !got.Equal(until) {
		t.Fatalf("held until %v, want %v", got, until)
	}
	if err := quiet(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("lifting left the marker")
	}
	if err := quiet(time.Time{}); err != nil {
		t.Fatal("lifting no hold failed:", err)
	}
}

func TestDoctorViewGroupsByArea(t *testing.T) {
	r := doctorReport{host: "ctl-a", at: time.Unix(0, 0), source: "checks run as felis-watchdog.service runs them", notes: []string{"no heartbeat"},
		skip: map[string]string{"offsite": "not configured"},
		findings: []watchdog.Finding{
			{Key: "deployment/felis-api", Severity: watchdog.Critical, SummaryEN: "felis-api has no ready replica"},
			{Key: "disk/var", Severity: watchdog.Warning, SummaryEN: "/var is 92% full"},
			{Key: "timer/felis-db-backup.timer", Severity: watchdog.Warning, SummaryEN: "inactive"},
		}}
	v := r.view()
	raw, _ := json.Marshal(v)
	if v.Critical != 1 || v.Warning != 2 || len(v.Areas) != len(doctorAreas) {
		t.Fatalf("%s", raw)
	}
	state := map[string]doctorAreaView{}
	for _, a := range v.Areas {
		state[a.Key] = a
	}
	if state["cluster"].State != "problem" || state["disk"].State != "problem" || state["systemd"].State != "problem" ||
		state["offsite"].State != "skipped" || state["offsite"].Skipped != "not configured" || state["memory"].State != "ok" {
		t.Fatalf("%s", raw)
	}
	if v.Notes[0] != "no heartbeat" || v.Source == "" {
		t.Fatalf("%s", raw)
	}
}
