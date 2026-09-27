package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/watchdog"
)

// bootstrapWatchdogExecStart is the ExecStart= line deploy/bootstrap.sh writes
// into felis-watchdog.service, with its variables filled in as an install
// fills them.
func bootstrapWatchdogExecStart(t *testing.T, vars map[string]string) string {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/bootstrap.sh")
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "ExecStart=${HOST_BIN} watchdog -config") {
			line = l
		}
	}
	if line == "" {
		t.Fatal("deploy/bootstrap.sh writes no `ExecStart=${HOST_BIN} watchdog -config` line")
	}
	line = strings.ReplaceAll(line, "${NODE_IP:+ -node-ip ${NODE_IP}}", " -node-ip "+vars["NODE_IP"])
	line = regexp.MustCompile(`\$\{([A-Za-z_]+)\}`).ReplaceAllStringFunc(line, func(m string) string {
		v, ok := vars[m[2:len(m)-1]]
		if !ok {
			t.Fatalf("bootstrap's watchdog ExecStart= uses %s, which this test does not fill in", m)
		}
		return v
	})
	return line
}

// felis doctor reads the watchdog's settings from the unit the installer
// writes, so it checks the paths the timer's runs check.
func TestWatchdogUnitFlagsReadsTheInstallersUnit(t *testing.T) {
	dir := t.TempDir()
	exec := bootstrapWatchdogExecStart(t, map[string]string{
		"HOST_BIN": "/usr/local/bin/felis", "STATE_DIR": "/srv/felis-etc", "WATCHDOG_STATE": "/srv/watchdog/state.json",
		"WATCHDOG_QUIET_FILE": "/srv/quiet-until", "FELIS_DB_BACKUP_DIR": "/srv/db-backups", "FELIS_GAME_PORT": "25577",
		"disks": "/,/srv/data", "NODE_IP": "10.0.0.5",
	})
	unit := filepath.Join(dir, "felis-watchdog.service")
	writeTestFile(t, unit, "[Unit]\nDescription=Felis watchdog\n\n[Service]\nType=oneshot\n"+exec+"\nTimeoutStartSec=3min\n", 0o644)

	w, found, err := watchdogUnitFlags(unit)
	if err != nil || !found {
		t.Fatalf("found %v, err %v", found, err)
	}
	got := []string{w.cfgPath, w.statePath, w.quietPath, w.backupDir, w.proxyAddr, w.diskPaths, w.nodeIP, w.heartbeatFile, w.controlNS}
	want := []string{"/srv/felis-etc/felis.host.toml", "/srv/watchdog/state.json", "/srv/quiet-until", "/srv/db-backups", "127.0.0.1:25577", "/,/srv/data", "10.0.0.5", defaultHeartbeatFile, "felis"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("read\n %q\nwant\n %q", got, want)
	}

	w, found, err = watchdogUnitFlags(filepath.Join(dir, "missing.service"))
	if err != nil || found || w.cfgPath != "/etc/felis/felis.toml" || w.backupDir != "/var/lib/felis/db-backups" {
		t.Errorf("no unit: found %v, err %v, config %q, backups %q; want the watchdog's defaults", found, err, w.cfgPath, w.backupDir)
	}

	writeTestFile(t, unit, "[Service]\nExecStart=/usr/local/bin/felis version\n", 0o644)
	if _, found, err := watchdogUnitFlags(unit); !found || err == nil || !strings.Contains(err.Error(), "runs no `felis watchdog`") {
		t.Errorf("a unit that runs something else: found %v, err %v", found, err)
	}
	writeTestFile(t, unit, "[Service]\nExecStart=/usr/local/bin/felis watchdog -no-such-flag x\n", 0o644)
	if _, _, err := watchdogUnitFlags(unit); err == nil {
		t.Error("a flag this binary does not know was accepted")
	}
	writeTestFile(t, unit, "[Service]\nExecStart=/usr/local/bin/felis watchdog -backup-dir \"\"\t-proxy-addr '127.0.0.1:1' -disk-paths \"/a b\"\n", 0o644)
	if w, _, err := watchdogUnitFlags(unit); err != nil || w.backupDir != "" || w.proxyAddr != "127.0.0.1:1" || w.diskPaths != "/a b" {
		t.Errorf("quoted words: backups %q, proxy %q, disks %q, err %v", w.backupDir, w.proxyAddr, w.diskPaths, err)
	}
}

func TestFindingArea(t *testing.T) {
	for key, want := range map[string]string{
		"kube-api":                           "cluster",
		"deployment/felis-api":               "cluster",
		"system-server/lobby":                "cluster",
		"server-failed/survival":             "cluster",
		"job-failed/reaper-123":              "cluster",
		"reaper-stale":                       "cluster",
		"node/felis-1/NotReady":              "cluster",
		"postgres":                           "postgres",
		"proxy":                              "proxy",
		"db-backup":                          "db-backup",
		"db-backup-servers":                  "db-backup",
		"offsite":                            "offsite",
		"scan-db":                            "scan-db",
		"disk//var/lib/felis":                "disk",
		"memory":                             "memory",
		"k3s-certs":                          "k3s-certs",
		"host-address":                       "host-address",
		"clock":                              "clock",
		"config":                             "config",
		"unit/felis-offsite.service":         "systemd",
		"timer/felis-db-backup.timer":        "systemd",
		"watchdog/unit":                      "alerts",
		"watchdog/heartbeat":                 "alerts",
		"alerts/relay":                       "alerts",
		"alerts/recipients":                  "alerts",
		"something-a-later-release-reported": "something-a-later-release-reported",
	} {
		if got := findingArea(key); got != want {
			t.Errorf("findingArea(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestAlertReachFindings(t *testing.T) {
	relay := &config.Config{SMTP: config.SMTPConfig{Host: "smtp.example.com"}}
	keys := func(fs []watchdog.Finding) string {
		var k []string
		for _, f := range fs {
			k = append(k, f.Key)
		}
		return strings.Join(k, ",")
	}
	for _, tc := range []struct {
		what      string
		cfg       *config.Config
		owners    []string
		ownersErr error
		want      string
	}{
		{"a relay and an owner", relay, []string{"owner@example.com"}, nil, ""},
		{"no relay", &config.Config{}, []string{"owner@example.com"}, nil, "alerts/relay"},
		{"no owner with an address", relay, nil, nil, "alerts/recipients"},
		{"PostgreSQL down: its own finding says so", relay, nil, errors.New("refused"), ""},
		{"neither", &config.Config{}, nil, nil, "alerts/relay,alerts/recipients"},
	} {
		if got := keys(alertReachFindings(tc.cfg, tc.owners, tc.ownersErr)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.what, got, tc.want)
		}
	}
}

// fakeSystemctl answers list-units with failed and is-active from states;
// a unit missing from states is "inactive", as systemctl says, and one whose
// state is "" gets no answer.
func fakeSystemctl(failed string, states map[string]string) func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "systemctl" || len(args) == 0 {
			return nil, errors.New("unexpected command " + name)
		}
		switch args[0] {
		case "list-units":
			return []byte(failed), nil
		case "is-active":
			if s, ok := states[args[1]]; ok && s == "" {
				return nil, errors.New("signal: killed")
			} else if ok {
				return []byte(s + "\n"), nil
			}
			return []byte("inactive\n"), errors.New("exit status 3")
		}
		return nil, errors.New("unexpected systemctl " + args[0])
	}
}

func TestUnitFindings(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"k3s.service", "felis-velocity.service", "felis-db-backup.timer", "felis-offsite.timer", "felis-offsite.service"} {
		writeTestFile(t, filepath.Join(dir, f), "[Unit]\n", 0o644)
	}
	env := doctorEnv{unitDir: dir, run: fakeSystemctl(
		"felis-offsite.service loaded failed failed Felis off-site copy\nfelis-velocity.service loaded failed failed Velocity\n",
		map[string]string{"k3s.service": "active", "felis-db-backup.timer": "active", "felis-offsite.timer": "inactive"},
	)}
	var got []string
	for _, f := range unitFindings(context.Background(), env) {
		got = append(got, string(f.Severity)+" "+f.Key+": "+f.SummaryEN)
	}
	want := []string{
		"critical unit/felis-offsite.service: felis-offsite.service failed",
		"critical unit/felis-velocity.service: felis-velocity.service failed",
		"warning timer/felis-offsite.timer: felis-offsite.timer is inactive: the job it starts no longer runs",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings:\n%s\nwant (felis-nano.service has no unit file here, and a failed unit is reported once):\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	env.run = fakeSystemctl("", map[string]string{"k3s.service": "activating", "felis-velocity.service": "active", "felis-db-backup.timer": "active", "felis-offsite.timer": "active"})
	got = nil
	for _, f := range unitFindings(context.Background(), env) {
		got = append(got, f.Key+": "+f.SummaryEN)
	}
	if strings.Join(got, "\n") != "unit/k3s.service: k3s.service is activating" {
		t.Errorf("findings %q, want only k3s.service, which is not active", got)
	}

	env.run = fakeSystemctl("", map[string]string{"k3s.service": "active", "felis-velocity.service": "active", "felis-db-backup.timer": "", "felis-offsite.timer": "active"})
	got = nil
	for _, f := range unitFindings(context.Background(), env) {
		got = append(got, f.Key+": "+f.SummaryEN)
	}
	if want := "timer/felis-db-backup.timer: felis-db-backup.timer is in an unknown state (systemctl is-active: signal: killed): the job it starts no longer runs"; strings.Join(got, "\n") != want {
		t.Errorf("findings %q, want %q", got, want)
	}

	env.run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no systemctl") }
	if fs := unitFindings(context.Background(), env); len(fs) != 1 || fs[0].Key != "unit/systemctl" || fs[0].Severity != watchdog.Warning {
		t.Errorf("without systemctl: %+v, want the one unit/systemctl warning", fs)
	}
}

// quoteArgs writes args as an ExecStart= line does, each word in quotes so an
// empty one survives.
func quoteArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = `"` + a + `"`
	}
	return strings.Join(q, " ")
}

// doctorHost is a host with the installer's watchdog unit, whose API server
// and PostgreSQL are down.
func doctorHost(t *testing.T, cfg string, extraFlags string) (env doctorEnv, h *watchdogHost) {
	t.Helper()
	h = newWatchdogHost(t, cfg, nil)
	unitDir := t.TempDir()
	writeTestFile(t, filepath.Join(unitDir, "felis-watchdog.service"),
		"[Service]\nType=oneshot\nExecStart=/usr/local/bin/felis watchdog "+quoteArgs(h.args)+
			// Off this machine's disk, whose free space is not the test's.
			` -disk-paths "/nonexistent-felis-doctor-test"`+extraFlags+"\n", 0o644)
	writeTestFile(t, filepath.Join(unitDir, "felis-velocity.service"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(unitDir, "felis-offsite.timer"), "[Unit]\n", 0o644)
	return doctorEnv{
		unitDir: unitDir, now: time.Now(), host: "felis-test",
		run: fakeSystemctl("felis-db-backup.service loaded failed failed Felis database backup\n",
			map[string]string{"felis-velocity.service": "active", "felis-offsite.timer": "active"}),
	}, h
}

func TestDoctorReportsByArea(t *testing.T) {
	env, _ := doctorHost(t, testWatchdogConfig, "")
	var out bytes.Buffer
	code := runDoctor(context.Background(), env, &out)
	got := out.String()
	if code != 1 {
		t.Errorf("exit %d, want 1 with problems found", code)
	}
	for _, want := range []string{
		"felis doctor on felis-test at ",
		"checks run as " + filepath.Join(env.unitDir, "felis-watchdog.service") + " runs them (config ",
		"✓  configuration\n",
		"✗  Kubernetes cluster\n     critical kube-api: ",
		"✗  PostgreSQL\n     critical postgres: ",
		"-  game proxy: not checked, no -proxy-addr\n",
		"-  database backups: not checked, no -backup-dir\n",
		"-  off-site copy: not checked, not configured\n",
		"-  build scan database: not checked, builds do not scan against the registry's copy\n",
		"-  k3s certificates: not checked, no -k3s-cert-dirs\n",
		"-  node address: not checked, no -node-ip\n",
		"✗  systemd units and timers\n     critical unit/felis-db-backup.service: felis-db-backup.service failed\n" +
			"              → journalctl -u felis-db-backup.service -n 100 --no-pager; once fixed, sudo systemctl reset-failed felis-db-backup.service",
		"!  alerting\n     warning  alerts/relay: no [smtp] relay is configured",
		"\n4 problem(s): 3 critical, 1 warning(s)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "alerts/recipients") {
		t.Errorf("reported no recipients although PostgreSQL, which names them, is down:\n%s", got)
	}
	if strings.Contains(got, "note: no heartbeat URL") {
		t.Errorf("the host has a heartbeat URL:\n%s", got)
	}
}

// A doctor run is only a look: whatever is due to be mailed stays due, no
// heartbeat is pinged, and the watchdog's state is left as it was.
func TestDoctorMailsPingsAndSavesNothing(t *testing.T) {
	cfg := testWatchdogConfig + "[smtp]\nhost = \"smtp.example.com\"\nport = 587\nfrom = \"felis@example.com\"\n"
	env, h := doctorHost(t, cfg, "")
	if err := watchdog.SaveState(h.statePath, duePostgres("cached-pw")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(h.statePath)
	if err != nil {
		t.Fatal(err)
	}
	watchdogSender = h.rec.sender
	defer func() { watchdogSender = smtpSender }()
	var out bytes.Buffer
	runDoctor(context.Background(), env, &out)
	if !strings.Contains(out.String(), "critical postgres: ") {
		t.Fatalf("the due PostgreSQL alert was not seen:\n%s", out.String())
	}
	if len(h.rec.sent) != 0 || len(h.rec.relays) != 0 {
		t.Errorf("mailed %v", h.rec.sent)
	}
	if n := len(h.pings.pings); n != 0 {
		t.Errorf("pinged the heartbeat %d times", n)
	}
	after, err := os.ReadFile(h.statePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("the watchdog state changed (err %v)", err)
	}
	if _, err := os.Stat(h.fallbackPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a fallback state was written: %v", err)
	}
}

func TestDoctorNotes(t *testing.T) {
	env, h := doctorHost(t, testWatchdogConfig, "")
	if err := os.Remove(filepath.Join(h.dir, "watchdog-heartbeat-url")); err != nil {
		t.Fatal(err)
	}
	until := env.now.Add(10 * time.Minute).Unix()
	writeTestFile(t, filepath.Join(h.dir, "quiet"), strconv.FormatInt(until, 10)+"\n", 0o644)
	var out bytes.Buffer
	runDoctor(context.Background(), env, &out)
	for _, want := range []string{
		"\nnote: no heartbeat URL is set: ",
		"\nnote: the watchdog mails nothing until " + time.Unix(until, 0).UTC().Format("2006-01-02 15:04 UTC") + " (" + filepath.Join(h.dir, "quiet") + ")",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}

	writeTestFile(t, filepath.Join(h.dir, "watchdog-heartbeat-url"), "not a url\n", 0o600)
	out.Reset()
	runDoctor(context.Background(), env, &out)
	if !strings.Contains(out.String(), "warning  watchdog/heartbeat: the heartbeat URL is unusable") {
		t.Errorf("an unusable heartbeat URL is not reported:\n%s", out.String())
	}
}

// Without the watchdog's unit the doctor still checks, with the watchdog's
// defaults, and says the host has no watchdog; a configuration that does not
// load leaves the checks that need it unchecked and the host's own checks on.
func TestDoctorWithoutUnitOrConfig(t *testing.T) {
	env, _ := doctorHost(t, testWatchdogConfig, "")
	if err := os.Remove(filepath.Join(env.unitDir, "felis-watchdog.service")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(env.unitDir, "felis-watchdog.service"), "[Service]\nExecStart=/usr/local/bin/felis watchdog -config "+filepath.Join(t.TempDir(), "absent.toml")+"\n", 0o644)
	env.run = fakeSystemctl("", map[string]string{"felis-velocity.service": "active", "felis-offsite.timer": "active"})
	var out bytes.Buffer
	if code := runDoctor(context.Background(), env, &out); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	for _, want := range []string{
		"✗  configuration\n     critical config: ",
		"-  Kubernetes cluster: not checked, the configuration did not load\n",
		"-  PostgreSQL: not checked, the configuration did not load\n",
		"-  disk space: not checked, the configuration did not load\n",
		"✓  systemd units and timers\n",
		"-  alerting: not checked, the configuration did not load\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}

	if err := os.Remove(filepath.Join(env.unitDir, "felis-watchdog.service")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	runDoctor(context.Background(), env, &out)
	if !strings.Contains(out.String(), "critical watchdog/unit: "+filepath.Join(env.unitDir, "felis-watchdog.service")+" is not installed") ||
		!strings.Contains(out.String(), "checks run with the watchdog's defaults (config /etc/felis/felis.toml)") {
		t.Errorf("a host without the watchdog's unit:\n%s", out.String())
	}

	unit := filepath.Join(env.unitDir, "felis-watchdog.service")
	writeTestFile(t, unit, "[Service]\nExecStart=/usr/local/bin/felis watchdog -no-such-flag x\n", 0o644)
	out.Reset()
	runDoctor(context.Background(), env, &out)
	if !strings.Contains(out.String(), "critical watchdog/unit: cannot read the watchdog's settings: "+unit+": flag provided but not defined: -no-such-flag\n") ||
		!strings.Contains(out.String(), "checks run with the watchdog's defaults (config /etc/felis/felis.toml)") {
		t.Errorf("a watchdog unit this binary cannot read:\n%s", out.String())
	}
}

func TestPrintDoctorReport(t *testing.T) {
	var out bytes.Buffer
	if code := printDoctorReport(&out, nil, map[string]string{"proxy": "no -proxy-addr"}, nil); code != 0 {
		t.Errorf("exit %d with nothing found, want 0", code)
	}
	want := "✓  configuration\n✓  Kubernetes cluster\n✓  PostgreSQL\n-  game proxy: not checked, no -proxy-addr\n✓  database backups\n✓  off-site copy\n" +
		"✓  build scan database\n✓  disk space\n✓  memory\n✓  k3s certificates\n✓  node address\n✓  clock\n✓  systemd units and timers\n✓  alerting\n" +
		"\nno problems found\n"
	if out.String() != want {
		t.Errorf("report:\n%s\nwant:\n%s", out.String(), want)
	}

	out.Reset()
	code := printDoctorReport(&out, []watchdog.Finding{
		{Key: "unit/systemctl", Severity: watchdog.Warning, SummaryEN: "systemctl list-units failed"},
		{Key: "postgres", Severity: watchdog.Critical, SummaryEN: "PostgreSQL is down", Hint: "kubectl -n felis get pods"},
	}, map[string]string{"postgres": "a skip loses to what was found"}, []string{"a note"})
	if code != 1 {
		t.Errorf("exit %d with problems, want 1", code)
	}
	for _, want := range []string{
		"✗  PostgreSQL\n     critical postgres: PostgreSQL is down\n              → kubectl -n felis get pods\n✓  game proxy\n",
		"!  systemd units and timers\n     warning  unit/systemctl: systemctl list-units failed\n✓  alerting\n",
		"✓  alerting\n\nnote: a note\n\n2 problem(s): 1 critical, 1 warning(s)\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}
