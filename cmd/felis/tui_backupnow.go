package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// backupModel is the break-glass "back up a world now" screen (§B4 "Sync"). It mirrors
// haltModel's shape (async load → huh pick → async work → outcome card), but the work
// step is an HTTP call to the felis-api internal face rather than a direct CRD write:
// rendering a backup Job needs felis-api's deployment coordinates. All decision logic
// lives in backupnow.go (unit-tested); this file is the untested terminal glue.
//
// It reuses listServersForHalt for the picker (the same "list servers with phase"
// need) — the API enforces the stopped-gate, so a running pick returns a friendly 409.
type backupStep int

const (
	backupLoading backupStep = iota // building the cluster client + listing servers
	backupPick                      // choosing which world to snapshot
	backupWorking                   // POSTing the internal backup endpoint
	backupDone                      // outcome card, or the empty/error terminal note
)

type backupListMsg struct {
	cl      client.Client
	servers []haltableServer
	err     error
}

type backupPerformedMsg struct {
	outcome backupNowOutcome
	err     error
}

// backupResultMsg is the terminal signal to the root: it records the outcome into
// breakGlassResult and quits. done is false for a cancel or an empty fleet.
type backupResultMsg struct {
	outcome backupNowOutcome
	done    bool
	err     error
}

type backupModel struct {
	ctx       context.Context
	namespace string // minecraft ns: where the servers live (the picker lists these)
	controlNS string // control ns: where the felis-api-internal Service + token live
	osUser    string

	step backupStep
	cl   client.Client
	sp   spinner.Model
	form *huh.Form

	servers []haltableServer
	pick    string // huh-bound selected server name
	outcome backupNowOutcome
	loadErr error
	empty   bool

	width, height int
}

func newBackupModel(ctx context.Context, namespace, controlNS, osUser string) *backupModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiLabel
	return &backupModel{ctx: ctx, namespace: namespace, controlNS: controlNS, osUser: osUser, sp: sp, step: backupLoading}
}

func (m *backupModel) Init() tea.Cmd { return tea.Batch(m.sp.Tick, m.loadCmd()) }

func (m *backupModel) loadCmd() tea.Cmd {
	return func() tea.Msg {
		cl, err := buildSystemServerClient()
		if err != nil {
			return backupListMsg{err: fmt.Errorf("connect to cluster: %w", err)}
		}
		servers, err := listServersForHalt(m.ctx, cl, m.namespace)
		if err != nil {
			return backupListMsg{err: fmt.Errorf("list servers: %w", err)}
		}
		return backupListMsg{cl: cl, servers: backupPickable(servers)}
	}
}

func (m *backupModel) setSize(w, h int) {
	m.width, m.height = w, h
	if m.form != nil {
		m.form = m.form.WithWidth(w).WithHeight(h)
	}
}

func (m *backupModel) sized(f *huh.Form) *huh.Form {
	if m.width > 0 {
		return f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

func (m *backupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case backupListMsg:
		if msg.err != nil {
			m.loadErr = msg.err
			m.step = backupDone
			return m, nil
		}
		m.cl = msg.cl
		m.servers = msg.servers
		if len(m.servers) == 0 {
			m.empty = true
			m.step = backupDone
			return m, nil
		}
		m.step = backupPick
		m.form = m.sized(m.buildPickForm())
		return m, m.form.Init()

	case backupPerformedMsg:
		m.step = backupDone
		if msg.err != nil {
			m.loadErr = msg.err
			return m, nil
		}
		m.outcome = msg.outcome
		return m, nil

	case spinner.TickMsg:
		if m.step == backupLoading || m.step == backupWorking {
			var cmd tea.Cmd
			m.sp, cmd = m.sp.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		switch m.step {
		case backupDone:
			switch msg.String() {
			case "ctrl+c", "esc", "enter":
				return m, m.exitCmd()
			}
			return m, nil
		case backupLoading, backupWorking:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		case backupPick:
			switch msg.String() {
			case "ctrl+c", "esc":
				return m, tea.Quit
			}
		}
	}

	if m.step == backupPick && m.form != nil {
		form, cmd := m.form.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			m.form = f
		}
		switch m.form.State {
		case huh.StateCompleted:
			return m.onPicked()
		case huh.StateAborted:
			return m, tea.Quit
		}
		return m, cmd
	}
	return m, nil
}

func (m *backupModel) onPicked() (tea.Model, tea.Cmd) {
	name := strings.TrimSpace(m.pick)
	m.step = backupWorking
	cl, controlNS, osUser := m.cl, m.controlNS, m.osUser
	return m, tea.Batch(m.sp.Tick, func() tea.Msg {
		out, err := performBackupNow(m.ctx, cl, controlNS, name, osUser)
		return backupPerformedMsg{outcome: out, err: err}
	})
}

func (m *backupModel) exitCmd() tea.Cmd {
	out, done, err := m.outcome, !m.empty && m.loadErr == nil, m.loadErr
	return func() tea.Msg { return backupResultMsg{outcome: out, done: done, err: err} }
}

func (m *backupModel) buildPickForm() *huh.Form {
	opts := make([]huh.Option[string], 0, len(m.servers))
	for _, s := range m.servers {
		opts = append(opts, huh.NewOption(fmt.Sprintf("%s  (%s)", s.name, s.phase), s.name))
	}
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Back up a world now").
			Description("Snapshots a STOPPED server's world through the live felis-api.").
			Value(&m.pick).
			Options(opts...),
		huh.NewNote().Description(
			"The world PVC is single-writer, so the server must be stopped. If it is still "+
				"running, halt it first, then back it up."),
	)))
}

func (m *backupModel) View() string {
	switch m.step {
	case backupLoading:
		return "  " + m.sp.View() + " " + tuiHint.Render("Connecting to the cluster…") + "\n"
	case backupWorking:
		return "  " + m.sp.View() + " " + tuiHint.Render("Requesting backup of "+strings.TrimSpace(m.pick)+"…") + "\n"
	case backupDone:
		return m.doneView()
	default:
		if m.form == nil {
			return ""
		}
		return m.form.View()
	}
}

func (m *backupModel) doneView() string {
	var b strings.Builder
	switch {
	case m.loadErr != nil:
		b.WriteString(tuiWarn.Render("Backup could not be started.") + "\n\n")
		b.WriteString(tuiCardStyle.Render(m.loadErr.Error()) + "\n\n")
	case m.empty:
		b.WriteString(tuiHint.Render("No servers to back up in namespace "+m.namespace+".") + "\n\n")
	default:
		b.WriteString(tuiSuccessBanner(m.outcome.name+" backup started.") + "\n\n")
		var box strings.Builder
		box.WriteString(tuiLabel.Render("server ") + m.outcome.name + "\n")
		box.WriteString(tuiLabel.Render("status ") + m.outcome.status)
		box.WriteString("\n\n" + tuiHint.Render(
			"A one-shot Job writes the archive asynchronously; it appears in the panel's "+
				"backups list when finished."))
		b.WriteString(tuiCardStyle.Render(box.String()) + "\n\n")
	}
	b.WriteString(tuiAction("enter", "continue"))
	return b.String()
}
