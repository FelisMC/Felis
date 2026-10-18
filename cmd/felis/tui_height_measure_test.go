package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestWizardViewsFitTerminal(t *testing.T) {
	stages := []struct {
		name string
		msg  tea.Msg
	}{
		{"owner", preflightDoneMsg{}},
		{"connect", ownerResultMsg{username: "owner", setupTokenURL: "https://op.console.example.com/setup?token=t0ken"}},
		{"summary", connectResultMsg{method: connectLocal, panelHostname: "panel.example.com"}},
	}

	// Include narrow widths because wrapped notes can exceed the terminal height budget.
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

func TestEdgeFormFitsTerminal(t *testing.T) {
	for _, w := range []int{60, 80, 90} {
		for _, h := range []int{24, 30, 45} {
			root := newTestRoot(false, consoleModeSetup, "")
			root = drive(t, root, tea.WindowSizeMsg{Width: w, Height: h})
			root.stage = stageConnect

			edge := newEdgeModel("felis.example.com", "admin.felis.example.com", "panel.felis.example.com")
			// Pretend the operator already installed cloudflared + logged in so the
			// intro lets us open the form without a live cloudflared/cert.
			edge.cloudflaredPath = "/usr/local/bin/cloudflared"
			edge.certExists = true
			root.adopt(edge) // sizes the edge model with the post-rail budget

			// intro → form (group 1: credentials)
			root = drive(t, root, key(tea.KeyEnter))
			if edge.step != egForm {
				t.Fatalf("terminal %dx%d: enter on a ready intro should open the form, step = %v", w, h, edge.step)
			}
			// huh's title renders outside its scrolling viewport, allowing the selected group to be identified.
			if v := root.View(); !strings.Contains(v, "Step 1 · Credentials") {
				t.Fatalf("terminal %dx%d: form should open on the credentials group, got:\n%s", w, h, v)
			}
			if got := lipgloss.Height(root.View()); got > h {
				t.Errorf("terminal %dx%d: edge credentials view = %d rows (exceeds height)", w, h, got)
			}

			// Advance huh to the taller hostname group before measuring its shared form.
			edge.form.NextGroup()
			if v := root.View(); !strings.Contains(v, "Step 2 · Hostnames & identity") {
				t.Fatalf("terminal %dx%d: NextGroup should reveal the hostnames group, got:\n%s", w, h, v)
			}
			if got := lipgloss.Height(root.View()); got > h {
				t.Errorf("terminal %dx%d: edge hostnames view = %d rows (exceeds height)", w, h, got)
			}
		}
	}
}

func TestEdgeValidators(t *testing.T) {
	accountCases := []struct {
		in string
		ok bool
	}{
		{"", false},
		{"0123456789abcdef0123456789abcdef", true},
		{"0123456789ABCDEF0123456789abcdef", true},
		{"cfat_looks_like_a_token", false}, // token pasted into the account field
		{"too-short", false},
		{"0123456789abcdef0123456789abcde", false}, // 31 chars
	}
	for _, c := range accountCases {
		if err := validateAccountID(c.in); (err == nil) != c.ok {
			t.Errorf("validateAccountID(%q): ok=%v, err=%v", c.in, c.ok, err)
		}
	}

	identityCases := []struct {
		in string
		ok bool
	}{
		{"", false},
		{"@", false}, // bare @ with no domain
		{"you@example.com", true},
		{"@your-domain.com", true},
	}
	for _, c := range identityCases {
		if err := validateAccessIdentity(c.in); (err == nil) != c.ok {
			t.Errorf("validateAccessIdentity(%q): ok=%v, err=%v", c.in, c.ok, err)
		}
	}
}

// Without this, ctrl+c from the form would return to the intro and a failed setup could strand the user on the form.
func TestEdgeFormExitKeys(t *testing.T) {
	open := func(t *testing.T) *edgeModel {
		t.Helper()
		e := newEdgeModel("felis.example.com", "admin.felis.example.com", "panel.felis.example.com")
		e.cloudflaredPath = "/usr/local/bin/cloudflared"
		e.certExists = true
		next, _ := e.Update(key(tea.KeyEnter)) // ready intro → form
		em, ok := next.(*edgeModel)
		if !ok {
			t.Fatalf("intro enter returned %T, want *edgeModel", next)
		}
		if em.step != egForm {
			t.Fatalf("intro enter should open the form, step = %v", em.step)
		}
		return em
	}

	// esc steps back to the intro/status screen — not quit, not stuck on the form.
	e := open(t)
	next, _ := e.Update(key(tea.KeyEsc))
	if got := next.(*edgeModel).step; got != egIntro {
		t.Fatalf("esc on the edge form should return to the intro, step = %v", got)
	}

	// ctrl+c quits, like every sibling screen.
	e = open(t)
	_, cmd := e.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c on the edge form should return a command (tea.Quit)")
	}
	if msg := cmd(); !isQuit(msg) {
		t.Fatalf("ctrl+c command = %T, want tea.Quit", msg)
	}
}
