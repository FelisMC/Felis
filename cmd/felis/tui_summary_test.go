package main

import (
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSummaryCopyLink(t *testing.T) {
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
				copyText: func(text string) (bool, error) { copied = text; return true, nil },
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

func TestSummaryCopyFailureAndTerminalFeedback(t *testing.T) {
	m := &summaryModel{
		panelURL: "https://panel.example",
		copyText: func(string) (bool, error) { return false, errors.New("open /dev/tty: no such device") },
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("C")})
	m.Update(cmd())
	if !m.copyFailed || !strings.Contains(m.View(), "no such device") || !strings.Contains(m.View(), "Select the link above") {
		t.Fatal("a copy failure must be visible, with the way to copy by hand")
	}
	m.Update(summaryCopiedMsg{})
	if m.copyFailed || !strings.Contains(m.View(), "Copy sent to your terminal") || strings.Contains(m.View(), "Link copied to clipboard") {
		t.Fatal("OSC 52 has no confirmation; do not claim the local clipboard was updated")
	}
	if _, cmd := (&summaryModel{}).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")}); cmd != nil {
		t.Fatal("a missing link must not trigger a clipboard write")
	}
}

type fakeTTY struct{ strings.Builder }

func (*fakeTTY) Close() error { return nil }

// TestCopyTerminalText covers the copy under sudo, which drops SSH_TTY and
// SSH_CONNECTION: a server without xclip or xsel must still send OSC 52 to the
// terminal instead of failing on the missing desktop clipboard.
func TestCopyTerminalText(t *testing.T) {
	const link = "https://panel.example/setup?token=test-token"
	// OSC 52 sets the system clipboard ("c") to base64 text, ended by BEL; tmux passes a
	// sequence through in a DCS "tmux;" string with every ESC doubled.
	osc52 := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(link)) + "\a"
	tmuxOSC52 := "\x1bPtmux;\x1b" + osc52 + "\x1b\\"
	noTools := errors.New("No clipboard utilities available")
	for _, tc := range []struct {
		name                   string
		display, wayland, tmux string
		desktopErr             error
		noTTY                  bool
		wantDesktop, wantErr   bool
		wantSequence           string
	}{
		{name: "SSH under sudo, no desktop tools", desktopErr: noTools, wantSequence: osc52},
		{name: "X11 desktop", display: ":0", wantDesktop: true, wantSequence: osc52},
		{name: "Wayland desktop", wayland: "wayland-0", wantDesktop: true, wantSequence: osc52},
		{name: "desktop without tools", display: ":0", desktopErr: noTools, wantSequence: osc52},
		{name: "inside tmux", tmux: "/tmp/tmux-0/default,1,0", desktopErr: noTools, wantSequence: tmuxOSC52},
		{name: "no terminal", desktopErr: noTools, noTTY: true, wantErr: true},
		{name: "no terminal, desktop copied", display: ":0", noTTY: true, wantDesktop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SSH_TTY", "")
			t.Setenv("SSH_CONNECTION", "")
			t.Setenv("DISPLAY", tc.display)
			t.Setenv("WAYLAND_DISPLAY", tc.wayland)
			t.Setenv("TMUX", tc.tmux)
			tty := &fakeTTY{}
			desktopCalls := 0
			oldTTY, oldDesktop := openTTY, desktopCopy
			t.Cleanup(func() { openTTY, desktopCopy = oldTTY, oldDesktop })
			openTTY = func() (io.WriteCloser, error) {
				if tc.noTTY {
					return nil, errors.New("open /dev/tty: no such device or address")
				}
				return tty, nil
			}
			desktopCopy = func(text string) error {
				desktopCalls++
				if text != link {
					t.Errorf("desktop clipboard got %q", text)
				}
				return tc.desktopErr
			}
			desktop, err := copyTerminalText(link)
			if desktop != tc.wantDesktop || (err != nil) != tc.wantErr {
				t.Fatalf("copyTerminalText = %v, %v; want desktop %v, error %v", desktop, err, tc.wantDesktop, tc.wantErr)
			}
			if got := tty.String(); got != tc.wantSequence {
				t.Fatalf("terminal got %q, want %q", got, tc.wantSequence)
			}
			if tc.display == "" && tc.wayland == "" && desktopCalls != 0 {
				t.Fatal("without a graphical session the desktop clipboard must not be tried")
			}
		})
	}
}
