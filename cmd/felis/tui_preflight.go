package main

import (
	"fmt"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// preflightModel auto-verifies the control plane before the operator does any
// hands-on configuration: PostgreSQL must be reachable and schema migrations
// must be applied. Both are expected to already be true after host bootstrap;
// this stage simply proves it (and self-heals pending migrations) so the rest
// of the wizard can assume a healthy backend. It is non-interactive — it runs
// to completion and reports back to the root via preflightDoneMsg.
type preflightModel struct {
	dbURL      string
	rootDomain string
	adminHost  string

	sp    spinner.Model
	state pfState
	err   error

	dbOK      bool
	applied   int
	total     int
	migrated  bool
	panelOK   bool
	panelNote string
}

type pfState int

const (
	pfCheckDB pfState = iota
	pfCheckMig
	pfApplyMig
	pfCheckPanel
	pfDone
	pfError
)

type pfDBMsg struct{ err error }

type pfMigCheckMsg struct {
	applied int
	total   int
	err     error
}

type pfMigApplyMsg struct {
	applied int
	err     error
}

type pfPanelMsg struct{ err error }

func newPreflightModel(dbURL, rootDomain, adminHostname string) *preflightModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiLabel
	return &preflightModel{dbURL: dbURL, rootDomain: rootDomain, adminHost: adminHostname, sp: sp, state: pfCheckDB}
}

func (m *preflightModel) Init() tea.Cmd {
	return tea.Batch(m.sp.Tick, m.checkDB())
}

func (m *preflightModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pfDBMsg:
		if msg.err != nil {
			m.state, m.err = pfError, msg.err
			return m, nil
		}
		m.dbOK = true
		m.state = pfCheckMig
		return m, m.checkMigrations()

	case pfMigCheckMsg:
		if msg.err != nil {
			m.state, m.err = pfError, msg.err
			return m, nil
		}
		m.applied, m.total = msg.applied, msg.total
		if msg.applied < msg.total {
			m.state = pfApplyMig
			return m, m.applyMigrations()
		}
		m.state = pfCheckPanel
		return m, m.checkPanel()

	case pfMigApplyMsg:
		if msg.err != nil {
			m.state, m.err = pfError, msg.err
			return m, nil
		}
		m.applied, m.migrated = msg.applied, true
		m.state = pfCheckPanel
		return m, m.checkPanel()

	case pfPanelMsg:
		// Non-blocking: a panel that isn't serving yet is a warning, not a wall —
		// it is often still starting. The operator can proceed regardless.
		if msg.err != nil {
			m.panelNote = "not serving yet (it may still be starting): " + msg.err.Error()
		} else {
			m.panelOK = true
		}
		m.state = pfDone
		return m, m.finish()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "r", "R", "enter":
			if m.state == pfError {
				m.state, m.err = pfCheckDB, nil
				m.dbOK, m.migrated, m.panelOK, m.panelNote = false, false, false, ""
				return m, tea.Batch(m.sp.Tick, m.checkDB())
			}
		}
	}
	return m, nil
}

func (m *preflightModel) View() string {
	var b string
	b += tuiLabel.Render("Preflight") + "  " + tuiHint.Render("verifying the control plane before configuration") + "\n\n"

	b += m.line(m.dbOK, m.state == pfCheckDB, "PostgreSQL reachable", "")

	migDetail := ""
	if m.total > 0 {
		migDetail = pluralMigrations(m.applied, m.total)
		if m.migrated {
			migDetail += " (just applied)"
		}
	}
	migDone := m.state == pfCheckPanel || m.state == pfDone
	migActive := m.state == pfCheckMig || m.state == pfApplyMig
	b += m.line(migDone, migActive, "Database migrations", migDetail)

	// Panel serving is best-effort: ✓ when reachable, ○ + note otherwise.
	panelIcon := tuiIconOpt
	panelDetail := m.panelNote
	switch {
	case m.state == pfCheckPanel:
		panelIcon = m.sp.View()
	case m.panelOK:
		panelIcon, panelDetail = tuiIconOK, "serving locally"
	}
	b += "  " + panelIcon + " " + tuiLabel.Render("Panel serving")
	if panelDetail != "" {
		b += "  " + tuiHint.Render(panelDetail)
	}
	b += "\n"

	b += "\n" + tuiSeparator() + "\n"
	switch m.state {
	case pfError:
		b += "\n" + tuiErrorBanner(m.errText()) + "\n\n"
		b += tuiAction("r", "retry", "esc", "exit")
	case pfDone:
		b += tuiHint.Render("Ready.") + "\n"
	default:
		b += tuiAction("esc", "cancel")
	}
	return b
}

func (m *preflightModel) line(done, active bool, label, detail string) string {
	icon := tuiIconOpt
	switch {
	case done:
		icon = tuiIconOK
	case active:
		icon = m.sp.View()
	}
	s := "  " + icon + " " + tuiLabel.Render(label)
	if detail != "" {
		s += "  " + tuiHint.Render(detail)
	}
	return s + "\n"
}

func (m *preflightModel) errText() string {
	if m.err == nil {
		return "preflight failed"
	}
	return m.err.Error()
}

func (m *preflightModel) checkDB() tea.Cmd {
	return func() tea.Msg { return pfDBMsg{err: checkPostgres(m.dbURL)} }
}

func (m *preflightModel) checkMigrations() tea.Cmd {
	return func() tea.Msg {
		applied, err := countMigrations(m.dbURL)
		if err != nil {
			return pfMigCheckMsg{err: err}
		}
		total, err := totalMigrations()
		return pfMigCheckMsg{applied: applied, total: total, err: err}
	}
}

func (m *preflightModel) applyMigrations() tea.Cmd {
	return func() tea.Msg {
		applied, err := applyMigrations(m.dbURL)
		return pfMigApplyMsg{applied: applied, err: err}
	}
}

func (m *preflightModel) checkPanel() tea.Cmd {
	return func() tea.Msg {
		return pfPanelMsg{err: checkPanelAccess(m.rootDomain, m.adminHost).err}
	}
}

func (m *preflightModel) finish() tea.Cmd {
	return func() tea.Msg { return preflightDoneMsg{} }
}

func pluralMigrations(applied, total int) string {
	if applied == total {
		return fmt.Sprintf("%d applied", total)
	}
	return fmt.Sprintf("%d of %d applied", applied, total)
}
