package main

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

// bgOperation is the account operation the break-glass menu dispatches to. Its
// zero value is bgProvisionOwner so any ownerModel built without an explicit
// operation keeps the original "provision the Owner" behaviour — the menu and the
// operator path are strictly additive.
type bgOperation int

const (
	bgProvisionOwner bgOperation = iota
	bgAddOperator
	bgHaltServer
	bgSyncBackup
)

// menuChoiceMsg is emitted to the root once the operator picks an operation. The
// menu screen has no database handle of its own, so it reports the choice upward
// and lets the root (which holds ctx/store) build the next screen.
type menuChoiceMsg struct{ op bgOperation }

// menuModel is the thin top-level router the break-glass console opens on when a
// staff account already exists, making the account operations peers rather than
// tails of one wizard. It is shown only in recovery (adminExists): on a fresh
// machine bootstrapping the first Owner is the only sensible operation, and adding
// an Operator first would mint a staff account the login gate still rejects, so the
// console skips straight to Owner provisioning there.
type menuModel struct {
	form          *huh.Form
	choice        bgOperation
	width, height int
}

func newMenuModel() *menuModel {
	m := &menuModel{}
	m.form = m.build()
	return m
}

func (m *menuModel) build() *huh.Form {
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewSelect[bgOperation]().
			Title("Break-glass console").
			Description("Local root recovery. Choose an operation.").
			Value(&m.choice).
			Options(
				// Owner-provision is first so it is the default: it is the common
				// recovery flow, and landing on it keeps that path a single Enter.
				huh.NewOption("Provision or reset the Owner account", bgProvisionOwner),
				huh.NewOption("Add an Operator account", bgAddOperator),
				huh.NewOption("Halt a running server", bgHaltServer),
				huh.NewOption("Back up a world now (Sync)", bgSyncBackup),
			),
		// A dim footnote spelling out the one behavioural difference that matters:
		// Owner-reset re-enables local session sign-in, operator-add never touches
		// the global auth toggle.
		huh.NewNote().Description(
			"Owner reset re-enables local session sign-in. Adding an Operator mints an "+
				"additional staff admin and leaves the global auth toggle untouched."),
	)))
}

func (m *menuModel) sized(f *huh.Form) *huh.Form {
	if m.width > 0 {
		return f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

func (m *menuModel) setSize(w, h int) {
	m.width, m.height = w, h
	if m.form != nil {
		m.form = m.form.WithWidth(w).WithHeight(h)
	}
}

func (m *menuModel) Init() tea.Cmd { return m.form.Init() }

func (m *menuModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "ctrl+c", "esc":
			// Backing out of the top-level menu cancels the whole console — no account
			// is created and the durable summary reports "no changes made".
			return m, tea.Quit
		}
	}

	form, cmd := m.form.Update(msg)
	if f, ok := form.(*huh.Form); ok {
		m.form = f
	}
	switch m.form.State {
	case huh.StateCompleted:
		op := m.choice
		return m, func() tea.Msg { return menuChoiceMsg{op: op} }
	case huh.StateAborted:
		return m, tea.Quit
	}
	return m, cmd
}

func (m *menuModel) View() string { return m.form.View() }
