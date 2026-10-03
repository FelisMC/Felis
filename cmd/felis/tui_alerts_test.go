package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"felis.lolicon.best/internal/config"
)

// TestAlertRouteLines: the summary's alert rows say where the watchdog's
// alerts go, and flag every route that reaches no one.
func TestAlertRouteLines(t *testing.T) {
	cases := []struct {
		name     string
		r        alertRoute
		alerts   string
		alertsOK bool
		beat     string
		beatOK   bool
	}{
		{
			name:   "fresh install",
			alerts: "only logged: no email relay (press e)",
			beat:   "none: no outside check (troubleshooting.md §14)",
		},
		{
			name:   "relay, no verified owner",
			r:      alertRoute{relay: "smtp.example.com:587", heartbeat: "https://hc-ping.com/..."},
			alerts: "only logged: no verified Owner email (panel → Account)",
			beat:   "https://hc-ping.com/... every 2 minutes", beatOK: true,
		},
		{
			name:   "relay, owners unreadable",
			r:      alertRoute{relay: "smtp.example.com:587", lookupErr: errors.New("connection refused")},
			alerts: "via smtp.example.com:587; could not read the Owner addresses",
			beat:   "none: no outside check (troubleshooting.md §14)",
		},
		{
			name:   "owners but no relay",
			r:      alertRoute{recipients: []string{"owner@example.com"}},
			alerts: "only logged: no email relay (press e)",
			beat:   "none: no outside check (troubleshooting.md §14)",
		},
		{
			name:   "mailed",
			r:      alertRoute{relay: "smtp.example.com:587", recipients: []string{"a@example.com", "b@example.com"}, heartbeatErr: errors.New("/etc/felis/watchdog-heartbeat-url: the heartbeat URL is not an http:// or https:// URL")},
			alerts: "mailed to a@example.com, b@example.com via smtp.example.com:587", alertsOK: true,
			beat: "unreadable: /etc/felis/watchdog-heartbeat-url: the heartbeat URL is not an http:// or https:// URL",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if line, ok := c.r.alertsLine(); line != c.alerts || ok != c.alertsOK {
				t.Errorf("alerts = %q, %v; want %q, %v", line, ok, c.alerts, c.alertsOK)
			}
			if line, ok := c.r.heartbeatLine(); line != c.beat || ok != c.beatOK {
				t.Errorf("heartbeat = %q, %v; want %q, %v", line, ok, c.beat, c.beatOK)
			}
		})
	}
}

// TestSummaryAlertRows: the rows show on the card, a route that needs action
// marked so it reads without colour, and stay off a summary with no route.
func TestSummaryAlertRows(t *testing.T) {
	m := &summaryModel{ownerUsername: "owner", alerts: &alertRoute{relay: "smtp.example.com:587", recipients: []string{"owner@example.com"}}}
	v := m.View()
	for _, want := range []string{"alerts    mailed to owner@example.com via smtp.example.com:587", "heartbeat ⚠ none: no outside check"} {
		if !strings.Contains(v, want) {
			t.Errorf("summary lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "⚠ mailed") {
		t.Errorf("a route that reaches the owners is marked:\n%s", v)
	}
	m.alerts = &alertRoute{heartbeat: "https://hc-ping.com/..."}
	v = m.View()
	for _, want := range []string{"alerts    ⚠ only logged: no email relay (press e)", "heartbeat https://hc-ping.com/... every 2 minutes"} {
		if !strings.Contains(v, want) {
			t.Errorf("summary lacks %q:\n%s", want, v)
		}
	}
	m.alerts = nil
	if v = m.View(); strings.Contains(v, "alerts") || strings.Contains(v, "heartbeat") {
		t.Errorf("a summary with no route shows alert rows:\n%s", v)
	}
}

// TestRootSummaryReadsAlertRoute: the summary and the re-run status screen
// read the route each time they open, so a relay configured with e shows at
// once.
func TestRootSummaryReadsAlertRoute(t *testing.T) {
	m := newTestRoot(false, consoleModeSetup, "")
	reads := 0
	route := alertRoute{}
	m.alertRoute = func(context.Context) alertRoute {
		reads++
		return route
	}
	m = drive(t, m, storageResultMsg{method: storageLocal, detail: "local disk"})
	m = drive(t, m, ownerResultMsg{username: "owner", setupTokenURL: "https://op.console.example.com/setup?token=t0ken"})
	sum, ok := m.screen.(*summaryModel)
	if !ok || sum.alerts == nil || sum.alerts.relay != "" {
		t.Fatalf("summary after storage: %T %+v", m.screen, sum)
	}
	route = alertRoute{relay: "smtp.example.com:587", recipients: []string{"owner@example.com"}}
	m = drive(t, m, smtpResultMsg{configured: true})
	sum, ok = m.screen.(*summaryModel)
	if !ok || sum.alerts == nil || sum.alerts.relay != "smtp.example.com:587" {
		t.Fatalf("summary after configuring email: %T %+v", m.screen, sum)
	}
	if reads != 2 {
		t.Errorf("route read %d times, want 2", reads)
	}

	status := newTestRoot(true, consoleModeSetup, "")
	status.alertRoute = m.alertRoute
	status.showStatus()
	if sum, ok := status.screen.(*summaryModel); !ok || sum.alerts == nil || sum.alerts.relay != "smtp.example.com:587" {
		t.Fatalf("status screen: %T %+v", status.screen, status.screen)
	}
}

// TestHostAlertRoute reads the relay from the host config and the heartbeat
// file, shows the heartbeat by its host only, and reports a database it cannot
// reach.
func TestHostAlertRoute(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "felis.host.toml")
	writeTestFile(t, cfg, testWatchdogConfig+testWatchdogSMTP, 0o600)
	beat := filepath.Join(dir, "watchdog-heartbeat-url")
	writeTestFile(t, beat, "https://hc-ping.com/check-key\n", 0o600)
	dbURL := "postgres://felis:pw@127.0.0.1:1/felis?sslmode=disable&connect_timeout=2"

	r := hostAlertRoute(context.Background(), cfg, dbURL, beat)
	if r.relay != "smtp.config.example:2525" {
		t.Errorf("relay = %q", r.relay)
	}
	if r.heartbeat != "https://hc-ping.com/..." || r.heartbeatErr != nil {
		t.Errorf("heartbeat = %q, %v", r.heartbeat, r.heartbeatErr)
	}
	if r.lookupErr == nil {
		t.Errorf("an unreachable database reads as %v", r.recipients)
	}

	noRelay := filepath.Join(dir, "no-relay.toml")
	writeTestFile(t, noRelay, testWatchdogConfig, 0o600)
	writeTestFile(t, beat, "hc-ping.com/check-key\n", 0o600)
	r = hostAlertRoute(context.Background(), noRelay, dbURL, beat)
	if r.relay != "" {
		t.Errorf("no [smtp]: relay = %q", r.relay)
	}
	if r.heartbeatErr == nil || strings.Contains(r.heartbeatErr.Error(), "check-key") {
		t.Errorf("a bad heartbeat file: %v", r.heartbeatErr)
	}
	r = hostAlertRoute(context.Background(), noRelay, dbURL, filepath.Join(dir, "none"))
	if r.heartbeat != "" || r.heartbeatErr != nil {
		t.Errorf("no heartbeat file: %q, %v", r.heartbeat, r.heartbeatErr)
	}
}

// TestSummaryWithAlertsFitsTerminal: the two rows keep the summary inside the
// terminal, with the longest route lines.
func TestSummaryWithAlertsFitsTerminal(t *testing.T) {
	for _, w := range []int{60, 80, 90} {
		for _, h := range []int{24, 30, 45} {
			m := newTestRoot(false, consoleModeSetup, "")
			m.alertRoute = func(context.Context) alertRoute {
				return alertRoute{relay: "smtp.example.com:587", lookupErr: errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")}
			}
			m = drive(t, m, tea.WindowSizeMsg{Width: w, Height: h})
			m = drive(t, m, preflightDoneMsg{})
			m = drive(t, m, ownerResultMsg{username: "owner", setupTokenURL: "https://op.console.example.com/setup?token=t0ken"})
			m = drive(t, m, connectResultMsg{method: connectLocal, panelHostname: "panel.example.com"})
			m = drive(t, m, storageResultMsg{method: storageLocal, detail: "local disk · /var/lib/felis/uploads"})
			if _, ok := m.screen.(*summaryModel); !ok {
				t.Fatalf("screen = %T, want the summary", m.screen)
			}
			if got := lipgloss.Height(m.View()); got > h {
				t.Errorf("terminal %dx%d: summary with alert rows = %d rows (exceeds height)", w, h, got)
			}
		}
	}
}

// TestConsoleRootReadsHostAlertRoute: the console the host runs gives its
// summary this host's route, read against its database.
func TestConsoleRootReadsHostAlertRoute(t *testing.T) {
	db := config.DatabaseConfig{URL: "postgres://felis:pw@127.0.0.1:1/felis?sslmode=disable&connect_timeout=2"}
	rm := newConsoleRoot(context.Background(), &fakeOwnerStore{}, db, "felis.example.com", "admin.felis.example.com", "panel.felis.example.com", "", "minecraft", "root", false, consoleModeSetup, recoveryConfig{})
	if rm.alertRoute == nil {
		t.Fatal("the console's summary reads no alert route")
	}
	if r := rm.alertRoute(context.Background()); r.lookupErr == nil {
		t.Errorf("the route did not query the console's database: %+v", r)
	}
}
