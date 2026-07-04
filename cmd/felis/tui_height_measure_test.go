package main

import (
	"strings"
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
		{"connect", ownerResultMsg{username: "owner", setupTokenURL: "https://op.console.example.com/setup?token=t0ken"}},
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

// TestEdgeFormFitsTerminal covers the screen the existing sweep can't reach: the
// Cloudflare edge form. The chooser only adopts the edge model when "Cloudflare"
// is picked, and its form only opens once cloudflared + the login cert are
// present — neither is available in CI — so the connectLocal sweep above never
// constructs it. Here we adopt a cloudflared-satisfied edge model through the
// root (so the real step-rail chrome is in the budget) and measure BOTH form
// groups: credentials (group 1) and the taller hostnames group (group 2, five
// inputs). huh clamps each group to a scrolling viewport, but a tall group title
// or help footer can still push the composed view past the terminal — which is
// exactly the clipping regression this guards.
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
			// huh clamps every group to a scrolling viewport sized to the budget, so
			// the rendered height is the same whether the viewport content has been
			// built yet or not — measuring the freshly-opened frame is a faithful
			// height check. The group title renders outside the viewport, so it is
			// present even before content builds, which is how we confirm we are on
			// the right group.
			if v := root.View(); !strings.Contains(v, "Step 1 · Credentials") {
				t.Fatalf("terminal %dx%d: form should open on the credentials group, got:\n%s", w, h, v)
			}
			if got := lipgloss.Height(root.View()); got > h {
				t.Errorf("terminal %dx%d: edge credentials view = %d rows (exceeds height)", w, h, got)
			}

			// Advance to the taller hostnames group (5 inputs). huh advances groups
			// natively when the current group has no errors; NextGroup mutates the
			// form in place, and edge holds the same form pointer the root renders.
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

// TestEdgeValidators locks the input-validation logic the edge form relies on —
// the part that decides whether a friend's real run is accepted or rejected
// before any Cloudflare call. These are the validators wired into the huh fields;
// testing them directly is honest coverage that does not depend on driving huh.
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

// TestEdgeFormExitKeys locks the form's exit keys, which huh cannot disambiguate
// on its own: it collapses esc and ctrl+c into a single StateAborted, so the edge
// model must intercept them before delegating. esc steps back to the intro/status
// screen (matching reverseProxyModel's esc=back); ctrl+c quits like every sibling
// screen. Without this, ctrl+c from the form would fall through huh's abort path
// and return to the intro instead of quitting, and a failed setup could strand the
// user on the form. The keys only matter live, so the contract lives here.
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
