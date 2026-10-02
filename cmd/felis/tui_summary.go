package main

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// summaryModel is the terminal screen of the setup wizard. On a first run it
// confirms what was just configured and points the operator at the panel for
// everything else. On a re-run (an Owner already exists) it doubles as a thin
// status landing screen — the whole point of the redesign is that setup runs
// once and the rest of administration happens in the panel.
type summaryModel struct {
	panelURL      string
	ownerUsername string
	ownerSkipped  bool   // the Owner step was skipped: say how to bind one
	gameAddr      string // where to join in Minecraft to bind the Owner
	setupTokenURL string // one-time first-login URL; shown once
	accessLabel   string
	storageLabel  string // build-context storage backend recap; empty to omit
	routedHosts   []string
	alreadySetUp  bool // re-run: Owner pre-existed
	localHint     bool // show the self-signed-cert note
	// alerts is where the watchdog's alerts go; nil leaves the rows out.
	alerts *alertRoute
}

func (m *summaryModel) Init() tea.Cmd { return nil }

// arrowNavOK lets the root repurpose ←/→ to walk back through completed steps;
// the summary takes no text input, so the horizontal arrows are free.
func (m *summaryModel) arrowNavOK() bool { return true }

func (m *summaryModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "c", "C":
			return m, func() tea.Msg { return reconfigureConnectMsg{} }
		case "s", "S":
			return m, func() tea.Msg { return reconfigureStorageMsg{} }
		case "e", "E":
			return m, func() tea.Msg { return reconfigureSMTPMsg{} }
		case "ctrl+c", "esc", "enter", "q":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *summaryModel) View() string {
	var b strings.Builder

	switch {
	case m.alreadySetUp:
		b.WriteString(tuiOK.Render("✓ Felis is already set up.") + "\n\n")
	case m.ownerSkipped:
		b.WriteString(tuiWarn.Render("⚠ Setup finished without an Owner.") + "\n\n")
	default:
		b.WriteString(tuiOK.Render("✓ Setup complete.") + "\n\n")
	}

	var card strings.Builder
	if m.ownerUsername != "" {
		card.WriteString(tuiLabel.Render("owner     ") + m.ownerUsername + "\n")
	}
	if m.ownerSkipped {
		card.WriteString(routeRow("owner     ", "not bound: nobody can sign in to the panel yet", false))
	}
	if m.setupTokenURL != "" {
		card.WriteString(tuiLabel.Render("setup URL ") + tuiPassword.Render(m.setupTokenURL) + "\n")
		card.WriteString("          " + tuiWarn.Render("one-time link — open it to finish login setup") + "\n")
	}
	if m.accessLabel != "" {
		card.WriteString(tuiLabel.Render("access    ") + m.accessLabel + "\n")
	}
	if m.storageLabel != "" {
		card.WriteString(tuiLabel.Render("storage   ") + m.storageLabel + "\n")
	}
	if len(m.routedHosts) > 0 {
		card.WriteString(tuiLabel.Render("routed    ") + strings.Join(m.routedHosts, ", ") + "\n")
	}
	if m.panelURL != "" {
		card.WriteString(tuiLabel.Render("panel     ") + m.panelURL + "\n")
	}
	if m.alerts != nil {
		line, ok := m.alerts.alertsLine()
		card.WriteString(routeRow("alerts    ", line, ok))
		line, ok = m.alerts.heartbeatLine()
		card.WriteString(routeRow("heartbeat ", line, ok))
	}
	b.WriteString(tuiCardStyle.Render(strings.TrimRight(card.String(), "\n")) + "\n\n")

	if m.ownerSkipped {
		b.WriteString(tuiWarn.Render("To bind the Owner, run  sudo felis setup  again and join "+ownerJoinTarget(m.gameAddr)+" in Minecraft.") + "\n")
	} else {
		b.WriteString(tuiHint.Render("ℹ Everything else — servers, users, plugins — is configured in the panel. You won't need this console again.") + "\n")
	}
	if m.localHint {
		b.WriteString(tuiHint.Render("  The local certificate is self-signed; your browser may warn on first visit.") + "\n")
	}

	b.WriteString("\n" + tuiAction("c", "change connection", "s", "change storage", "e", "configure email", "enter/esc", "exit"))
	return b.String()
}

// alertRoute is where this host's watchdog alerts go, as the summary shows it:
// by mail through the [smtp] relay to the Owners' verified addresses, and the
// heartbeat that notices the host itself going down (docs/troubleshooting.md
// §14). Setup runs mail-less by design, so a fresh install has neither; the
// summary says so where the Owner can press e.
type alertRoute struct {
	relay        string   // "host:port", "" with no [smtp] relay
	recipients   []string // enabled Owners' verified addresses
	lookupErr    error    // the recipients could not be read
	heartbeat    string   // the heartbeat URL's scheme and host, "" with none
	heartbeatErr error    // the heartbeat file does not read
}

// hostAlertRoute reads the route from the host config at cfgPath, the database
// at dbURL and the heartbeat file at heartbeatPath: what the next watchdog run
// uses.
func hostAlertRoute(ctx context.Context, cfgPath, dbURL, heartbeatPath string) alertRoute {
	var r alertRoute
	if in := smtpInputsFrom(cfgPath); in.host != "" {
		r.relay = in.host + ":" + in.port
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r.recipients, r.lookupErr = ownerEmails(ctx, dbURL)
	u, err := readHeartbeatURL(heartbeatPath)
	switch {
	case err != nil:
		r.heartbeatErr = err
	case u != "":
		r.heartbeat = redactURL(u)
	}
	return r
}

// alertsLine is the summary's alerts row; ok is false when the alerts reach no one.
func (r alertRoute) alertsLine() (line string, ok bool) {
	switch {
	case r.relay == "":
		return "only logged: no email relay (press e)", false
	case r.lookupErr != nil:
		return "via " + r.relay + "; could not read the Owner addresses", false
	case len(r.recipients) == 0:
		return "only logged: no verified Owner email (panel → Account)", false
	}
	return "mailed to " + strings.Join(r.recipients, ", ") + " via " + r.relay, true
}

// heartbeatLine is the summary's heartbeat row; ok is false with no heartbeat.
func (r alertRoute) heartbeatLine() (line string, ok bool) {
	switch {
	case r.heartbeatErr != nil:
		return "unreadable: " + r.heartbeatErr.Error(), false
	case r.heartbeat == "":
		return "none: no outside check (troubleshooting.md §14)", false
	}
	return r.heartbeat + " every 2 minutes", true
}

// routeRow renders one alert row, marked and in the warning style when it
// needs action: the mark reads without colour too.
func routeRow(label, line string, ok bool) string {
	if !ok {
		line = tuiWarn.Render("⚠ " + line)
	}
	return tuiLabel.Render(label) + line + "\n"
}
