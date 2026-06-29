package main

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type dashboardModel struct {
	ctx         context.Context
	store       ownerStore
	rootDomain  string
	adminHost   string
	panelHost   string
	osUser      string
	dbURL       string
	adminExists bool

	focus int

	pgStatus stepStatus
	pgDetail string

	mgStatus stepStatus
	mgDetail string

	owStatus stepStatus
	owDetail string

	paStatus stepStatus
	paDetail string

	egStatus stepStatus
	egDetail string
}

func newDashboardModel(ctx context.Context, store ownerStore, dbURL, osUser, rootDomain, adminHost, panelHost string) *dashboardModel {
	return &dashboardModel{
		ctx:        ctx,
		store:      store,
		dbURL:      dbURL,
		osUser:     osUser,
		rootDomain: rootDomain,
		adminHost:  adminHost,
		panelHost:  panelHost,
		pgStatus:   statusPending,
		mgStatus:   statusPending,
		owStatus:   statusOptional,
		paStatus:   statusPending,
		egStatus:   statusOptional,
	}
}

func (m *dashboardModel) Init() tea.Cmd { return nil }

func (m *dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "up":
			if m.focus > 0 {
				m.focus--
			}
			return m, nil
		case "down":
			if m.focus < 4 {
				m.focus++
			}
			return m, nil
		case "enter":
			return m.enterStep()
		case "r", "R":
			return m, func() tea.Msg { return switchToDashboard{} }
		}
	}
	return m, nil
}

func (m *dashboardModel) enterStep() (tea.Model, tea.Cmd) {
	switch m.focus {
	case 0:
		return newPostgresModel(m.dbURL, m.osUser), nil
	case 1:
		return newMigrationModel(m.dbURL, m.osUser), nil
	case 2:
		if m.adminExists {
			return m, nil
		}
		return newOwnerModel(m.ctx, m.store, m.osUser, false), nil
	case 3:
		return m, nil
	case 4:
		return newEdgeModel(m.rootDomain, m.adminHost, m.panelHost), nil
	}
	return m, nil
}

func (m *dashboardModel) View() string {
	var b strings.Builder
	b.WriteString(tuiHeader("Setup"))

	type step struct {
		title  string
		status stepStatus
		detail string
		idx    int
	}

	steps := []step{
		{"Database & Migrations", m.pgStatus, m.pgDetail, 0},
		{"Database Migrations", m.mgStatus, m.mgDetail, 1},
		{"Owner Account", m.owStatus, m.owDetail, 2},
		{"Panel Access", m.paStatus, m.paDetail, 3},
		{"Cloudflare Edge", m.egStatus, m.egDetail, 4},
	}

	for _, s := range steps {
		icon := tuiIcon(s.status)
		label := s.title
		if s.detail != "" {
			label += "  " + tuiHint.Render(s.detail)
		}
		prefix := "  "
		if m.focus == s.idx {
			prefix = tuiLabel.Render("▸ ")
		}
		b.WriteString(fmt.Sprintf("%s%s %s\n\n", prefix, icon, label))
	}

	b.WriteString("\n")
	b.WriteString(tuiSeparator())
	b.WriteString("\n")
	b.WriteString(tuiAction("enter", "configure", "r", "refresh", "↑↓", "navigate", "esc", "exit"))
	return b.String()
}
