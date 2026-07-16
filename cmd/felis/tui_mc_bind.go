package main

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

// mcBindModel is the `felis setup` Owner-establishment screen: the operator
// joins the server, runs /link to get a one-time code, and types it here. The
// bound Minecraft account is promoted to the passwordless Owner, and a one-time
// setup URL is minted for the first web login. It replaces the old ownerModel
// bootstrap form in setup mode — no username/email/password is typed here, the
// MC identity is the root of trust.
type mcBindModel struct {
	ctx       context.Context
	store     ownerStore
	panelHost string
	osUser    string

	step    mcBindStep
	form    *huh.Form
	sp      spinner.Model
	working string

	linkCode      string
	ownerIdentity string
	setupTokenURL string
	auditWarning  string

	width, height int
}

type mcBindStep int

const (
	mcBindForm mcBindStep = iota
	mcBindWorking
	mcBindDone
)

type mcBindMsg struct {
	outcome breakGlassOutcome
	err     error
}

func newMCBindModel(ctx context.Context, store ownerStore, panelHost, osUser string) *mcBindModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiLabel
	m := &mcBindModel{
		ctx:       ctx,
		store:     store,
		panelHost: panelHost,
		osUser:    osUser,
		sp:        sp,
		step:      mcBindForm,
	}
	m.form = m.buildForm()
	return m
}

func (m *mcBindModel) buildForm() *huh.Form {
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewNote().
			Title("Bind your Minecraft account").
			Description("Join the server and run /link to get a one-time code,\nthen type it here. Your bound account becomes the\npasswordless Owner."),
		huh.NewInput().
			Title("Link code").
			Placeholder("ABCD12").
			Value(&m.linkCode).
			Validate(requiredField("link code")),
	)))
}

func (m *mcBindModel) sized(f *huh.Form) *huh.Form {
	if m.width > 0 {
		return f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

func (m *mcBindModel) setSize(w, h int) {
	m.width, m.height = w, h
	if m.form != nil {
		m.form = m.form.WithWidth(w).WithHeight(h)
	}
}

func (m *mcBindModel) Init() tea.Cmd { return m.form.Init() }

func (m *mcBindModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case mcBindMsg:
		if msg.err != nil {
			return m, m.failCmd(msg.err)
		}
		m.step = mcBindDone
		m.ownerIdentity = msg.outcome.ownerIdentity
		m.setupTokenURL = msg.outcome.setupTokenURL
		if msg.outcome.auditErr != nil {
			m.auditWarning = msg.outcome.auditErr.Error()
		}
		return m, nil

	case spinner.TickMsg:
		if m.step == mcBindWorking {
			var cmd tea.Cmd
			m.sp, cmd = m.sp.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		switch m.step {
		case mcBindDone:
			switch msg.String() {
			case "ctrl+c", "esc", "enter":
				return m, m.resultCmd()
			}
			return m, nil
		case mcBindWorking:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		default:
			switch msg.String() {
			case "ctrl+c", "esc":
				return m, tea.Quit
			}
		}
	}

	if m.step == mcBindForm && m.form != nil {
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

func (m *mcBindModel) onFormComplete() (tea.Model, tea.Cmd) {
	m.step = mcBindWorking
	m.working = "Binding Minecraft account…"
	code := strings.TrimSpace(strings.ToUpper(m.linkCode))
	return m, tea.Batch(m.sp.Tick, func() tea.Msg {
		out, err := performSetupMCBind(m.ctx, m.store, code, m.panelHost, m.osUser)
		return mcBindMsg{outcome: out, err: err}
	})
}

func (m *mcBindModel) failCmd(err error) tea.Cmd {
	return func() tea.Msg { return ownerResultMsg{err: err} }
}

func (m *mcBindModel) resultCmd() tea.Cmd {
	return func() tea.Msg {
		return ownerResultMsg{
			username:      m.ownerIdentity,
			setupTokenURL: m.setupTokenURL,
			mode:          "setup",
			accountable:   m.osUser,
			auditWarning:  m.auditWarning,
		}
	}
}

func (m *mcBindModel) View() string {
	switch m.step {
	case mcBindWorking:
		msg := m.working
		if msg == "" {
			msg = "Working…"
		}
		return "  " + m.sp.View() + " " + tuiHint.Render(msg) + "\n"
	case mcBindDone:
		return m.doneView()
	default:
		if m.form == nil {
			return ""
		}
		return m.form.View()
	}
}

func (m *mcBindModel) doneView() string {
	var b strings.Builder
	b.WriteString(tuiSuccessBanner("Owner account is ready.") + "\n\n")

	var box strings.Builder
	if m.ownerIdentity != "" {
		box.WriteString(tuiLabel.Render("minecraft  ") + m.ownerIdentity + "\n")
	}
	if m.setupTokenURL != "" {
		if box.Len() > 0 {
			box.WriteString("\n")
		}
		box.WriteString(tuiLabel.Render("setup URL  ") + "\n" + tuiPassword.Render(m.setupTokenURL) + "\n\n")
		box.WriteString(tuiWarn.Render("Open this URL to complete passwordless login setup.\nIt is shown only once."))
	}
	if m.auditWarning != "" {
		box.WriteString("\n\n" + tuiWarn.Render("Audit warning: "+m.auditWarning))
	}
	b.WriteString(tuiCardStyle.Render(box.String()) + "\n\n")
	b.WriteString(tuiAction("enter", "continue"))
	return b.String()
}
