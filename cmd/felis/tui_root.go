package main

import (
	"context"
	"fmt"

	"felis.lolicon.best/internal/cfsetup"

	tea "github.com/charmbracelet/bubbletea"
)

// ---- Messages: sub-model → root ----

type pgDoneMsg struct{ err error }

type migrationDoneMsg struct {
	n   int
	err error
}

type ownerResultMsg struct {
	username        string
	displayPassword string
	mode            string
	accountable     string
	auditWarning    string
	err             error
}

type edgeResultMsg struct {
	result        *cfsetup.Result
	panelHostname string
	adminHostname string
	err           error
}

type panelCheckMsg struct{ result panelAccessResult }

// switchToDashboard tells the root to show the dashboard.
type switchToDashboard struct{}

// ---- rootModel: top-level session ----

type rootModel struct {
	ctx context.Context

	screen    tea.Model       // current active screen
	dashboard *dashboardModel // always preserved

	result breakGlassResult
	err    error

	mode        consoleMode
	dbURL       string
	store       ownerStore
	osUser      string
	rootDomain  string
	adminHost   string
	panelHost   string
	adminExists bool
}

func newRootModel(ctx context.Context, store ownerStore, dbURL, rootDomain, adminHostname, panelHostname, osUser string, adminExists bool, mode consoleMode) *rootModel {
	rm := &rootModel{
		ctx:         ctx,
		dbURL:       dbURL,
		store:       store,
		osUser:      osUser,
		rootDomain:  rootDomain,
		adminHost:   adminHostname,
		panelHost:   panelHostname,
		adminExists: adminExists,
		mode:        mode,
		dashboard:   newDashboardModel(ctx, store, dbURL, osUser, rootDomain, adminHostname, panelHostname),
		result: breakGlassResult{
			osUser:        osUser,
			rootDomain:    rootDomain,
			adminHostname: adminHostname,
		},
	}
	rm.dashboard.adminExists = adminExists
	rm.refreshDashboard()
	if mode == consoleModeBreakGlass {
		rm.screen = newOwnerModel(ctx, store, osUser, adminExists)
	} else {
		rm.screen = rm.dashboard
	}
	return rm
}

func (m *rootModel) Init() tea.Cmd {
	if m.mode == consoleModeSetup {
		return m.checkStatus()
	}
	return m.screen.Init()
}

func (m *rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pgDoneMsg:
		if msg.err != nil {
			m.dashboard.pgStatus = statusFailed
			m.dashboard.pgDetail = msg.err.Error()
			m.showDashboard()
			return m, nil
		}
		m.dashboard.pgStatus = statusDone
		m.dashboard.pgDetail = "ready"
		m.showDashboard()
		return m, m.checkMigrations()

	case migrationDoneMsg:
		if msg.err == nil {
			m.dashboard.mgStatus = statusDone
			m.dashboard.mgDetail = fmt.Sprintf("%d applied", msg.n)
		} else {
			m.dashboard.mgStatus = statusFailed
			m.dashboard.mgDetail = msg.err.Error()
		}
		m.showDashboard()
		if msg.err != nil {
			return m, nil
		}
		return m, m.checkPanel()

	case ownerResultMsg:
		if msg.err != nil {
			if m.mode == consoleModeBreakGlass {
				m.err = msg.err
				return m, tea.Quit
			}
			m.dashboard.owStatus = statusFailed
			m.dashboard.owDetail = msg.err.Error()
			m.showDashboard()
			return m, nil
		}
		m.result.provisioned = true
		m.result.username = msg.username
		m.result.displayPassword = msg.displayPassword
		m.result.mode = msg.mode
		m.result.accountable = msg.accountable
		m.result.auditWarning = msg.auditWarning
		if m.mode == consoleModeBreakGlass {
			return m, tea.Quit
		}
		m.adminExists = true
		m.dashboard.adminExists = true
		m.dashboard.owStatus = statusDone
		m.dashboard.owDetail = msg.username
		m.showDashboard()
		return m, m.checkPanel()

	case panelCheckMsg:
		m.result.panelURL = msg.result.url
		if msg.result.err != nil {
			m.dashboard.paStatus = statusFailed
			m.dashboard.paDetail = msg.result.err.Error()
		} else {
			m.dashboard.paStatus = statusDone
			m.dashboard.paDetail = msg.result.url
		}
		m.showDashboard()
		return m, nil

	case edgeResultMsg:
		if msg.err != nil {
			if m.mode == consoleModeBreakGlass {
				m.err = msg.err
				return m, tea.Quit
			}
			m.dashboard.egStatus = statusFailed
			m.dashboard.egDetail = msg.err.Error()
			m.showDashboard()
			return m, nil
		}
		m.result.edgeConfigured = true
		m.result.edgeAud = msg.result.AccessAud
		m.result.edgeRoutedHosts = msg.result.RoutedHostnames
		m.result.edgeConfigPath = msg.result.ConfigPath
		m.result.edgePanelHostname = msg.panelHostname
		m.result.edgeAdminHostname = msg.adminHostname
		if m.mode == consoleModeBreakGlass {
			return m, tea.Quit
		}
		m.dashboard.egStatus = statusDone
		m.dashboard.egDetail = "configured"
		m.showDashboard()
		return m, nil

	case switchToDashboard:
		m.refreshDashboard()
		return m, m.checkStatus()

	}

	if m.screen != nil {
		newScreen, cmd := m.screen.Update(msg)
		if newScreen != nil {
			if newScreen != m.screen {
				m.screen = newScreen
				return m, tea.Batch(cmd, m.screen.Init())
			}
		}
		return m, cmd
	}
	return m, nil
}

func (m *rootModel) View() string {
	if m.screen != nil {
		return m.screen.View()
	}
	return ""
}

func (m *rootModel) showDashboard() {
	m.screen = m.dashboard
}

func (m *rootModel) refreshDashboard() {
	d := m.dashboard
	d.pgStatus = statusPending
	d.mgStatus = statusPending
	d.owStatus = statusOptional
	d.paStatus = statusPending
	d.egStatus = statusOptional
	if m.adminExists {
		d.owStatus = statusDone
		d.owDetail = "already exists (use breakGlass to reset)"
	}
	if m.result.provisioned {
		d.owStatus = statusDone
		d.owDetail = m.result.username
	}
	if m.result.edgeConfigured {
		d.egStatus = statusDone
		d.egDetail = "configured"
	}
	if m.result.panelURL != "" {
		d.paStatus = statusDone
		d.paDetail = m.result.panelURL
	}
}

func (m *rootModel) checkStatus() tea.Cmd {
	return func() tea.Msg {
		if err := checkPostgres(m.dbURL); err != nil {
			return pgDoneMsg{err: err}
		}
		return pgDoneMsg{}
	}
}

func (m *rootModel) checkMigrations() tea.Cmd {
	return func() tea.Msg {
		n, err := countMigrations(m.dbURL)
		return migrationDoneMsg{n: n, err: err}
	}
}

func (m *rootModel) checkPanel() tea.Cmd {
	return func() tea.Msg {
		return panelCheckMsg{result: checkPanelAccess(m.rootDomain)}
	}
}
