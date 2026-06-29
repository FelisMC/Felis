package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
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

// ownerModel collects the owner account. The input phases (admin auth, root
// override, owner details) are huh forms; the async phases (verifying,
// provisioning) show a spinner; the done phase shows the credential card. The
// outward contract is unchanged: it emits an ownerResultMsg when finished.
type ownerModel struct {
	ctx         context.Context
	store       ownerStore
	osUser      string
	adminExists bool
	mode        string // "bootstrap", "recovery", "root_override"
	accountable string
	attempt     string

	step    owStep
	form    *huh.Form
	sp      spinner.Model
	working string

	width, height int

	// huh-bound form values
	authUser     string
	authPass     string
	overrideTok  string
	ownerUser    string
	ownerEmail   string
	ownerPass    string
	ownerConfirm string

	username        string
	displayPassword string
	auditWarning    string
}

func newOwnerModel(ctx context.Context, store ownerStore, osUser string, adminExists bool) *ownerModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiLabel

	m := &ownerModel{
		ctx:         ctx,
		store:       store,
		osUser:      osUser,
		adminExists: adminExists,
		sp:          sp,
		ownerUser:   "owner",
	}
	if adminExists {
		m.step = owAuth
		m.form = m.buildAuthForm()
	} else {
		m.mode = "bootstrap"
		m.accountable = osUser
		m.step = owProvision
		m.form = m.buildProvisionForm()
	}
	return m
}

func (m *ownerModel) Init() tea.Cmd { return m.form.Init() }

func (m *ownerModel) setSize(w, h int) {
	m.width, m.height = w, h
	if m.form != nil {
		m.form = m.form.WithWidth(w).WithHeight(h)
	}
}

// sized applies the current terminal area to a freshly built form so phase
// transitions don't reset back to huh's default 80-column layout.
func (m *ownerModel) sized(f *huh.Form) *huh.Form {
	if m.width > 0 {
		return f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

func (m *ownerModel) isFormStep() bool {
	return m.step == owAuth || m.step == owOverride || m.step == owProvision
}

func (m *ownerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case owAuthMsg:
		if msg.err != nil {
			return m, m.failCmd(msg.err)
		}
		if msg.ok {
			m.mode = "recovery"
			m.accountable = msg.matched
			m.step = owProvision
			m.form = m.sized(m.buildProvisionForm())
			return m, m.form.Init()
		}
		m.step = owOverride
		m.form = m.sized(m.buildOverrideForm())
		return m, m.form.Init()

	case owProvisionMsg:
		if msg.err != nil {
			return m, m.failCmd(msg.err)
		}
		m.step = owDone
		m.displayPassword = msg.outcome.displayPassword
		if msg.outcome.auditErr != nil {
			m.auditWarning = msg.outcome.auditErr.Error()
		}
		return m, nil

	case spinner.TickMsg:
		if m.step == owWorking {
			var cmd tea.Cmd
			m.sp, cmd = m.sp.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		switch m.step {
		case owDone:
			switch msg.String() {
			case "ctrl+c", "esc", "enter":
				return m, m.ownerResultCmd()
			}
			return m, nil
		case owWorking:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		default: // form steps — intercept cancel/back, let huh handle the rest
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				if m.step == owOverride {
					m.step = owAuth
					m.form = m.sized(m.buildAuthForm())
					return m, m.form.Init()
				}
				return m, tea.Quit
			}
		}
	}

	// Drive the active form.
	if m.isFormStep() && m.form != nil {
		form, cmd := m.form.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			m.form = f
		}
		switch m.form.State {
		case huh.StateCompleted:
			return m.onFormComplete()
		case huh.StateAborted:
			return m, tea.Quit
		}
		return m, cmd
	}
	return m, nil
}

func (m *ownerModel) onFormComplete() (tea.Model, tea.Cmd) {
	switch m.step {
	case owAuth:
		m.attempt = strings.TrimSpace(m.authUser)
		m.step = owWorking
		m.working = "Verifying admin credential…"
		user, pass := m.authUser, m.authPass
		return m, tea.Batch(m.sp.Tick, func() tea.Msg {
			matched, ok, err := authenticateAdmin(m.ctx, m.store, user, pass)
			return owAuthMsg{matched: matched, ok: ok, err: err}
		})
	case owOverride:
		m.mode = "root_override"
		m.accountable = m.osUser
		m.step = owProvision
		m.form = m.sized(m.buildProvisionForm())
		return m, m.form.Init()
	case owProvision:
		m.username = strings.TrimSpace(m.ownerUser)
		m.step = owWorking
		m.working = "Provisioning Owner account…"
		return m, tea.Batch(m.sp.Tick, m.provisionCmd())
	}
	return m, nil
}

func (m *ownerModel) provisionCmd() tea.Cmd {
	password := ""
	if m.mode == "bootstrap" {
		password = m.ownerPass
	}
	op := breakGlassOp{
		mode:           m.mode,
		accountable:    m.accountable,
		osUser:         m.osUser,
		ownerUsername:  m.username,
		ownerEmail:     m.ownerEmail,
		ownerPassword:  password,
		attemptedAdmin: m.attempt,
	}
	return func() tea.Msg {
		out, err := performBreakGlass(m.ctx, m.store, op)
		return owProvisionMsg{outcome: out, err: err}
	}
}

func (m *ownerModel) failCmd(err error) tea.Cmd {
	return func() tea.Msg { return ownerResultMsg{err: err} }
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

// ---- form builders ----

func (m *ownerModel) buildAuthForm() *huh.Form {
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewNote().
			Title("Admin authentication").
			Description("A staff account already exists. Identify yourself to continue."),
		huh.NewInput().
			Title("Admin username").
			Value(&m.authUser).
			Validate(requiredField("admin username")),
		huh.NewInput().
			Title("Admin password").
			EchoMode(huh.EchoModePassword).
			Value(&m.authPass).
			Validate(requiredField("admin password")),
	)))
}

func (m *ownerModel) buildOverrideForm() *huh.Form {
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewNote().
			Title("Root override").
			Description(fmt.Sprintf("That credential did not match. Proceed as OS user %q with root authority by typing the confirmation token.", m.osUser)),
		huh.NewInput().
			Title("Type "+breakGlassOverrideToken+" to confirm").
			Value(&m.overrideTok).
			Validate(func(s string) error {
				if s != breakGlassOverrideToken {
					return errors.New("type " + breakGlassOverrideToken + " exactly to proceed")
				}
				return nil
			}),
	)))
}

func (m *ownerModel) buildProvisionForm() *huh.Form {
	desc := fmt.Sprintf("Create the first Owner — recorded as OS user %q.", m.osUser)
	switch m.mode {
	case "recovery":
		desc = fmt.Sprintf("Authenticated as %q — a one-time password will be generated.", m.accountable)
	case "root_override":
		desc = "Root override — a one-time password will be generated."
	}

	fields := []huh.Field{
		huh.NewNote().Title("Owner account").Description(desc),
		huh.NewInput().
			Title("Owner username").
			Value(&m.ownerUser).
			Validate(requiredField("owner username")),
		huh.NewInput().
			Title("Owner email").
			Description("optional").
			Placeholder("you@example.com").
			Value(&m.ownerEmail),
	}
	if m.mode == "bootstrap" {
		fields = append(fields,
			huh.NewInput().
				Title("Owner password").
				Description("at least 8 characters").
				EchoMode(huh.EchoModePassword).
				Value(&m.ownerPass).
				Validate(validateOwnerPassword),
			huh.NewInput().
				Title("Confirm password").
				EchoMode(huh.EchoModePassword).
				Value(&m.ownerConfirm).
				Validate(func(s string) error {
					if s != m.ownerPass {
						return errors.New("the two passwords do not match")
					}
					return nil
				}),
		)
	}
	return m.sized(newFelisForm(huh.NewGroup(fields...)))
}

func requiredField(name string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New(name + " is required")
		}
		return nil
	}
}

// ---- views ----

func (m *ownerModel) View() string {
	switch m.step {
	case owWorking:
		msg := m.working
		if msg == "" {
			msg = "Working…"
		}
		return "  " + m.sp.View() + " " + tuiHint.Render(msg) + "\n"
	case owDone:
		return m.doneView()
	default:
		if m.form == nil {
			return ""
		}
		return m.form.View()
	}
}

func (m *ownerModel) doneView() string {
	var b strings.Builder
	b.WriteString(tuiSuccessBanner("Owner account is ready.") + "\n\n")

	var box strings.Builder
	box.WriteString(tuiLabel.Render("username  ") + m.username + "\n")
	if m.displayPassword != "" {
		box.WriteString(tuiLabel.Render("password  ") + tuiPassword.Render(m.displayPassword) + "\n\n")
		box.WriteString(tuiWarn.Render("Record this password — it is shown only once."))
	} else {
		box.WriteString(tuiHint.Render("Log in with the password you entered."))
	}
	if m.auditWarning != "" {
		box.WriteString("\n\n" + tuiWarn.Render("Audit warning: "+m.auditWarning))
	}
	b.WriteString(tuiCardStyle.Render(box.String()) + "\n\n")
	b.WriteString(tuiAction("enter", "continue"))
	return b.String()
}
