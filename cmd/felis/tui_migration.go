package main

import (
	"context"
	"fmt"
	"time"

	"felis.lolicon.best/internal/store"

	tea "github.com/charmbracelet/bubbletea"
)

type migRunMsg struct {
	n     int
	total int
	err   error
}

type migrationModel struct {
	dbURL  string
	osUser string

	state   string
	applied int
	total   int
	lastErr error
}

func newMigrationModel(dbURL, osUser string) *migrationModel {
	return &migrationModel{
		dbURL:  dbURL,
		osUser: osUser,
		state:  "checking",
	}
}

func (m *migrationModel) Init() tea.Cmd {
	return func() tea.Msg {
		n, err := countMigrations(m.dbURL)
		if err != nil {
			return migRunMsg{err: err}
		}
		total, err := totalMigrations()
		return migRunMsg{n: n, total: total, err: err}
	}
}

func (m *migrationModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case migRunMsg:
		if msg.err != nil {
			m.state = "error"
			m.lastErr = msg.err
			return m, nil
		}
		m.applied = msg.n
		m.total = msg.total
		if m.applied < m.total {
			m.state = "pending"
			return m, nil
		}
		return m, func() tea.Msg { return migrationDoneMsg{n: msg.n} }

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, func() tea.Msg { return switchToDashboard{} }
		case "enter":
			if m.state == "pending" {
				m.state = "running"
				return m, runMigrationsCmd(m.dbURL)
			}
		}
	}
	return m, nil
}

func (m *migrationModel) View() string {
	var b string
	b += tuiHeader("Database Migrations") + "\n"

	switch m.state {
	case "checking":
		b += tuiHint.Render("Checking migration status…") + "\n"
	case "pending":
		b += tuiWarn.Render(fmt.Sprintf("%d of %d migrations applied. %d pending.", m.applied, m.total, m.total-m.applied)) + "\n\n"
		b += tuiInfo("Press enter to run pending migrations.") + "\n"
	case "running":
		b += tuiHint.Render("Running migrations…") + "\n"
		b += tuiInfo("This should take only a moment.") + "\n"
	case "error":
		b += tuiErr.Render("Migration check failed:") + "\n"
		b += tuiHint.Render(m.lastErr.Error()) + "\n"
	default:
		b += tuiOK.Render(fmt.Sprintf("✓ %d migrations applied.", m.applied)) + "\n"
	}

	b += "\n"
	b += tuiSeparator() + "\n"
	switch m.state {
	case "pending":
		b += tuiAction("enter", "run", "esc", "back")
	case "running":
		b += tuiAction("esc", "cancel")
	default:
		b += tuiAction("esc", "back")
	}
	return b
}

func runMigrationsCmd(dbURL string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		drv, err := store.Open(ctx, dbURL)
		if err != nil {
			return migRunMsg{err: fmt.Errorf("open db: %w", err)}
		}
		defer drv.Close()
		migrations, err := store.LoadMigrations()
		if err != nil {
			return migRunMsg{err: err}
		}
		_, err = store.Up(ctx, drv, migrations)
		if err != nil {
			return migRunMsg{err: err}
		}
		applied, err := drv.AppliedVersions(ctx)
		if err != nil {
			return migRunMsg{err: err}
		}
		return migRunMsg{n: len(applied), total: len(migrations)}
	}
}

func totalMigrations() (int, error) {
	migrations, err := store.LoadMigrations()
	if err != nil {
		return 0, err
	}
	return len(migrations), nil
}
