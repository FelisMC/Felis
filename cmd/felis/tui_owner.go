package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type owAuthMsg struct {
	matched string
	ok      bool
	err     error
}

type owProvisionMsg struct {
	outcome breakGlassOutcome
	err     error
}

type owStep int

const (
	owAuth owStep = iota
	owOverride
	owProvision
	owWorking
	owDone
	owError
)

type ownerModel struct {
	ctx         context.Context
	store       ownerStore
	osUser      string
	adminExists bool
	mode        string // "bootstrap", "recovery", "root_override"
	accountable string

	step    owStep
	inputs  []textinput.Model
	focus   int
	formErr string
	working string
	attempt string

	username        string
	displayPassword string
	auditWarning    string
}

func newOwnerModel(ctx context.Context, store ownerStore, osUser string, adminExists bool) *ownerModel {
	m := &ownerModel{
		ctx:         ctx,
		store:       store,
		osUser:      osUser,
		adminExists: adminExists,
	}
	if adminExists {
		m.step = owAuth
	} else {
		m.mode = "bootstrap"
		m.accountable = osUser
		m.step = owProvision
	}
	return m
}

func (m *ownerModel) Init() tea.Cmd {
	if m.step == owAuth {
		return m.buildAuth()
	}
	return m.buildProvision(true)
}

func (m *ownerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case owAuthMsg:
		m.working = ""
		if msg.err != nil {
			return m, func() tea.Msg { return ownerResultMsg{err: msg.err} }
		}
		if msg.ok {
			m.mode = "recovery"
			m.accountable = msg.matched
			m.step = owProvision
			return m, m.buildProvision(false)
		}
		m.step = owOverride
		m.formErr = ""
		return m, m.buildOverride()

	case owProvisionMsg:
		if msg.err != nil {
			return m, func() tea.Msg { return ownerResultMsg{err: msg.err} }
		}
		m.step = owDone
		m.displayPassword = msg.outcome.displayPassword
		if msg.outcome.auditErr != nil {
			m.auditWarning = msg.outcome.auditErr.Error()
		}
		return m, nil

	case tea.KeyMsg:
		switch m.step {
		case owDone, owError:
			switch msg.String() {
			case "ctrl+c", "esc", "enter":
				if m.step == owDone {
					return m, m.ownerResultCmd()
				}
				return m, nil
			}
			return m, nil
		case owWorking:
			return m, nil
		default:
			return m.handleFormKey(msg)
		}
	}
	// Forward to inputs.
	if m.step != owWorking && m.step != owDone && m.step != owError {
		return m, m.updateInputs(msg)
	}
	return m, nil
}

func (m *ownerModel) View() string {
	var b strings.Builder
	b.WriteString(tuiHeader("Owner Account"))

	switch m.step {
	case owAuth:
		b.WriteString(tuiHint.Render("A staff account exists. Identify yourself before proceeding.") + "\n\n")
		b.WriteString(tuiWizardCard("Admin Authentication", "",
			tuiFormField("Admin username", m.inputs[0])+"\n\n"+
				tuiFormField("Admin password", m.inputs[1])))
		if m.formErr != "" {
			b.WriteString("\n" + tuiErrorBanner(m.formErr) + "\n")
		}
		b.WriteString("\n" + tuiSeparator() + "\n")
		b.WriteString(tuiAction("tab/↑↓", "move", "enter", "verify", "esc", "cancel"))

	case owOverride:
		b.WriteString(tuiErrorBanner("That credential did not match.") + "\n\n")
		b.WriteString(tuiHint.Render(fmt.Sprintf("You can proceed as OS user %q with root authority.", m.osUser)) + "\n\n")
		b.WriteString(tuiWizardCard("Root Override", "",
			tuiFormField("Type "+breakGlassOverrideToken+" to confirm", m.inputs[0])))
		if m.formErr != "" {
			b.WriteString("\n" + tuiErrorBanner(m.formErr) + "\n")
		}
		b.WriteString("\n" + tuiSeparator() + "\n")
		b.WriteString(tuiAction("enter", "confirm", "esc", "go back"))

	case owProvision:
		if m.mode == "bootstrap" {
			b.WriteString(tuiHint.Render(fmt.Sprintf("Creating the first Owner. Recorded as OS user %q.", m.osUser)) + "\n\n")
		} else if m.mode == "root_override" {
			b.WriteString(tuiWarn.Render("Root override — a one-time password will be generated.") + "\n\n")
		} else {
			b.WriteString(tuiHint.Render(fmt.Sprintf("Authenticated as %q — a one-time password will be generated.", m.accountable)) + "\n\n")
		}
		var fields string
		fields = tuiFormField("Owner username", m.inputs[0]) + "\n\n"
		fields += tuiFormField("Owner email (optional)", m.inputs[1])
		if m.mode == "bootstrap" {
			fields += "\n\n" + tuiFormField("Owner password", m.inputs[2])
			fields += "\n\n" + tuiFormField("Confirm password", m.inputs[3])
		}
		b.WriteString(tuiWizardCard("Account Details", "", fields))
		if m.formErr != "" {
			b.WriteString("\n" + tuiErrorBanner(m.formErr) + "\n")
		}
		b.WriteString("\n" + tuiSeparator() + "\n")
		b.WriteString(tuiAction("tab/↑↓", "move", "enter", "provision", "esc", "cancel"))

	case owWorking:
		msg := m.working
		if msg == "" {
			msg = "Working…"
		}
		b.WriteString(tuiHint.Render(msg) + "\n")

	case owDone:
		b.WriteString(tuiSuccessBanner("Owner account is ready.") + "\n\n")
		var box strings.Builder
		box.WriteString(tuiLabel.Render("username  ") + m.username + "\n")
		if m.displayPassword != "" {
			box.WriteString(tuiLabel.Render("password  ") + tuiPassword.Render(m.displayPassword) + "\n\n")
			box.WriteString(tuiWarn.Render("Record this password — it is shown only once.") + "\n")
		} else {
			box.WriteString(tuiHint.Render("Log in with the password you entered.") + "\n")
		}
		if m.auditWarning != "" {
			box.WriteString("\n" + tuiWarn.Render("Audit warning: "+m.auditWarning) + "\n")
		}
		b.WriteString(tuiCardStyle.Render(box.String()) + "\n\n")
		b.WriteString(tuiSeparator() + "\n")
		b.WriteString(tuiAction("enter/esc", "back"))

	case owError:
		b.WriteString(tuiErrorBanner("Owner provisioning failed.") + "\n")
		b.WriteString("\n" + tuiSeparator() + "\n")
		b.WriteString(tuiAction("esc", "exit"))
	}
	return b.String()
}

func (m *ownerModel) handleFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.step == owOverride {
			m.step, m.formErr = owAuth, ""
			return m, m.buildAuth()
		}
		return m, func() tea.Msg { return switchToDashboard{} }
	case "tab", "down":
		m.focus = m.focus + 1
		if m.focus >= len(m.inputs) {
			m.focus = 0
		}
		return m, m.focusInput(m.focus)
	case "shift+tab", "up":
		m.focus = m.focus - 1
		if m.focus < 0 {
			m.focus = len(m.inputs) - 1
		}
		return m, m.focusInput(m.focus)
	case "enter":
		return m.submit()
	}
	return m, m.updateInputs(msg)
}

func (m *ownerModel) focusInput(i int) tea.Cmd {
	var cmd tea.Cmd
	for j := range m.inputs {
		if j == i {
			cmd = m.inputs[j].Focus()
		} else {
			m.inputs[j].Blur()
		}
	}
	return cmd
}

func (m *ownerModel) updateInputs(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, len(m.inputs))
	for i := range m.inputs {
		m.inputs[i], cmds[i] = m.inputs[i].Update(msg)
	}
	return tea.Batch(cmds...)
}

func (m *ownerModel) ownerResultCmd() tea.Cmd {
	return func() tea.Msg {
		return ownerResultMsg{
			username:        m.username,
			displayPassword: m.displayPassword,
			mode:            m.mode,
			accountable:     m.accountable,
			auditWarning:    m.auditWarning,
		}
	}
}

func (m *ownerModel) setInputs(ins []textinput.Model) tea.Cmd {
	m.inputs = ins
	m.focus = 0
	return m.focusInput(0)
}

func (m *ownerModel) buildAuth() tea.Cmd {
	user := tuiInput("admin username", 64, false)
	pass := tuiInput("admin password", 128, true)
	return m.setInputs([]textinput.Model{user, pass})
}

func (m *ownerModel) buildOverride() tea.Cmd {
	confirm := tuiInput("type "+breakGlassOverrideToken, 16, false)
	return m.setInputs([]textinput.Model{confirm})
}

func (m *ownerModel) buildProvision(withPassword bool) tea.Cmd {
	user := tuiInput("owner", 64, false)
	user.SetValue("owner")
	email := tuiInput("(optional)", 254, false)
	ins := []textinput.Model{user, email}
	if withPassword {
		ins = append(ins, tuiInput("at least 8 characters", 128, true))
		ins = append(ins, tuiInput("re-enter password", 128, true))
	}
	return m.setInputs(ins)
}

func (m *ownerModel) submit() (tea.Model, tea.Cmd) {
	switch m.step {
	case owAuth:
		return m.submitAuth()
	case owOverride:
		return m.submitOverride()
	case owProvision:
		return m.submitProvision()
	}
	return m, nil
}

func (m *ownerModel) submitAuth() (tea.Model, tea.Cmd) {
	user := strings.TrimSpace(m.inputs[0].Value())
	pass := m.inputs[1].Value()
	if user == "" || pass == "" {
		m.formErr = "enter the username and password of an existing admin"
		return m, nil
	}
	m.attempt = user
	m.formErr, m.working = "", "Verifying admin credential…"
	m.step = owWorking
	return m, func() tea.Msg {
		matched, ok, err := authenticateAdmin(m.ctx, m.store, user, pass)
		return owAuthMsg{matched: matched, ok: ok, err: err}
	}
}

func (m *ownerModel) submitOverride() (tea.Model, tea.Cmd) {
	if m.inputs[0].Value() != breakGlassOverrideToken {
		m.formErr = "type " + breakGlassOverrideToken + " exactly to proceed"
		return m, nil
	}
	m.mode = "root_override"
	m.accountable = m.osUser
	m.step = owProvision
	return m, m.buildProvision(false)
}

func (m *ownerModel) submitProvision() (tea.Model, tea.Cmd) {
	owner := strings.TrimSpace(m.inputs[0].Value())
	if owner == "" {
		m.formErr = "owner username is required"
		return m, m.focusForField(0)
	}
	email := m.inputs[1].Value()
	password := ""
	if m.mode == "bootstrap" {
		pw := m.inputs[2].Value()
		confirm := m.inputs[3].Value()
		if err := validateOwnerPassword(pw); err != nil {
			m.formErr = err.Error()
			return m, m.focusForField(2)
		}
		if pw != confirm {
			m.formErr = "the two passwords do not match"
			return m, m.focusForField(3)
		}
		password = pw
	}
	m.username = owner
	m.formErr, m.working = "", "Provisioning Owner account…"
	m.step = owWorking
	return m, func() tea.Msg {
		out, err := performBreakGlass(m.ctx, m.store, breakGlassOp{
			mode:           m.mode,
			accountable:    m.accountable,
			osUser:         m.osUser,
			ownerUsername:  owner,
			ownerEmail:     email,
			ownerPassword:  password,
			attemptedAdmin: m.attempt,
		})
		return owProvisionMsg{outcome: out, err: err}
	}
}

func (m *ownerModel) focusForField(i int) tea.Cmd {
	m.focus = i
	return m.focusInput(i)
}
