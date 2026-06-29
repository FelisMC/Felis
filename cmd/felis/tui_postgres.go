package main

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"felis.lolicon.best/internal/store"

	tea "github.com/charmbracelet/bubbletea"
)

type pgCheckMsg struct {
	running   bool
	reachable bool
	err       error
}

type pgInstallMsg struct{ err error }

type pgCreateMsg struct{ err error }

type postgresModel struct {
	dbURL  string
	osUser string

	state    string // "checking", "missing", "installing", "creating", "done", "error"
	lastErr  error
	pgExists bool
}

func newPostgresModel(dbURL, osUser string) *postgresModel {
	return &postgresModel{
		dbURL:  dbURL,
		osUser: osUser,
		state:  "checking",
	}
}

func (m *postgresModel) Init() tea.Cmd {
	return checkPostgresCmd(m.dbURL)
}

func (m *postgresModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pgCheckMsg:
		if msg.err != nil || !msg.reachable {
			m.state = "missing"
			m.lastErr = msg.err
			return m, nil
		}
		return m, func() tea.Msg { return pgDoneMsg{} }

	case pgInstallMsg:
		if msg.err != nil {
			m.state = "error"
			m.lastErr = msg.err
			return m, nil
		}
		m.state = "creating"
		return m, createDatabaseCmd(m.dbURL)

	case pgCreateMsg:
		if msg.err != nil {
			m.state = "error"
			m.lastErr = msg.err
			return m, nil
		}
		return m, func() tea.Msg { return pgDoneMsg{} }

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, func() tea.Msg { return switchToDashboard{} }
		case "i", "I":
			if m.state == "missing" {
				m.state = "installing"
				return m, installPostgresCmd()
			}
		}
	}
	return m, nil
}

func (m *postgresModel) View() string {
	var b strings.Builder
	b.WriteString(tuiHeader("Database Setup"))

	switch m.state {
	case "checking":
		b.WriteString(tuiHint.Render("Checking PostgreSQL status…") + "\n")
	case "missing":
		b.WriteString(tuiWarn.Render("PostgreSQL is not reachable.") + "\n\n")
		if m.lastErr != nil {
			b.WriteString(tuiHint.Render(m.lastErr.Error()) + "\n\n")
		}
		b.WriteString(tuiInfo("Press i to install PostgreSQL, or esc to skip.") + "\n")
	case "installing":
		b.WriteString(tuiHint.Render("Installing PostgreSQL via apt…") + "\n")
		b.WriteString(tuiInfo("This may take up to a minute.") + "\n")
	case "creating":
		b.WriteString(tuiHint.Render("PostgreSQL installed. Creating database…") + "\n")
	case "done":
		b.WriteString(tuiOK.Render("✓ Database is ready.") + "\n")
	case "error":
		b.WriteString(tuiErr.Render("Failed to set up PostgreSQL:") + "\n")
		if m.lastErr != nil {
			b.WriteString(tuiHint.Render(m.lastErr.Error()) + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(tuiSeparator())
	b.WriteString("\n")
	switch m.state {
	case "missing":
		b.WriteString(tuiAction("i", "install", "esc", "back"))
	case "done", "error":
		b.WriteString(tuiAction("esc", "back"))
	default:
		b.WriteString(tuiAction("esc", "cancel"))
	}
	return b.String()
}

func checkPostgresCmd(dbURL string) tea.Cmd {
	return func() tea.Msg {
		err := checkPostgres(dbURL)
		if err != nil {
			return pgCheckMsg{reachable: false, err: err}
		}
		return pgCheckMsg{reachable: true}
	}
}

func checkPostgres(dbURL string) error {
	cfg, err := parseDBURL(dbURL)
	if err != nil {
		return fmt.Errorf("invalid database URL: %w", err)
	}
	conn, err := net.DialTimeout("tcp", cfg.addr, 2*time.Second)
	if err != nil {
		return fmt.Errorf("cannot reach PostgreSQL at %s: %w", cfg.addr, err)
	}
	conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drv, err := store.Open(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	drv.Close()
	return nil
}

func installPostgresCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "apt-get", "install", "-y", "postgresql")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return pgInstallMsg{err: fmt.Errorf("%w: %s", err, string(out))}
		}
		// Start the service.
		start := exec.CommandContext(ctx, "systemctl", "restart", "postgresql")
		start.CombinedOutput()
		return pgInstallMsg{}
	}
}

func createDatabaseCmd(dbURL string) tea.Cmd {
	return func() tea.Msg {
		cfg, err := parseDBURL(dbURL)
		if err != nil {
			return pgCreateMsg{err: err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Create user and database via PostgreSQL command line.
		cmds := [][]string{
			{"psql", "-c", fmt.Sprintf("CREATE USER %s WITH PASSWORD '%s';", cfg.user, cfg.pass)},
			{"psql", "-c", fmt.Sprintf("CREATE DATABASE %s OWNER %s;", cfg.db, cfg.user)},
			{"psql", "-c", fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE %s TO %s;", cfg.db, cfg.user)},
		}
		for _, args := range cmds {
			cmd := exec.CommandContext(ctx, "su", append([]string{"-", "postgres", "-c"}, strings.Join(args, " "))...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				// Ignore "already exists" errors.
				s := string(out)
				if strings.Contains(s, "already exists") {
					continue
				}
				return pgCreateMsg{err: fmt.Errorf("%w: %s", err, s)}
			}
		}
		return pgCreateMsg{}
	}
}

type dbCfg struct {
	addr string
	user string
	pass string
	db   string
}

func parseDBURL(url string) (dbCfg, error) {
	// Simple parser for postgres://user:pass@host:port/db?options
	s := strings.TrimPrefix(url, "postgres://")
	s = strings.TrimPrefix(s, "postgresql://")
	parts := strings.SplitN(s, "@", 2)
	if len(parts) != 2 {
		return dbCfg{}, fmt.Errorf("malformed URL")
	}
	auth := strings.SplitN(parts[0], ":", 2)
	rest := strings.SplitN(parts[1], "/", 2)
	if len(rest) < 2 {
		return dbCfg{}, fmt.Errorf("malformed URL: no database")
	}
	hostport := rest[0]
	dbname := strings.SplitN(rest[1], "?", 2)[0]
	if !strings.Contains(hostport, ":") {
		hostport += ":5432"
	}
	return dbCfg{
		addr: hostport,
		user: auth[0],
		pass: func() string {
			if len(auth) > 1 {
				return auth[1]
			}
			return ""
		}(),
		db: dbname,
	}, nil
}

func countMigrations(dbURL string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drv, err := store.Open(ctx, dbURL)
	if err != nil {
		return 0, err
	}
	defer drv.Close()
	if err := drv.EnsureVersionTable(ctx); err != nil {
		return 0, err
	}
	applied, err := drv.AppliedVersions(ctx)
	if err != nil {
		return 0, err
	}
	return len(applied), nil
}
