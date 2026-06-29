package main

import (
	"strings"

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
	ownerPassword string // one-time; shown once
	accessLabel   string
	routedHosts   []string
	alreadySetUp  bool // re-run: Owner pre-existed
	localHint     bool // show the self-signed-cert note
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
		case "ctrl+c", "esc", "enter", "q":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *summaryModel) View() string {
	var b strings.Builder

	if m.alreadySetUp {
		b.WriteString(tuiOK.Render("✓ Felis is already set up.") + "\n\n")
	} else {
		b.WriteString(tuiOK.Render("✓ Setup complete.") + "\n\n")
	}

	var card strings.Builder
	if m.ownerUsername != "" {
		card.WriteString(tuiLabel.Render("owner     ") + m.ownerUsername + "\n")
	}
	if m.ownerPassword != "" {
		card.WriteString(tuiLabel.Render("password  ") + tuiPassword.Render(m.ownerPassword) + "\n")
		card.WriteString("          " + tuiWarn.Render("shown only once — record it now") + "\n")
	}
	if m.accessLabel != "" {
		card.WriteString(tuiLabel.Render("access    ") + m.accessLabel + "\n")
	}
	if len(m.routedHosts) > 0 {
		card.WriteString(tuiLabel.Render("routed    ") + strings.Join(m.routedHosts, ", ") + "\n")
	}
	if m.panelURL != "" {
		card.WriteString(tuiLabel.Render("panel     ") + m.panelURL + "\n")
	}
	b.WriteString(tuiCardStyle.Render(strings.TrimRight(card.String(), "\n")) + "\n\n")

	b.WriteString(tuiHint.Render("ℹ Everything else — servers, users, plugins — is configured in the panel. You won't need this console again.") + "\n")
	if m.localHint {
		b.WriteString(tuiHint.Render("  The local certificate is self-signed; your browser may warn on first visit.") + "\n")
	}

	b.WriteString("\n" + tuiAction("c", "change connection", "enter/esc", "exit"))
	return b.String()
}
