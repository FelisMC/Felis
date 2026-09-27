package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/mail"
	"felis.lolicon.best/internal/offsite"
	"felis.lolicon.best/internal/watchdog"
)

// pingLog is a monitoring service that records every ping it gets.
type pingLog struct {
	mu     sync.Mutex
	pings  []string // "METHOD path?query"
	bodies []string
	status int
}

func newPingServer(t *testing.T) (*pingLog, *httptest.Server) {
	t.Helper()
	l := &pingLog{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		defer l.mu.Unlock()
		p := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			p += "?" + r.URL.RawQuery
		}
		l.pings = append(l.pings, p)
		l.bodies = append(l.bodies, string(body))
		w.WriteHeader(l.status)
	}))
	t.Cleanup(srv.Close)
	return l, srv
}

func (l *pingLog) got() ([]string, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.pings...), append([]string(nil), l.bodies...)
}

// TestHeartbeatSend: a run whose alerts reach the owners GETs the URL, one
// whose alerts reach no one POSTs its report to /fail, and a standby host, a
// failure in the installer's quiet window and a query URL that has no /fail
// send nothing.
func TestHeartbeatSend(t *testing.T) {
	const key = "/ping/5f1e0c2a-check-key"
	for _, tc := range []struct {
		what     string
		beat     heartbeat
		path     string // appended to the server URL
		want     string // the ping, "" for none
		wantBody string
		wantOut  string
	}{
		{what: "success", beat: heartbeat{}, path: key, want: "GET " + key},
		{what: "failure", beat: heartbeat{fail: true, report: "the alerts reach no one: x"}, path: key, want: "POST " + key + "/fail", wantBody: "the alerts reach no one: x", wantOut: "pinged the heartbeat's failure endpoint"},
		{what: "failure, URL with a trailing slash", beat: heartbeat{fail: true, report: "r"}, path: key + "/", want: "POST " + key + "/fail", wantBody: "r"},
		{what: "success, URL with a query", beat: heartbeat{}, path: key + "?rid=7", want: "GET " + key + "?rid=7"},
		{what: "failure, URL with a query", beat: heartbeat{fail: true, report: "r"}, path: key + "?rid=7", wantOut: "withholding the heartbeat ping"},
		{what: "standby, success", beat: heartbeat{standby: true}, path: key, wantOut: "stands by for the off-site bucket"},
		{what: "standby, failure", beat: heartbeat{standby: true, fail: true}, path: key, wantOut: "stands by for the off-site bucket"},
		{what: "quiet, failure", beat: heartbeat{quiet: true, fail: true}, path: key, wantOut: "withholding the failure ping"},
		{what: "quiet, success", beat: heartbeat{quiet: true}, path: key, want: "GET " + key},
	} {
		log, srv := newPingServer(t)
		b := tc.beat
		b.url = srv.URL + tc.path
		var stdout, stderr bytes.Buffer
		b.send(srv.Client(), &stdout, &stderr)
		pings, bodies := log.got()
		switch {
		case tc.want == "" && len(pings) != 0:
			t.Errorf("%s: pinged %v, want nothing", tc.what, pings)
		case tc.want != "" && (len(pings) != 1 || pings[0] != tc.want):
			t.Errorf("%s: pinged %v, want %q", tc.what, pings, tc.want)
		case tc.want != "" && bodies[0] != tc.wantBody:
			t.Errorf("%s: body %q, want %q", tc.what, bodies[0], tc.wantBody)
		}
		if !strings.Contains(stdout.String(), tc.wantOut) || stderr.Len() != 0 {
			t.Errorf("%s: stdout %q (want %q), stderr %q", tc.what, stdout.String(), tc.wantOut, stderr.String())
		}
	}
}

// TestHeartbeatErrors: a ping the service refuses or that cannot connect is
// logged without the URL's path, the check's key.
func TestHeartbeatErrors(t *testing.T) {
	const key = "5f1e0c2a-check-key"
	log, srv := newPingServer(t)
	log.status = http.StatusNotFound
	var stdout, stderr bytes.Buffer
	heartbeat{url: srv.URL + "/" + key}.send(srv.Client(), &stdout, &stderr)
	if got := stderr.String(); !strings.Contains(got, "heartbeat: GET http://127.0.0.1") || !strings.Contains(got, "404") || strings.Contains(got, key) {
		t.Errorf("refused ping: stderr %q; want the host and status, not the key", got)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	dead := closed.URL + "/" + key
	closed.Close()
	stderr.Reset()
	heartbeat{url: dead, fail: true, report: "r"}.send(http.DefaultClient, &stdout, &stderr)
	if got := stderr.String(); !strings.Contains(got, "heartbeat: POST http://127.0.0.1") || strings.Contains(got, key) {
		t.Errorf("unreachable service: stderr %q; want the host, not the key", got)
	}
}

// TestHeartbeatReportClipped: a long report is cut to what the service keeps,
// on a character boundary.
func TestHeartbeatReportClipped(t *testing.T) {
	log, srv := newPingServer(t)
	report := strings.Repeat("磁盘", heartbeatReportMax)
	heartbeat{url: srv.URL + "/k", fail: true, report: report}.send(srv.Client(), io.Discard, io.Discard)
	_, bodies := log.got()
	if len(bodies) != 1 || len(bodies[0]) > heartbeatReportMax || len(bodies[0]) < heartbeatReportMax-3 || !utf8.ValidString(bodies[0]) {
		t.Fatalf("clipped body: %d pings, %d bytes, valid UTF-8 %v", len(bodies), len(bodies[0]), utf8.ValidString(bodies[0]))
	}
	if got := clipUTF8("short", heartbeatReportMax); got != "short" {
		t.Errorf("clipUTF8(short) = %q", got)
	}
}

func TestReadHeartbeatURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watchdog-heartbeat-url")
	if u, err := readHeartbeatURL(path); u != "" || err != nil {
		t.Fatalf("no file: %q, %v", u, err)
	}
	if u, err := readHeartbeatURL(""); u != "" || err != nil {
		t.Fatalf("no path: %q, %v", u, err)
	}
	writeTestFile(t, path, "https://hc-ping.com/5f1e0c2a\n", 0o600)
	if u, err := readHeartbeatURL(path); u != "https://hc-ping.com/5f1e0c2a" || err != nil {
		t.Fatalf("good file: %q, %v", u, err)
	}
	for _, bad := range []string{"", "hc-ping.com/5f1e0c2a", "ftp://hc-ping.com/5f1e0c2a", "https:///5f1e0c2a", "https://hc-ping.com/5f1e 0c2a"} {
		writeTestFile(t, path, bad, 0o600)
		u, err := readHeartbeatURL(path)
		if u != "" || err == nil || !strings.Contains(err.Error(), path) || (bad != "" && strings.Contains(err.Error(), "5f1e")) {
			t.Errorf("%q: %q, %v; want an error naming the file and not the URL", bad, u, err)
		}
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://hc-ping.com/5f1e0c2a":             "https://hc-ping.com/...",
		"http://user:pw@status.example:8080/k?x=1": "http://status.example:8080/...",
		"not a url": "(the heartbeat URL)",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStandsBy: a standby record keeps the host from pinging however old it
// is; a displaced host, the writer, or a host without [offsite] pings.
func TestStandsBy(t *testing.T) {
	status := filepath.Join(t.TempDir(), "status.json")
	if standsBy(true, status) {
		t.Fatal("no status file stands by")
	}
	old := &offsite.Writer{HostID: "bbbbbbbbbbbbbbbb", Host: "prod-1", At: time.Now().Add(-30 * 24 * time.Hour)}
	for _, tc := range []struct {
		what      string
		st        offsite.Status
		offsiteOn bool
		want      bool
	}{
		{"standing by for a writer gone quiet a month ago", offsite.Status{Standby: true, Writer: old}, true, true},
		{"standing by, [offsite] since removed", offsite.Status{Standby: true, Writer: old}, false, false},
		{"displaced", offsite.Status{Displaced: true, Writer: old}, true, false},
		{"the writer itself", offsite.Status{LastSuccess: time.Now()}, true, false},
	} {
		if err := offsite.WriteStatus(status, tc.st); err != nil {
			t.Fatal(err)
		}
		if got := standsBy(tc.offsiteOn, status); got != tc.want {
			t.Errorf("%s: standsBy = %v, want %v", tc.what, got, tc.want)
		}
	}
}

func TestFailureReport(t *testing.T) {
	r := watchdog.Report{Findings: []watchdog.Finding{{Key: "postgres", Severity: watchdog.Critical, SummaryEN: "PostgreSQL is down"}}}
	for _, tc := range []struct {
		what       string
		unheard    string
		mailFailed bool
		saveErr    error
		open       bool
		want       string // "" for a success ping
	}{
		{what: "all good", open: true},
		{what: "no relay, nothing mailed yet", unheard: "no [smtp] relay is configured"},
		{what: "no relay, an alert open", unheard: "no [smtp] relay is configured", open: true, want: "the alerts reach no one: no [smtp] relay is configured"},
		{what: "the mail failed", unheard: "the alert mail failed: refused", mailFailed: true, want: "the alerts reach no one: the alert mail failed: refused"},
		{what: "the state did not save", saveErr: errors.New("disk full"), want: "the watchdog state did not save: disk full"},
	} {
		got := failureReport(tc.unheard, tc.mailFailed, tc.saveErr, tc.open, r)
		if (got == "") != (tc.want == "") || !strings.Contains(got, tc.want) || (got != "" && !strings.Contains(got, "[critical] postgres: PostgreSQL is down")) {
			t.Errorf("%s: report %q, want %q and the findings", tc.what, got, tc.want)
		}
	}
}

// alertRecorder records the mail a run hands the relay.
type alertRecorder struct {
	relays []*mail.SMTP
	sent   []string // "to: subject"
	fail   map[string]bool
}

func (f *alertRecorder) sender(relay *mail.SMTP) alertSender {
	f.relays = append(f.relays, relay)
	return func(_ context.Context, to, subject, _ string) error {
		if f.fail[to] {
			return errors.New("550 refused")
		}
		f.sent = append(f.sent, to+": "+subject)
		return nil
	}
}

// TestMailerDeliver: a plan goes out and is committed; held, it waits
// uncommitted; with no relay or recipient it is logged and committed; a mail
// no recipient got is left uncommitted to come due again.
func TestMailerDeliver(t *testing.T) {
	now := time.Now()
	relay := &watchdog.Relay{Host: "smtp.example.com", Port: 587, From: "felis@example.com", Username: "felis"}
	for _, tc := range []struct {
		what        string
		relay       *watchdog.Relay
		recipients  []string
		hold        string
		fail        map[string]bool
		wantUnheard string
		wantFailed  bool
		committed   bool
		sent        int
	}{
		{what: "mailed", relay: relay, recipients: []string{"a@example.com", "b@example.com"}, committed: true, sent: 2},
		{what: "one recipient refused", relay: relay, recipients: []string{"a@example.com", "b@example.com"}, fail: map[string]bool{"a@example.com": true}, committed: true, sent: 1},
		{what: "every recipient refused", relay: relay, recipients: []string{"a@example.com"}, fail: map[string]bool{"a@example.com": true}, wantUnheard: "the alert mail failed", wantFailed: true},
		{what: "held", relay: relay, recipients: []string{"a@example.com"}, hold: "quiet until later"},
		{what: "no relay", recipients: []string{"a@example.com"}, wantUnheard: "no [smtp] relay is configured", committed: true},
		{what: "no recipient", relay: relay, wantUnheard: "no owner account has a verified email", committed: true},
	} {
		state := &watchdog.State{Recipients: tc.recipients}
		plan := state.Observe(watchdog.Report{Findings: []watchdog.Finding{{Key: "postgres", Severity: watchdog.Critical, SummaryEN: "down"}}}, now)
		f := &alertRecorder{fail: tc.fail}
		m := mailer{relay: tc.relay, password: "relay-pw", send: f.sender}
		var stdout, stderr bytes.Buffer
		unheard, failed := m.deliver(context.Background(), state, plan, "subject", "body", tc.hold, now, &stdout, &stderr)
		if !strings.HasPrefix(unheard, tc.wantUnheard) || (tc.wantUnheard == "") != (unheard == "") || failed != tc.wantFailed {
			t.Errorf("%s: unheard %q, failed %v; want %q, %v", tc.what, unheard, failed, tc.wantUnheard, tc.wantFailed)
		}
		if got := !state.Alerts["postgres"].Notified.IsZero(); got != tc.committed {
			t.Errorf("%s: committed %v, want %v", tc.what, got, tc.committed)
		}
		if len(f.sent) != tc.sent {
			t.Errorf("%s: sent %v, want %d mails", tc.what, f.sent, tc.sent)
		}
		if len(f.relays) > 0 {
			if got := *f.relays[0]; got != (mail.SMTP{Host: "smtp.example.com", Port: 587, From: "felis@example.com", Username: "felis", Password: "relay-pw"}) {
				t.Errorf("%s: relay %+v", tc.what, got)
			}
		}
	}
}

// TestCachedRelay: the cache keeps the relay's coordinates and its TLS rule,
// and nothing when no relay is configured.
func TestCachedRelay(t *testing.T) {
	if r := cachedRelay(testSMTPConfig("")); r != nil {
		t.Fatalf("no relay cached %+v", r)
	}
	got := cachedRelay(testSMTPConfig("smtp.example.com"))
	if got == nil || *got != (watchdog.Relay{Host: "smtp.example.com", Port: 2525, From: "felis@example.com", Username: "felis", RequireTLS: true}) {
		t.Fatalf("cachedRelay = %+v", got)
	}
	if got := cachedRelay(testSMTPConfig("127.0.0.1")); got == nil || got.RequireTLS {
		t.Fatalf("a relay on this host: %+v, want TLS not required", got)
	}
}

func testSMTPConfig(host string) config.SMTPConfig {
	if host == "" {
		return config.SMTPConfig{}
	}
	return config.SMTPConfig{Host: host, Port: 2525, From: "felis@example.com", Username: "felis"}
}

// TestSMTPPassword: the env var password_ref names wins over the cache once
// it is set.
func TestSMTPPassword(t *testing.T) {
	c := testSMTPConfig("smtp.example.com")
	c.PasswordRef = "FELIS_TEST_WATCHDOG_RELAY_PW"
	t.Setenv(c.PasswordRef, "")
	if got := smtpPassword(c, "cached"); got != "cached" {
		t.Errorf("env var unset: %q", got)
	}
	t.Setenv(c.PasswordRef, "from-env")
	if got := smtpPassword(c, "cached"); got != "from-env" {
		t.Errorf("env var set: %q", got)
	}
}

const testWatchdogConfig = `[database]
url = "postgres://felis:pw@127.0.0.1:1/felis?sslmode=disable&connect_timeout=2"
[server]
root_domain = "example.com"
[archive]
store = "tarLocal"
[k8s]
egress_mode = "nodeport"
`

const testWatchdogSMTP = `[smtp]
host = "smtp.config.example"
port = 2525
from = "felis@example.com"
username = "felis"
password_ref = "FELIS_TEST_WATCHDOG_RELAY_PW"
`

// unitFailedFixture is a host whose watchdog last ran well: one alert open,
// one owner and the relay cached.
func unitFailedFixture(t *testing.T, cfg string) (unitFailedRun, *alertRecorder, *pingLog) {
	t.Helper()
	dir := t.TempDir()
	log, srv := newPingServer(t)
	beatFile := filepath.Join(dir, "watchdog-heartbeat-url")
	writeTestFile(t, beatFile, srv.URL+"/check-key\n", 0o600)
	cfgPath := filepath.Join(dir, "felis.toml")
	writeTestFile(t, cfgPath, cfg, 0o600)
	statePath := filepath.Join(dir, "state.json")
	state := &watchdog.State{
		Recipients:   []string{"owner@example.com"},
		SMTPPassword: "cached-pw",
		Relay:        &watchdog.Relay{Host: "smtp.cached.example", Port: 587, From: "felis@example.com", RequireTLS: true},
	}
	mem := watchdog.Finding{Key: "memory", Severity: watchdog.Warning, SummaryEN: "memory low"}
	state.Commit(state.Observe(watchdog.Report{Findings: []watchdog.Finding{mem}}, time.Now().Add(-time.Hour)), time.Now().Add(-time.Hour))
	if err := watchdog.SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	f := &alertRecorder{}
	return unitFailedRun{
		cfgPath: cfgPath, statePath: statePath, quietPath: filepath.Join(dir, "quiet"),
		offsiteStatus: filepath.Join(dir, "offsite-status.json"), heartbeatFile: beatFile,
		result: "exit-code", exitStatus: "1", send: f.sender, client: srv.Client(), now: time.Now(),
	}, f, log
}

// TestWatchdogUnitFailedBrokenConfig: failed runs of a watchdog whose
// felis.toml no longer loads are mailed through the relay the last good run
// cached, after five in a row; every one pings /fail; the open alert keeps
// its state.
func TestWatchdogUnitFailedBrokenConfig(t *testing.T) {
	r, f, log := unitFailedFixture(t, "[database\n")
	start := r.now
	for i := 0; i <= 5; i++ {
		r.now = start.Add(time.Duration(i) * 2 * time.Minute)
		var stdout, stderr bytes.Buffer
		if code := watchdogUnitFailed(r, &stdout, &stderr); code != 0 {
			t.Fatalf("run %d: exit %d (stdout %s, stderr %s)", i, code, stdout.String(), stderr.String())
		}
		if i < 5 && len(f.sent) != 0 {
			t.Fatalf("run %d, %v after the first failure: mailed %v, want nothing yet", i, r.now.Sub(start), f.sent)
		}
	}
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "owner@example.com: ") {
		t.Fatalf("mailed %v, want one alert to the cached owner", f.sent)
	}
	if got := *f.relays[0]; got.Host != "smtp.cached.example" || got.Password != "cached-pw" || !got.RequireTLS {
		t.Fatalf("relay %+v, want the cached one", got)
	}
	pings, bodies := log.got()
	if len(pings) != 6 || pings[0] != "POST /check-key/fail" || !strings.Contains(bodies[0], "felis-watchdog.service failed: result exit-code, exit status 1; ") {
		t.Fatalf("pings %v, bodies %q", pings, bodies)
	}
	state, err := watchdog.LoadState(r.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if a := state.Alerts["watchdog/run"]; a == nil || a.Notified.IsZero() || !strings.Contains(a.SummaryEN, "result exit-code, exit status 1") {
		t.Fatalf("watchdog/run alert = %+v", a)
	}
	if a := state.Alerts["memory"]; a == nil || !a.ClearedAt.IsZero() || !a.Notified.Before(start) {
		t.Fatalf("memory alert = %+v, want it untouched", a)
	}
}

// TestWatchdogUnitFailedConfigRelay: with felis.toml loading, the relay is
// the configured one and signs in with the env var password_ref names.
func TestWatchdogUnitFailedConfigRelay(t *testing.T) {
	r, f, _ := unitFailedFixture(t, testWatchdogConfig+testWatchdogSMTP)
	t.Setenv("FELIS_TEST_WATCHDOG_RELAY_PW", "env-pw")
	start := r.now
	for _, at := range []time.Duration{0, 10 * time.Minute} {
		r.now = start.Add(at)
		var stdout, stderr bytes.Buffer
		if code := watchdogUnitFailed(r, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d (stdout %s, stderr %s)", code, stdout.String(), stderr.String())
		}
	}
	if len(f.relays) != 1 || f.relays[0].Host != "smtp.config.example" || f.relays[0].Port != 2525 || f.relays[0].Password != "env-pw" {
		t.Fatalf("relays %+v, want the configured one with the env password", f.relays)
	}
}

// TestWatchdogUnitFailedHeld: in the installer's quiet window the alert waits
// and no failure is pinged; a host standing by for the off-site bucket pings
// nothing either.
func TestWatchdogUnitFailedHeld(t *testing.T) {
	r, f, log := unitFailedFixture(t, "[database\n")
	writeTestFile(t, r.quietPath, fmt.Sprintf("%d\n", r.now.Add(time.Hour).Unix()), 0o644)
	start := r.now
	for _, at := range []time.Duration{0, 10 * time.Minute} {
		r.now = start.Add(at)
		watchdogUnitFailed(r, io.Discard, io.Discard)
	}
	if pings, _ := log.got(); len(f.sent) != 0 || len(pings) != 0 {
		t.Fatalf("quiet window: mailed %v, pinged %v", f.sent, pings)
	}

	os.Remove(r.quietPath)
	w := &offsite.Writer{HostID: "bbbbbbbbbbbbbbbb", Host: "prod-1", At: r.now.Add(-time.Hour)}
	if err := offsite.WriteStatus(r.offsiteStatus, offsite.Status{Standby: true, Writer: w}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	watchdogUnitFailed(r, &stdout, io.Discard)
	if pings, _ := log.got(); len(f.sent) != 0 || len(pings) != 0 || !strings.Contains(stdout.String(), "stands by for host prod-1") {
		t.Fatalf("standby: mailed %v, pinged %v, stdout %s", f.sent, pings, stdout.String())
	}
}

// TestWatchdogUnitFailedStateUnreadable: with no state to mail from, the
// failure still reaches the heartbeat.
func TestWatchdogUnitFailedStateUnreadable(t *testing.T) {
	r, f, log := unitFailedFixture(t, "[database\n")
	writeTestFile(t, r.statePath, "{", 0o600)
	if code := watchdogUnitFailed(r, io.Discard, io.Discard); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	pings, bodies := log.got()
	if len(f.sent) != 0 || len(pings) != 1 || pings[0] != "POST /check-key/fail" || !strings.Contains(bodies[0], "state does not load") {
		t.Fatalf("mailed %v, pinged %v %q", f.sent, pings, bodies)
	}
}

func TestFailureDetail(t *testing.T) {
	for _, tc := range [][3]string{
		{"exit-code", "1", "result exit-code, exit status 1"},
		{"timeout", "", "result timeout"},
		{"", "", "systemd named no cause (journalctl -u felis-watchdog -n 50)"},
	} {
		if got := failureDetail(tc[0], tc[1]); got != tc[2] {
			t.Errorf("failureDetail(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}

// TestWatchdogRunHeartbeat runs whole watchdog passes with the API server and
// PostgreSQL down and the relay a recorder: a pass whose alerts reach the
// owners pings success; one whose mail fails, or that has no relay while an
// alert is open, posts its report to /fail; a host standing by for the
// off-site bucket pings nothing; a state file that does not parse is moved
// aside and reported.
func TestWatchdogRunHeartbeat(t *testing.T) {
	due := func(statePath string) {
		// PostgreSQL has been down for an hour and nobody was told yet.
		s := &watchdog.State{Recipients: []string{"owner@example.com"}, SMTPPassword: "cached-pw"}
		s.Observe(watchdog.Report{Findings: []watchdog.Finding{watchdog.PostgresDown(errors.New("refused"))}}, time.Now().Add(-time.Hour))
		if err := watchdog.SaveState(statePath, s); err != nil {
			t.Fatal(err)
		}
	}
	open := func(statePath string) {
		// The owners were told of PostgreSQL half an hour ago.
		s := &watchdog.State{Recipients: []string{"owner@example.com"}}
		at := time.Now().Add(-30 * time.Minute)
		r := watchdog.Report{Findings: []watchdog.Finding{watchdog.PostgresDown(errors.New("refused"))}}
		s.Observe(r, at.Add(-time.Hour))
		s.Commit(s.Observe(r, at), at)
		if err := watchdog.SaveState(statePath, s); err != nil {
			t.Fatal(err)
		}
	}
	const offsiteTable = "[offsite]\nendpoint = \"https://s3.example.com\"\nbucket = \"felis\"\n"
	t.Setenv("FELIS_TEST_WATCHDOG_RELAY_PW", "env-pw")
	for _, tc := range []struct {
		what     string
		cfg      string
		state    func(path string)
		standby  bool
		quiet    bool
		refused  bool
		wantCode int
		want     string // the ping, "" for none
		wantBody string
		wantOut  string
		wantSent int
	}{
		{what: "nothing due", cfg: testWatchdogConfig + testWatchdogSMTP, want: "GET /check-key"},
		{what: "mailed", cfg: testWatchdogConfig + testWatchdogSMTP, state: due, want: "GET /check-key", wantSent: 1},
		{what: "the mail fails", cfg: testWatchdogConfig + testWatchdogSMTP, state: due, refused: true, wantCode: 1, want: "POST /check-key/fail", wantBody: "the alerts reach no one: the alert mail failed: owner@example.com: 550 refused"},
		{what: "no relay, an alert open", cfg: testWatchdogConfig, state: due, want: "POST /check-key/fail", wantBody: "the alerts reach no one: no [smtp] relay is configured"},
		{what: "no relay, an alert open, the installer running", cfg: testWatchdogConfig, state: open, quiet: true, wantOut: "withholding the failure ping"},
		{what: "standing by for the off-site bucket", cfg: testWatchdogConfig + testWatchdogSMTP + offsiteTable, state: due, standby: true, wantOut: "stands by for the off-site bucket"},
		{what: "a state that does not parse", cfg: testWatchdogConfig, state: func(p string) { writeTestFile(t, p, "{", 0o600) }, want: "GET /check-key", wantOut: "[warning] watchdog/state: the watchdog's state file was unreadable and was moved to "},
	} {
		dir := t.TempDir()
		t.Setenv("KUBECONFIG", filepath.Join(dir, "no-kubeconfig"))
		log, srv := newPingServer(t)
		beatFile := filepath.Join(dir, "watchdog-heartbeat-url")
		writeTestFile(t, beatFile, srv.URL+"/check-key\n", 0o600)
		cfgPath := filepath.Join(dir, "felis.toml")
		writeTestFile(t, cfgPath, tc.cfg, 0o600)
		statePath := filepath.Join(dir, "state.json")
		if tc.state != nil {
			tc.state(statePath)
		}
		statusPath := filepath.Join(dir, "offsite-status.json")
		if tc.standby {
			w := &offsite.Writer{HostID: "bbbbbbbbbbbbbbbb", Host: "prod-1", At: time.Now().Add(-time.Hour)}
			if err := offsite.WriteStatus(statusPath, offsite.Status{Standby: true, Writer: w}); err != nil {
				t.Fatal(err)
			}
		}
		if tc.quiet {
			writeTestFile(t, filepath.Join(dir, "quiet"), fmt.Sprintf("%d\n", time.Now().Add(time.Hour).Unix()), 0o644)
		}
		rec := &alertRecorder{}
		if tc.refused {
			rec.fail = map[string]bool{"owner@example.com": true}
		}
		watchdogSender = rec.sender
		var stdout, stderr bytes.Buffer
		code := cmdWatchdog([]string{
			"-config", cfgPath, "-state", statePath, "-quiet-file", filepath.Join(dir, "quiet"),
			"-backup-dir", "", "-disk-paths", dir, "-smtp-password-file", filepath.Join(dir, "smtp-password"),
			"-offsite-status", statusPath, "-heartbeat-file", beatFile,
		}, &stdout, &stderr)
		watchdogSender = smtpSender
		pings, bodies := log.got()
		fail := code != tc.wantCode || len(rec.sent) != tc.wantSent || !strings.Contains(stdout.String(), tc.wantOut)
		if tc.want == "" {
			fail = fail || len(pings) != 0
		} else {
			fail = fail || len(pings) != 1 || pings[0] != tc.want || !strings.Contains(bodies[0], tc.wantBody)
		}
		if fail {
			t.Errorf("%s: exit %d, mailed %v, pings %v %q; want exit %d, %d mails, %q with %q\nstdout %s\nstderr %s", tc.what, code, rec.sent, pings, bodies, tc.wantCode, tc.wantSent, tc.want, tc.wantBody, stdout.String(), stderr.String())
		}
		if tc.wantSent > 0 {
			if got := *rec.relays[0]; got.Host != "smtp.config.example" || got.Port != 2525 || got.Password != "env-pw" {
				t.Errorf("%s: relay %+v, want the configured one with the env password", tc.what, got)
			}
			if s, err := watchdog.LoadState(statePath); err != nil || s.Relay == nil || s.Relay.Host != "smtp.config.example" {
				t.Errorf("%s: the state caches relay %+v (%v), want the configured one", tc.what, s.Relay, err)
			}
		}
		if strings.Contains(tc.wantOut, "watchdog/state") {
			if aside, _ := filepath.Glob(statePath + ".unreadable-*"); len(aside) != 1 {
				t.Errorf("%s: moved aside %v, want one file", tc.what, aside)
			}
		}
	}
}
