package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/offsite"
	"felis.lolicon.best/internal/watchdog"
)

// TestProxyFinding: a listening proxy is healthy; a closed port is the critical
// "players cannot reach any server" finding.
func TestProxyFinding(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if f := proxyFinding(context.Background(), addr); f != nil {
		t.Fatalf("listening proxy reported: %+v", f)
	}
	ln.Close()
	f := proxyFinding(context.Background(), addr)
	if f == nil || f.Key != "proxy" || !strings.Contains(f.SummaryEN, addr) {
		t.Fatalf("closed proxy = %+v, want the proxy finding", f)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" /, /var/lib/felis ,,")
	if strings.Join(got, "|") != "/|/var/lib/felis" {
		t.Fatalf("splitList = %q", got)
	}
}

// TestMailHold: mail waits through the installer's quiet window, and on a host
// standing by for another host's off-site bucket while that host writes it.
func TestMailHold(t *testing.T) {
	dir := t.TempDir()
	quiet, status := filepath.Join(dir, "quiet"), filepath.Join(dir, "status.json")
	now := time.Now()
	if got := mailHold(quiet, true, status, now); got != "" {
		t.Errorf("no quiet file, no status: %q", got)
	}
	writeTestFile(t, quiet, fmt.Sprintf("%d\n", now.Add(time.Hour).Unix()), 0o644)
	if got := mailHold(quiet, false, status, now); !strings.Contains(got, "quiet until") {
		t.Errorf("inside the quiet window: %q", got)
	}
	os.Remove(quiet)

	w := &offsite.Writer{HostID: "bbbbbbbbbbbbbbbb", Host: "prod-1", At: now.Add(-40 * time.Minute)}
	for _, tc := range []struct {
		what      string
		st        offsite.Status
		offsiteOn bool
		held      bool
	}{
		{"standing by for a live writer", offsite.Status{Standby: true, Writer: w}, true, true},
		{"standing by, [offsite] since removed", offsite.Status{Standby: true, Writer: w}, false, false},
		{"standing by for a writer gone quiet", offsite.Status{Standby: true, Writer: &offsite.Writer{HostID: w.HostID, Host: w.Host, At: now.Add(-offsite.WriterLive - time.Minute)}}, true, false},
		{"standing by, no writer named", offsite.Status{Standby: true}, true, false},
		{"displaced", offsite.Status{Displaced: true, Writer: w}, true, false},
		{"the writer itself", offsite.Status{LastSuccess: now}, true, false},
	} {
		if err := offsite.WriteStatus(status, tc.st); err != nil {
			t.Fatal(err)
		}
		got := mailHold(quiet, tc.offsiteOn, status, now)
		if (got != "") != tc.held || (tc.held && !strings.Contains(got, "stands by for host prod-1 (id bbbbbbbbbbbbbbbb)")) {
			t.Errorf("%s: hold = %q, want held %v", tc.what, got, tc.held)
		}
	}
	writeTestFile(t, status, "{", 0o600)
	if got := mailHold(quiet, true, status, now); got != "" {
		t.Errorf("an unreadable status held the mail: %q", got)
	}
}

// watchdogHost is a host whole watchdog passes run on: the API server and PostgreSQL
// are down (no kubeconfig, nothing on the database port), the relay is a recorder and
// the heartbeat URL points at a ping log.
type watchdogHost struct {
	dir, statePath string
	// fallbackPath stands in for /run/felis, in a directory of its own.
	fallbackPath string
	args         []string
	rec          *alertRecorder
	pings        *pingLog
	url          string
}

func newWatchdogHost(t *testing.T, cfg string, state *watchdog.State) *watchdogHost {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KUBECONFIG", filepath.Join(dir, "no-kubeconfig"))
	pings, srv := newPingServer(t)
	h := &watchdogHost{dir: dir, statePath: filepath.Join(dir, "state.json"), fallbackPath: filepath.Join(t.TempDir(), "watchdog-state.json"),
		rec: &alertRecorder{}, pings: pings, url: srv.URL}
	writeTestFile(t, filepath.Join(dir, "felis.toml"), cfg, 0o600)
	writeTestFile(t, filepath.Join(dir, "watchdog-heartbeat-url"), srv.URL+"/check-key\n", 0o600)
	if state != nil {
		if err := watchdog.SaveState(h.statePath, state); err != nil {
			t.Fatal(err)
		}
	}
	h.args = []string{
		"-config", filepath.Join(dir, "felis.toml"), "-state", h.statePath, "-fallback-state", h.fallbackPath, "-quiet-file", filepath.Join(dir, "quiet"),
		"-backup-dir", "", "-disk-paths", dir, "-k3s-cert-dirs", "", "-smtp-password-file", filepath.Join(dir, "smtp-password"),
		"-offsite-status", filepath.Join(dir, "offsite-status.json"), "-build-tools-status", filepath.Join(dir, "build-tools.json"),
		"-heartbeat-file", filepath.Join(dir, "watchdog-heartbeat-url"),
	}
	return h
}

// run is one pass; flags given here override the host's own.
func (h *watchdogHost) run(flags ...string) (code int, stdout, stderr string) {
	watchdogSender = h.rec.sender
	defer func() { watchdogSender = smtpSender }()
	var out, errOut bytes.Buffer
	code = cmdWatchdog(append(append([]string(nil), h.args...), flags...), &out, &errOut)
	return code, out.String(), errOut.String()
}

// duePostgres is a state whose owner has not yet been told of PostgreSQL, down
// for an hour.
func duePostgres(cachedPassword string) *watchdog.State {
	s := &watchdog.State{Recipients: []string{"owner@example.com"}, SMTPPassword: cachedPassword}
	s.Observe(watchdog.Report{Findings: []watchdog.Finding{watchdog.PostgresDown(errors.New("refused"))}}, time.Now().Add(-time.Hour))
	return s
}

// TestWatchdogRunKeepsClusterAlertsWhileTheAPIIsDown: with the API server down,
// the alerts under the cluster checks keep their state, since nothing looked
// at them; an alert of a check that did run and found nothing reads as cleared.
func TestWatchdogRunKeepsClusterAlertsWhileTheAPIIsDown(t *testing.T) {
	told := time.Now().Add(-30 * time.Minute)
	cluster := []string{"deployment/felis-api", "node/felis-1/NotReady", "server-failed/lobby"}
	var r watchdog.Report
	for _, key := range append([]string{"proxy"}, cluster...) {
		r.Findings = append(r.Findings, watchdog.Finding{Key: key, Severity: watchdog.Critical, SummaryEN: key + " is down"})
	}
	s := &watchdog.State{Recipients: []string{"owner@example.com"}}
	s.Commit(s.Observe(r, told), told)
	h := newWatchdogHost(t, testWatchdogConfig, s)

	code, stdout, stderr := h.run()
	if code != 0 || len(h.rec.relays) != 0 {
		t.Fatalf("exit %d, mailed %v\nstdout %s\nstderr %s", code, h.rec.sent, stdout, stderr)
	}
	for _, want := range []string{"felis watchdog: [critical] kube-api: ", "felis watchdog: [critical] postgres: "} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	got, err := watchdog.LoadState(h.statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range cluster {
		if a := got.Alerts[key]; a == nil || !a.ClearedAt.IsZero() || !a.Notified.Equal(told) {
			t.Errorf("%s with the API server down: %+v, want it kept open as mailed at %s", key, a, told)
		}
	}
	if a := got.Alerts["proxy"]; a == nil || a.ClearedAt.IsZero() {
		t.Errorf("proxy, whose check ran and found nothing: %+v, want it cleared", a)
	}
}

// TestWatchdogDryRunMailsAndSavesNothing: a dry run prints the mail that is due
// and the heartbeat it would ping, and sends, pings and saves nothing.
func TestWatchdogDryRunMailsAndSavesNothing(t *testing.T) {
	t.Setenv("FELIS_TEST_WATCHDOG_RELAY_PW", "env-pw")
	h := newWatchdogHost(t, testWatchdogConfig+testWatchdogSMTP, duePostgres(""))
	before, err := os.ReadFile(h.statePath)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := h.run("-dry-run")
	after, err := os.ReadFile(h.statePath)
	if err != nil {
		t.Fatal(err)
	}
	pings, _ := h.pings.got()
	if code != 0 || len(h.rec.relays) != 0 || len(pings) != 0 || !bytes.Equal(before, after) {
		t.Errorf("dry run: exit %d, relays %d, pings %v, state changed %v\nstdout %s\nstderr %s", code, len(h.rec.relays), pings, !bytes.Equal(before, after), stdout, stderr)
	}
	host, _ := os.Hostname()
	for _, want := range []string{
		"felis watchdog: due to be mailed to owner@example.com:\nSubject: Felis 严重告警（" + host + "）：1 项异常 · 1 firing\n\n",
		"felis watchdog: a run pings the heartbeat at " + h.url + "/...\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "\r") {
		t.Errorf("the mail body printed with CRLF:\n%q", stdout)
	}

	h = newWatchdogHost(t, testWatchdogConfig, nil)
	code, stdout, stderr = h.run("-dry-run")
	if code != 0 || !strings.Contains(stdout, "felis watchdog: nothing is due to be mailed\n") {
		t.Errorf("dry run with nothing due: exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	if _, err := os.Stat(h.statePath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a dry run wrote the state (%v)", err)
	}
}

// TestWatchdogRunSignsInWithTheHostRelayPassword: a pass caches the relay
// password from the host copy even while the cluster is down, and signs in with
// it; with no host copy and no cluster it keeps the one it had.
func TestWatchdogRunSignsInWithTheHostRelayPassword(t *testing.T) {
	const smtpNoRef = "[smtp]\nhost = \"smtp.config.example\"\nport = 2525\nfrom = \"felis@example.com\"\nusername = \"felis\"\n"
	for _, tc := range []struct{ what, hostCopy, want string }{
		{"the host copy", "host-pw", "host-pw"},
		{"no host copy, the cluster down", "", "cached-pw"},
	} {
		h := newWatchdogHost(t, testWatchdogConfig+smtpNoRef, duePostgres("cached-pw"))
		if tc.hostCopy != "" {
			writeTestFile(t, filepath.Join(h.dir, "smtp-password"), tc.hostCopy, 0o600)
		}
		code, stdout, stderr := h.run()
		if code != 0 || len(h.rec.relays) != 1 || h.rec.relays[0].Password != tc.want {
			t.Errorf("%s: exit %d, relays %+v; want one mail signed in with %q\nstdout %s\nstderr %s", tc.what, code, h.rec.relays, tc.want, stdout, stderr)
			continue
		}
		if s, err := watchdog.LoadState(h.statePath); err != nil || s.SMTPPassword != tc.want {
			t.Errorf("%s: the state caches another password (%v)", tc.what, err)
		}
	}
}

// TestWatchdogRunChecksWhatIsConfigured: the proxy, the database backups, the
// off-site copy and the build lane's scan DB are each checked when the host
// has them, and only then.
func TestWatchdogRunChecksWhatIsConfigured(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	ln.Close()
	const offsiteTable = "[offsite]\nendpoint = \"https://s3.example.com\"\nbucket = \"felis\"\n"
	const registry = "[registry]\nurl = \"registry.felis.svc:5000\"\n"
	optional := []string{"proxy", "db-backup", "offsite", "scan-db"}
	for _, tc := range []struct {
		what  string
		cfg   string
		flags func(dir string) []string
		want  string // the one optional check that reports, "" for none
	}{
		{what: "none of them", cfg: testWatchdogConfig},
		{what: "a proxy address", cfg: testWatchdogConfig, flags: func(string) []string { return []string{"-proxy-addr", closed} }, want: "proxy"},
		{what: "a backup directory", cfg: testWatchdogConfig, flags: func(dir string) []string { return []string{"-backup-dir", dir} }, want: "db-backup"},
		{what: "[offsite]", cfg: testWatchdogConfig + offsiteTable, want: "offsite"},
		{what: "a registry with the default scan DB", cfg: testWatchdogConfig + registry, want: "scan-db"},
		{what: "a registry with its scan DB under mirror/", cfg: testWatchdogConfig + registry + "trivy_db_repository = \"registry.felis.svc:5000/mirror/trivy-db\"\n", want: "scan-db"},
		{what: "a registry with the scan DB elsewhere", cfg: testWatchdogConfig + registry + "trivy_db_repository = \"ghcr.io/aquasecurity/trivy-db\"\n"},
	} {
		h := newWatchdogHost(t, tc.cfg, nil)
		var flags []string
		if tc.flags != nil {
			flags = tc.flags(t.TempDir())
		}
		code, stdout, stderr := h.run(append(flags, "-dry-run")...)
		var reported []string
		for _, key := range optional {
			if strings.Contains(stdout, "] "+key+": ") {
				reported = append(reported, key)
			}
		}
		if code != 0 || strings.Join(reported, ",") != tc.want {
			t.Errorf("%s: exit %d, reported %v; want %q\nstdout %s\nstderr %s", tc.what, code, reported, tc.want, stdout, stderr)
		}
	}
}

// TestWatchdogRunStateThatDoesNotSave: a pass whose state does not save mails
// as usual, keeps its state in the fallback, exits 1 and posts the failure to
// the heartbeat. The next pass reads the fallback and mails nothing again; the
// first one whose state saves drops the fallback.
func TestWatchdogRunStateThatDoesNotSave(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	t.Setenv("FELIS_TEST_WATCHDOG_RELAY_PW", "env-pw")
	h := newWatchdogHost(t, testWatchdogConfig+testWatchdogSMTP, duePostgres(""))
	if err := os.Chmod(h.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(h.dir, 0o700) })
	kept := "; kept in " + h.fallbackPath + " until the host restarts"
	for i := range 2 {
		code, stdout, stderr := h.run()
		pings, bodies := h.pings.got()
		if code != 1 || len(h.rec.sent) != 1 || !strings.Contains(stderr, "felis watchdog: save state: ") || !strings.Contains(stderr, kept) ||
			len(pings) != i+1 || pings[i] != "POST /check-key/fail" || !strings.HasPrefix(bodies[i], "the watchdog state did not save: ") {
			t.Fatalf("pass %d: exit %d, mailed %v, pings %v %q; want exit 1, one mail in all and the save failure posted to /fail\nstdout %s\nstderr %s", i, code, h.rec.sent, pings, bodies, stdout, stderr)
		}
	}

	if err := os.Chmod(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := h.run()
	if _, err := os.Stat(h.fallbackPath); code != 0 || len(h.rec.sent) != 1 || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("once the state saves: exit %d, mailed %v, fallback %v; want exit 0, no new mail, the fallback gone\nstdout %s\nstderr %s", code, h.rec.sent, err, stdout, stderr)
	}
	if s, err := watchdog.LoadState(h.statePath); err != nil || s.Alerts["postgres"] == nil || s.Alerts["postgres"].Notified.IsZero() {
		t.Fatalf("saved state = %+v, %v; want the mailed PostgreSQL alert", s, err)
	}
}
