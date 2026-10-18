package main

import (
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type summaryModel struct {
	panelURL      string
	ownerUsername string
	setupTokenURL string // one-time first-login URL; shown once
	auditWarning  string
	accessLabel   string
	storageLabel  string // build-context storage backend recap; empty to omit
	routedHosts   []string
	alreadySetUp  bool // re-run: Owner pre-existed
	localHint     bool // show the self-signed-cert note
	// alerts is where the watchdog's alerts go; nil leaves the rows out.
	alerts     *alertRoute
	copyText   func(string) (desktop bool, err error)
	copyNotice string
	copyFailed bool
}

// summaryCopiedMsg reports a copy: desktop is set when a desktop clipboard on this
// machine took the text, which is the only case that can be confirmed.
type summaryCopiedMsg struct {
	desktop bool
	err     error
}

func (m *summaryModel) Init() tea.Cmd { return nil }

// arrowNavOK lets the root repurpose ←/→ to walk back through completed steps;
// the summary takes no text input, so the horizontal arrows are free.
func (m *summaryModel) arrowNavOK() bool { return true }

func (m *summaryModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if copied, ok := msg.(summaryCopiedMsg); ok {
		m.copyFailed = copied.err != nil
		switch {
		case copied.err != nil:
			m.copyNotice = "Could not copy: " + copied.err.Error() + ". Select the link above to copy it."
		case copied.desktop:
			m.copyNotice = "Link copied to clipboard."
		default:
			m.copyNotice = "Copy sent to your terminal. If nothing pastes, the terminal lacks OSC 52 support; select the link above to copy it."
		}
		return m, nil
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "c", "C":
			link := m.panelURL
			if m.setupTokenURL != "" {
				link = m.setupTokenURL
			}
			if link == "" {
				m.copyNotice, m.copyFailed = "No link to copy.", true
				return m, nil
			}
			copyText := m.copyText
			if copyText == nil {
				copyText = copyTerminalText
			}
			return m, func() tea.Msg {
				desktop, err := copyText(link)
				return summaryCopiedMsg{desktop: desktop, err: err}
			}
		case "n", "N":
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
	case m.setupTokenURL != "":
		b.WriteString(tuiOK.Render("✓ Deployment ready. Finish Owner login in your browser.") + "\n\n")
	case m.alreadySetUp:
		b.WriteString(tuiOK.Render("✓ Felis is already set up.") + "\n\n")
	default:
		b.WriteString(tuiOK.Render("✓ Setup complete.") + "\n\n")
	}

	var card strings.Builder
	if m.ownerUsername != "" {
		card.WriteString(tuiLabel.Render("owner     ") + m.ownerUsername + "\n")
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

	if m.setupTokenURL != "" {
		b.WriteString(tuiHint.Render("Open the setup link, record your email, and create a passkey.\nMinecraft can be linked later from Account; it is not required for panel access.") + "\n")
	} else {
		b.WriteString(tuiHint.Render("Manage servers, users, and Minecraft identities in the panel.") + "\n")
	}
	if m.auditWarning != "" {
		b.WriteString(tuiWarn.Render("Audit warning: "+m.auditWarning) + "\n")
	}
	if m.localHint {
		b.WriteString(tuiHint.Render("  The local certificate is self-signed; your browser may warn on first visit.") + "\n")
	}

	if m.copyNotice != "" {
		style := tuiOK
		if m.copyFailed {
			style = tuiWarn
		}
		b.WriteString("\n" + style.Render(m.copyNotice) + "\n")
	}
	copyLabel := "copy panel link"
	if m.setupTokenURL != "" {
		copyLabel = "copy setup link"
	}
	b.WriteString("\n" + tuiAction("c", copyLabel, "n", "change connection", "s", "change storage", "e", "configure email", "enter/esc", "exit"))
	return b.String()
}

// openTTY and desktopCopy are where copyTerminalText writes; tests replace them.
var (
	openTTY     = func() (io.WriteCloser, error) { return os.OpenFile("/dev/tty", os.O_WRONLY, 0) }
	desktopCopy = clipboard.WriteAll
)

// OSC 52 reaches SSH clients; graphical sessions also use the local clipboard.
// /dev/tty keeps the one-time link out of redirected stdout and install logs.
func copyTerminalText(text string) (desktop bool, err error) {
	if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		desktop = desktopCopy(text) == nil
	}
	if err := writeOSC52(text); err != nil && !desktop {
		return false, err
	}
	return desktop, nil
}

func writeOSC52(text string) error {
	tty, err := openTTY()
	if err != nil {
		return err
	}
	defer tty.Close()
	sequence := ansi.SetSystemClipboard(text)
	if os.Getenv("TMUX") != "" {
		sequence = ansi.TmuxPassthrough(sequence)
	}
	_, err = io.WriteString(tty, sequence)
	return err
}

type alertRoute struct {
	relay        string   // "host:port", "" with no relay
	recipients   []string // enabled Owners' verified addresses
	lookupErr    error    // the recipients could not be read
	heartbeat    string   // the heartbeat URL's scheme and host, "" with none
	heartbeatErr error    // the heartbeat file does not read
}

// readPanelSettings reads the relay and the heartbeat the Owner saved in the panel; tests
// replace it.
var readPanelSettings = panelSettings

// Panel relay and heartbeat settings override host configuration.
func hostAlertRoute(ctx context.Context, cfgPath, dbURL, heartbeatPath string) alertRoute {
	var r alertRoute
	if in := smtpInputsFrom(cfgPath); in.host != "" {
		r.relay = in.host + ":" + in.port
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r.recipients, r.lookupErr = ownerEmails(ctx, dbURL)
	panelRelay, panelBeat, _ := readPanelSettings(ctx, dbURL)
	if panelRelay != nil {
		r.relay = net.JoinHostPort(panelRelay.Host, strconv.Itoa(panelRelay.Port))
	}
	u, err := readHeartbeatURL(heartbeatPath)
	switch {
	case panelBeat != nil:
		r.heartbeat = redactURL(panelBeat.URL)
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
		return "only logged: no email relay (press e, or set it in the panel)", false
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
		return "none: no outside check (set one in the panel)", false
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
