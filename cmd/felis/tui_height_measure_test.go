package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TestWizardViewsFitTerminal guards against the regression that motivated the
// redesign: a composed view taller than the terminal makes bubbletea clip the
// top (the step rail) and read as janky. After a WindowSizeMsg the root budgets
// height across the rail and the active screen, so no stage's rendered view may
// exceed the terminal height.
func TestWizardViewsFitTerminal(t *testing.T) {
	stages := []struct {
		name string
		msg  tea.Msg
	}{
		{"owner", preflightDoneMsg{}},
		{"connect", ownerResultMsg{username: "owner", displayPassword: "hunter2pw"}},
		{"summary", connectResultMsg{method: connectLocal, panelHostname: "panel.example.com"}},
	}

	// Sweep widths too: narrow terminals wrap the long notes (e.g. the connect
	// chooser's security warning), which is exactly where height can creep back
	// over budget. 60 is about as narrow as a real terminal gets.
	for _, w := range []int{60, 80, 90} {
		for _, h := range []int{24, 30, 45} {
			var m tea.Model = newTestRoot(false, consoleModeSetup, "")
			m, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			for _, s := range stages {
				m, _ = m.Update(s.msg)
				got := lipgloss.Height(m.(*rootModel).View())
				if got > h {
					t.Errorf("terminal %dx%d: %s stage view = %d rows (exceeds height)", w, h, s.name, got)
				}
			}
		}
	}
}
