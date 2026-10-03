package main

import (
	"context"
	"errors"

	"felis.lolicon.best/internal/api"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

type setupOwnerMsg struct {
	outcome breakGlassOutcome
	err     error
}

type setupReadyMsg struct{ username string }

// setupOwnerModel waits for the panel, then issues a host-authorized login link.
// It never needs a running Minecraft client or login server.
type setupOwnerModel struct {
	ctx              context.Context
	store            ownerStore
	panelURL, osUser string
	probe            func() error
	sp               spinner.Model
	err              error
}

func newSetupOwnerModel(ctx context.Context, store ownerStore, panelURL, osUser string) *setupOwnerModel {
	sp := spinner.New()
	sp.Spinner, sp.Style = spinner.Dot, tuiLabel
	return &setupOwnerModel{ctx: ctx, store: store, panelURL: panelURL, osUser: osUser, sp: sp}
}

func (m *setupOwnerModel) Init() tea.Cmd {
	return tea.Batch(m.sp.Tick, func() tea.Msg {
		if m.probe != nil {
			if err := m.probe(); err != nil {
				return setupOwnerMsg{err: err}
			}
		}
		out, err := performSetupOwner(m.ctx, m.store, m.panelURL, m.osUser)
		return setupOwnerMsg{outcome: out, err: err}
	})
}

func (m *setupOwnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case setupOwnerMsg:
		if errors.Is(msg.err, api.ErrConflict) {
			return m, func() tea.Msg { return setupReadyMsg{username: msg.outcome.ownerUsername} }
		}
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		return m, func() tea.Msg {
			res := ownerResultMsg{username: msg.outcome.ownerUsername,
				setupTokenURL: msg.outcome.setupTokenURL, mode: "setup", accountable: m.osUser}
			if msg.outcome.auditErr != nil {
				res.auditWarning = msg.outcome.auditErr.Error()
			}
			return res
		}
	case spinner.TickMsg:
		if m.err == nil {
			var cmd tea.Cmd
			m.sp, cmd = m.sp.Update(msg)
			return m, cmd
		}
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "r", "R", "enter":
			if m.err != nil {
				m.err = nil
				return m, m.Init()
			}
		}
	}
	return m, nil
}

func (m *setupOwnerModel) View() string {
	if m.err != nil {
		return tuiWarn.Render("Panel login setup could not finish: "+m.err.Error()) + "\n\n" +
			tuiHint.Render("Minecraft is not required. Fix the reported service, then retry.") + "\n\n" +
			tuiAction("r/enter", "retry", "esc", "exit")
	}
	return "  " + m.sp.View() + " " + tuiHint.Render("Preparing your first panel login…") + "\n"
}
