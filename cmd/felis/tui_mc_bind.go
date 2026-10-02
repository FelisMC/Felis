package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"felis.lolicon.best/internal/api"
)

// mcBindModel is the `felis setup` Owner-establishment screen: the operator
// joins the server, reads the one-time code the login server shows, and types it
// here. The bound Minecraft account is promoted to the passwordless Owner, and a
// one-time setup URL is minted for the first web login. It replaces the old
// ownerModel bootstrap form in setup mode — no username/email/password is typed
// here, the MC identity is the root of trust.
//
// The screen says where to join and what the code looks like, because the
// operator arrives here straight from the installer with nothing else to go on.
// A refused code returns to the form with the reason: CompleteOwnerSetup rolls
// back on every failure, so another try is always safe. An empty code offers to
// skip, and setup goes on to the connection and storage steps without an Owner.
type mcBindModel struct {
	ctx       context.Context
	store     ownerStore
	adminHost string
	osUser    string
	gameAddr  string // where to join in Minecraft; "" names no address

	step    mcBindStep
	form    *huh.Form
	sp      spinner.Model
	working string

	linkCode      string
	skip          bool   // the skip confirmation's answer
	note          string // why the last code was refused, shown above the rebuilt form
	ownerIdentity string
	setupTokenURL string
	auditWarning  string

	width, height int
}

// ownerSkippedMsg leaves the Owner step without binding one.
type ownerSkippedMsg struct{}

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

func newMCBindModel(ctx context.Context, store ownerStore, adminHost, osUser, gameAddr string) *mcBindModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiLabel
	m := &mcBindModel{
		ctx:       ctx,
		store:     store,
		adminHost: adminHost,
		osUser:    osUser,
		gameAddr:  gameAddr,
		sp:        sp,
		step:      mcBindForm,
	}
	m.form = m.buildForm()
	return m
}

// buildForm starts the code entry afresh, with the last refusal (if any) on top.
func (m *mcBindModel) buildForm() *huh.Form {
	m.linkCode, m.skip = "", false
	desc := m.instructions()
	if m.note != "" {
		desc = m.note + "\n\n" + desc
	}
	return m.sized(newFelisForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Bind the Owner's Minecraft account").
				Description(desc),
			huh.NewInput().
				Title("Link code").
				Description("Leave it empty and press enter to skip this step for now.").
				Placeholder("K7M2QX9P").
				Value(&m.linkCode).
				Validate(validLinkCode),
		),
		// Asked only for an empty code, so an enter pressed too early cannot skip.
		huh.NewGroup(
			huh.NewConfirm().
				Title("Skip the Owner for now?").
				Description("Setup goes on to the connection and storage steps. Nobody can sign in\n"+
					"to the panel until an Owner is bound: run  sudo felis setup  again to bind one.").
				Affirmative("Skip for now").
				Negative("Enter a code").
				Value(&m.skip),
		).WithHideFunc(func() bool { return normalizeLinkCode(m.linkCode) != "" }),
	))
}

// instructions is the way to a code, for an operator who has only this screen.
func (m *mcBindModel) instructions() string {
	join := ownerJoinTarget(m.gameAddr)
	if m.gameAddr != "" && !gameAddrIsIP(m.gameAddr) {
		join += "\n   (or this host's IP address while that name does not point here yet)"
	}
	return fmt.Sprintf("The Minecraft account you bind becomes the Owner and signs in to the\n"+
		"panel without a password.\n\n"+
		"1. In Minecraft (Java Edition), join  %s\n"+
		"2. The login server opens a book with your link code; chat shows it too.\n"+
		"   It is %d characters and works for %d minutes. /link prints a new one.\n"+
		"3. Type the code below. The web link in the book is for players: the\n"+
		"   Owner's code goes here.",
		join, api.LinkCodeLen, int(api.LinkCodeTTL/time.Minute))
}

// ownerJoinTarget names where to join in Minecraft to bind the Owner.
func ownerJoinTarget(gameAddr string) string {
	if gameAddr == "" {
		return "this server"
	}
	return gameAddr
}

// normalizeLinkCode is a code as the store keeps it: upper case, without the
// spaces or dashes an operator may type to group it.
func normalizeLinkCode(s string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(s)))
}

// validLinkCode catches a mistyped code before it costs a round trip. Empty is
// valid: it asks to skip.
func validLinkCode(s string) error {
	code := normalizeLinkCode(s)
	if code == "" {
		return nil
	}
	if n := utf8.RuneCountInString(code); n != api.LinkCodeLen {
		return fmt.Errorf("a link code is %d characters; this is %d", api.LinkCodeLen, n)
	}
	for _, c := range code {
		if !strings.ContainsRune(api.LinkCodeAlphabet, c) {
			return fmt.Errorf("a link code never contains %q (codes leave out I, O, 0 and 1)", c)
		}
	}
	return nil
}

// bindFailureNote says why a code was refused and what to do next.
func bindFailureNote(err error) string {
	if errors.Is(err, api.ErrLinkCodeInvalid) {
		return fmt.Sprintf("✗ That code was not accepted: it is mistyped, older than %d minutes, or\n"+
			"  already used. Type /link in Minecraft for a new one.", int(api.LinkCodeTTL/time.Minute))
	}
	return "✗ Binding failed: " + err.Error() + "\n  Nothing was changed; try again."
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
			m.step = mcBindForm
			m.note = bindFailureNote(msg.err)
			m.form = m.buildForm()
			return m, m.form.Init()
		}
		m.note = ""
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
	code := normalizeLinkCode(m.linkCode)
	if code == "" {
		if m.skip {
			return m, func() tea.Msg { return ownerSkippedMsg{} }
		}
		// "Enter a code": back to the entry, keeping any refusal on screen.
		m.form = m.buildForm()
		return m, m.form.Init()
	}
	m.step = mcBindWorking
	m.working = "Binding Minecraft account…"
	return m, tea.Batch(m.sp.Tick, func() tea.Msg {
		out, err := performSetupMCBind(m.ctx, m.store, code, m.adminHost, m.osUser)
		return mcBindMsg{outcome: out, err: err}
	})
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
		box.WriteString(tuiLabel.Render("setup URL  ") + "\n")
		for _, line := range wrapDisplayURL(m.setupTokenURL, 70) {
			box.WriteString(tuiPassword.Render(line) + "\n")
		}
		box.WriteString("\n")
		box.WriteString(tuiWarn.Render("Open this URL to complete passwordless login setup.\nIt is shown only once."))
	}
	if m.auditWarning != "" {
		box.WriteString("\n\n" + tuiWarn.Render("Audit warning: "+m.auditWarning))
	}
	b.WriteString(tuiCardStyle.Render(box.String()) + "\n\n")
	b.WriteString(tuiAction("enter", "continue"))
	return b.String()
}
