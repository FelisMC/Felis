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

// haltModel is the break-glass "halt a server" screen. It mirrors ownerModel's
// shape (async load → huh pick → async work → outcome card) but carries no auth
// branch: the console's root gate is the authority, and the accountable OS user is
// already known. All decision logic lives in halt.go (unit-tested); this file is the
// untested terminal glue, like the other console screens.
type haltStep int

const (
	haltLoading haltStep = iota // building the cluster client + listing servers
	haltPick                    // choosing which server to halt
	haltWorking                 // patching desiredState=Stopped
	haltDone                    // outcome card, or the empty/error terminal note
)

// haltListMsg carries the built cluster client and haltable server list (or the
// failure of either) back from the async load.
type haltListMsg struct {
	cl      client.Client
	servers []haltableServer
	err     error
}

type haltPerformedMsg struct {
	outcome haltOutcome
	err     error
}

// haltResultMsg is the terminal signal to the root: it records the outcome into
// breakGlassResult and quits, so cmdBreakGlass can re-print it after the alt-screen
// is torn down. done is false for a cancel or an empty fleet (no change made).
type haltResultMsg struct {
	outcome haltOutcome
	done    bool
	err     error
}

type haltModel struct {
	ctx       context.Context
	store     ownerStore
	namespace string
	osUser    string

	step haltStep
	cl   client.Client
	sp   spinner.Model
	form *huh.Form

	servers []haltableServer
	pick    string // huh-bound selected server name
	outcome haltOutcome
	loadErr error
	empty   bool

	width, height int
}

func newHaltModel(ctx context.Context, store ownerStore, namespace, osUser string) *haltModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiLabel
	return &haltModel{ctx: ctx, store: store, namespace: namespace, osUser: osUser, sp: sp, step: haltLoading}
}

func (m *haltModel) Init() tea.Cmd { return tea.Batch(m.sp.Tick, m.loadCmd()) }

// loadCmd builds the cluster client and lists haltable servers off the UI thread.
// The client build (no kubeconfig) and the list (no API) can each fail; either
// becomes the screen's terminal error rather than a panic.
func (m *haltModel) loadCmd() tea.Cmd {
	return func() tea.Msg {
		cl, err := buildSystemServerClient()
		if err != nil {
			return haltListMsg{err: fmt.Errorf("connect to cluster: %w", err)}
		}
		servers, err := listServersForHalt(m.ctx, cl, m.namespace)
		if err != nil {
			return haltListMsg{err: fmt.Errorf("list servers: %w", err)}
		}
		return haltListMsg{cl: cl, servers: servers}
	}
}

func (m *haltModel) setSize(w, h int) {
	m.width, m.height = w, h
	if m.form != nil {
		m.form = m.form.WithWidth(w).WithHeight(h)
	}
}

func (m *haltModel) sized(f *huh.Form) *huh.Form {
	if m.width > 0 {
		return f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

func (m *haltModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case haltListMsg:
		if msg.err != nil {
			m.loadErr = msg.err
			m.step = haltDone
			return m, nil
		}
		m.cl = msg.cl
		m.servers = msg.servers
		if len(m.servers) == 0 {
			m.empty = true
			m.step = haltDone
			return m, nil
		}
		m.step = haltPick
		m.form = m.sized(m.buildPickForm())
		return m, m.form.Init()

	case haltPerformedMsg:
		m.step = haltDone
		if msg.err != nil {
			m.loadErr = msg.err
			return m, nil
		}
		m.outcome = msg.outcome
		return m, nil

	case spinner.TickMsg:
		if m.step == haltLoading || m.step == haltWorking {
			var cmd tea.Cmd
			m.sp, cmd = m.sp.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		switch m.step {
		case haltDone:
			switch msg.String() {
			case "ctrl+c", "esc", "enter":
				return m, m.exitCmd()
			}
			return m, nil
		case haltLoading, haltWorking:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		case haltPick:
			switch msg.String() {
			case "ctrl+c", "esc":
				return m, tea.Quit
			}
		}
	}

	if m.step == haltPick && m.form != nil {
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

func (m *haltModel) onPicked() (tea.Model, tea.Cmd) {
	name := strings.TrimSpace(m.pick)
	m.step = haltWorking
	cl, ns, store, osUser := m.cl, m.namespace, m.store, m.osUser
	return m, tea.Batch(m.sp.Tick, func() tea.Msg {
		out, err := performHalt(m.ctx, cl, store, ns, name, osUser)
		return haltPerformedMsg{outcome: out, err: err}
	})
}

// exitCmd hands the outcome to the root: a load/perform error routes through the
// root's error path, a real halt records the durable summary, and an empty fleet is
// a benign cancel.
func (m *haltModel) exitCmd() tea.Cmd {
	out, done, err := m.outcome, !m.empty && m.loadErr == nil, m.loadErr
	return func() tea.Msg { return haltResultMsg{outcome: out, done: done, err: err} }
}

func (m *haltModel) buildPickForm() *huh.Form {
	opts := make([]huh.Option[string], 0, len(m.servers))
	for _, s := range m.servers {
		label := fmt.Sprintf("%s  (%s)", s.name, s.phase)
		if s.system {
			label += "  ⚠ system — front door"
		}
		opts = append(opts, huh.NewOption(label, s.name))
	}
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Halt a server").
			Description("Sets desiredState=Stopped; the operator reconciles a graceful shutdown.").
			Value(&m.pick).
			Options(opts...),
		huh.NewNote().Description(
			"Halting a ⚠ system server stops shared infrastructure: halting login takes "+
				"the whole auth front door down (it has no fallback)."),
	)))
}

func (m *haltModel) View() string {
	switch m.step {
	case haltLoading:
		return "  " + m.sp.View() + " " + tuiHint.Render("Connecting to the cluster…") + "\n"
	case haltWorking:
		return "  " + m.sp.View() + " " + tuiHint.Render("Halting "+strings.TrimSpace(m.pick)+"…") + "\n"
	case haltDone:
		return m.doneView()
	default:
		if m.form == nil {
			return ""
		}
		return m.form.View()
	}
}

func (m *haltModel) doneView() string {
	var b strings.Builder
	switch {
	case m.loadErr != nil:
		b.WriteString(tuiWarn.Render("Halt failed.") + "\n\n")
		b.WriteString(tuiCardStyle.Render(m.loadErr.Error()) + "\n\n")
	case m.empty:
		b.WriteString(tuiHint.Render("No servers to halt in namespace "+m.namespace+".") + "\n\n")
	default:
		b.WriteString(tuiSuccessBanner(haltHeadline(m.outcome)) + "\n\n")
		var box strings.Builder
		box.WriteString(tuiLabel.Render("server    ") + m.outcome.name + "\n")
		box.WriteString(tuiLabel.Render("namespace ") + m.outcome.namespace)
		if m.outcome.system {
			box.WriteString("\n\n" + tuiWarn.Render("This is a system server — the shared front door is now going down."))
		}
		if m.outcome.auditErr != nil {
			box.WriteString("\n\n" + tuiWarn.Render("Audit warning: "+m.outcome.auditErr.Error()))
		}
		b.WriteString(tuiCardStyle.Render(box.String()) + "\n\n")
	}
	b.WriteString(tuiAction("enter", "continue"))
	return b.String()
}

func haltHeadline(out haltOutcome) string {
	if out.alreadyStopped {
		return out.name + " was already stopped."
	}
	return out.name + " is stopping."
}
