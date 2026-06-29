package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"

	felis "felis.lolicon.best"

	tea "github.com/charmbracelet/bubbletea"
)

type hostBootstrapState int

const (
	hostBootstrapIntro hostBootstrapState = iota
	hostBootstrapRunning
	hostBootstrapDone
	hostBootstrapError
)

type hostBootstrapDoneMsg struct {
	err error
}

type hostBootstrapModel struct {
	ctx       context.Context
	state     hostBootstrapState
	completed bool
	err       error
}

func newHostBootstrapModel(ctx context.Context) *hostBootstrapModel {
	return &hostBootstrapModel{ctx: ctx}
}

func (m *hostBootstrapModel) Init() tea.Cmd { return nil }

func (m *hostBootstrapModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case hostBootstrapDoneMsg:
		if msg.err != nil {
			m.state = hostBootstrapError
			m.err = msg.err
			return m, nil
		}
		m.state = hostBootstrapDone
		m.completed = true
		m.err = nil
		return m, nil

	case tea.KeyMsg:
		switch m.state {
		case hostBootstrapIntro:
			switch msg.String() {
			case "ctrl+c", "esc":
				return m, tea.Quit
			case "enter":
				m.state = hostBootstrapRunning
				m.err = nil
				return m, m.runBootstrap()
			}
		case hostBootstrapDone:
			switch msg.String() {
			case "ctrl+c", "esc", "enter":
				return m, tea.Quit
			}
		case hostBootstrapError:
			switch msg.String() {
			case "ctrl+c", "esc":
				return m, tea.Quit
			case "enter":
				m.state = hostBootstrapRunning
				m.err = nil
				return m, m.runBootstrap()
			}
		}
	}
	return m, nil
}

func (m *hostBootstrapModel) View() string {
	var b strings.Builder
	b.WriteString(tuiHeader("Host Bootstrap"))

	switch m.state {
	case hostBootstrapIntro:
		b.WriteString(tuiHint.Render("Host bootstrap has not been marked complete.") + "\n\n")
		b.WriteString(tuiInfo("The embedded installer will configure system packages, Docker, k3s, PostgreSQL, migrations, and the Felis control plane.") + "\n\n")
		b.WriteString(tuiWarn.Render("Run this on a disposable Linux host or VM. It changes system services.") + "\n")
		b.WriteString("\n" + tuiSeparator() + "\n")
		b.WriteString(tuiAction("enter", "run bootstrap", "esc", "cancel"))
	case hostBootstrapRunning:
		b.WriteString(tuiHint.Render("Running host bootstrap...") + "\n")
		b.WriteString(tuiInfo("The terminal is handed to the installer until it finishes.") + "\n")
	case hostBootstrapDone:
		b.WriteString(tuiSuccessBanner("Host bootstrap completed.") + "\n\n")
		b.WriteString(tuiInfo("Continue to create the Owner account and optional Cloudflare edge.") + "\n")
		b.WriteString("\n" + tuiSeparator() + "\n")
		b.WriteString(tuiAction("enter", "continue", "esc", "continue"))
	case hostBootstrapError:
		b.WriteString(tuiErrorBanner("Host bootstrap failed.") + "\n\n")
		if m.err != nil {
			b.WriteString(tuiHint.Render(m.err.Error()) + "\n")
		}
		b.WriteString("\n" + tuiSeparator() + "\n")
		b.WriteString(tuiAction("enter", "retry", "esc", "exit"))
	}
	return b.String()
}

func (m *hostBootstrapModel) runBootstrap() tea.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return hostBootstrapDoneMsg{err: err} }
	}
	cmd := exec.CommandContext(m.ctx, "bash", "-s")
	cmd.Stdin = strings.NewReader(felis.BootstrapScript())
	cmd.Env = append(os.Environ(),
		"FELIS_BOOTSTRAP_FROM_TUI=1",
		"FELIS_BOOTSTRAP_BINARY="+exe,
	)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return hostBootstrapDoneMsg{err: err}
	})
}

func runHostBootstrapTUI(ctx context.Context) (bool, error) {
	final, err := tea.NewProgram(newHostBootstrapModel(ctx)).Run()
	if err != nil {
		return false, err
	}
	m, ok := final.(*hostBootstrapModel)
	if !ok {
		return false, errors.New("unexpected bootstrap model type")
	}
	return m.completed, m.err
}
