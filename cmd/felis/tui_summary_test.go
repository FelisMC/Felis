package main

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSummaryCopyLink(t *testing.T) {
	t.Setenv("SSH_TTY", "")
	t.Setenv("SSH_CONNECTION", "")
	for _, tc := range []struct {
		name, setup, want string
	}{
		{"first login", "https://panel.example/setup?token=test-token", "https://panel.example/setup?token=test-token"},
		{"already set up", "", "https://panel.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var copied string
			m := &summaryModel{
				panelURL: "https://panel.example", setupTokenURL: tc.setup,
				copyText: func(text string) error { copied = text; return nil },
			}
			_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
			if cmd == nil {
				t.Fatal("copy did not return a command")
			}
			msg := cmd()
			if _, ok := msg.(summaryCopiedMsg); !ok {
				t.Fatalf("c returned %T; must copy, not reconfigure", msg)
			}
			m.Update(msg)
			if copied != tc.want || m.copyFailed || !strings.Contains(m.View(), "Link copied to clipboard.") {
				t.Fatalf("copied %q, notice %q", copied, m.copyNotice)
			}
			_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
			if _, ok := cmd().(reconfigureConnectMsg); !ok {
				t.Fatal("n must still allow changing the connection")
			}
		})
	}
}

func TestSummaryCopyFailureAndSSHFeedback(t *testing.T) {
	t.Setenv("SSH_TTY", "/dev/pts/1")
	m := &summaryModel{
		panelURL: "https://panel.example",
		copyText: func(string) error { return errors.New("clipboard unavailable") },
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("C")})
	m.Update(cmd())
	if !m.copyFailed || !strings.Contains(m.View(), "clipboard unavailable") {
		t.Fatal("copy failure must be visible")
	}
	m.Update(summaryCopiedMsg{})
	if m.copyFailed || !strings.Contains(m.View(), "Copy sent to terminal") || strings.Contains(m.View(), "Link copied to clipboard") {
		t.Fatal("OSC 52 has no confirmation; do not claim the local clipboard was updated")
	}
	if _, cmd := (&summaryModel{}).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")}); cmd != nil {
		t.Fatal("a missing link must not trigger a clipboard write")
	}
}
