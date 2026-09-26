package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"felis.lolicon.best/internal/api"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

type owAuthMsg struct {
	start recoveryStart
	err   error
}

type owProvisionMsg struct {
	outcome breakGlassOutcome
	err     error
}

type owStep int

const (
	owAuth owStep = iota
	owOverride
	owCode // typing the recovery code mailed to the named admin
	owProvision
	owWorking
	owDone
	owError
)

// ownerModel collects the owner account. The input phases (admin name, recovery
// code, root override, owner details) are huh forms; the async phases (verifying,
// provisioning) show a spinner; the done phase shows the credential card. The
// outward contract is unchanged: it emits an ownerResultMsg when finished.
//
// The same model serves the Add-Operator break-glass operation: the Owner and
// Operator flows are identical in shape (authenticate or override → collect a
// username → provision → show a one-time credential), so an operation discriminator
// switches the few differences (which provision function runs, the on-screen
// labels, whether a username is defaulted) rather than forking a near-duplicate
// model. The zero value, bgProvisionOwner, is the original Owner behaviour.
type ownerModel struct {
	ctx         context.Context
	store       ownerStore
	osUser      string
	adminExists bool
	operation   bgOperation
	mode        string // "bootstrap", "recovery", "root_override"
	accountable string
	attempt     string
	recovery    recoveryConfig

	// Recovery proof: the admin the typed name resolved to, the code mailed to it,
	// and once settled either how it was proven or why the run fell back to the
	// override.
	admin      *api.StaffUser
	code       *recoveryCode
	codeNote   string // "wrong code" line shown above a rebuilt code form
	verifiedBy string
	codeSentTo string
	skip       string // otpSkip*
	skipDetail string

	step         owStep
	form         *huh.Form
	sp           spinner.Model
	working      string
	provisionErr error // last provision failure routed back to the form (operator name clash)

	width, height int

	// huh-bound form values
	authUser    string
	codeInput   string
	overrideTok string
	ownerUser   string
	ownerEmail  string

	username      string
	setupTokenURL string
	auditWarning  string
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

// newOperatorModel builds the model for the Add-Operator break-glass operation. It
// always starts at admin authentication: adding an Operator presupposes an existing
// admin (that is why the menu only offers it when one exists), so there is no
// bootstrap branch. The username is left empty
// on purpose — defaulting it to "owner" (as the Owner flow does) would make the
// happy path insert a duplicate and hit ErrConflict on every attempt.
func newOperatorModel(ctx context.Context, store ownerStore, osUser string) *ownerModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiLabel

	m := &ownerModel{
		ctx:         ctx,
		store:       store,
		osUser:      osUser,
		adminExists: true,
		operation:   bgAddOperator,
		sp:          sp,
	}
	m.step = owAuth
	m.form = m.buildAuthForm()
	return m
}

// withRecovery hands the model the relay its recovery codes go through.
func (m *ownerModel) withRecovery(r recoveryConfig) *ownerModel {
	m.recovery = r
	return m
}

func (m *ownerModel) Init() tea.Cmd { return m.form.Init() }

// subject is the human label for the account being provisioned, branching every
// on-screen string and the durable summary between the two operations.
func (m *ownerModel) subject() string {
	if m.operation == bgAddOperator {
		return "Operator"
	}
	return "Owner"
}

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
	return m.step == owAuth || m.step == owOverride || m.step == owCode || m.step == owProvision
}

func (m *ownerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case owAuthMsg:
		if msg.err != nil {
			return m, m.failCmd(msg.err)
		}
		m.admin = msg.start.admin
		if msg.start.code != nil {
			m.code = msg.start.code
			m.codeNote = ""
			m.step = owCode
			m.form = m.sized(m.buildCodeForm())
			return m, m.form.Init()
		}
		return m.toOverride(msg.start.skip, msg.start.detail)

	case owProvisionMsg:
		if msg.err != nil {
			// api.ErrConflict marks the two recoverable refusals: a taken Operator
			// username (insert-only clash) and an Owner reset naming anything but the
			// occupied seat (ownerSeatTakenError Is ErrConflict). Route back to the
			// form with a note so the operator can retype, rather than tearing down
			// the console — any other error is a genuine fault and still ends the
			// session.
			if errors.Is(msg.err, api.ErrConflict) {
				m.provisionErr = msg.err
				m.step = owProvision
				m.form = m.sized(m.buildProvisionForm())
				return m, m.form.Init()
			}
			return m, m.failCmd(msg.err)
		}
		m.step = owDone
		m.setupTokenURL = msg.outcome.setupTokenURL
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
				if m.step == owOverride || m.step == owCode {
					// Start over: a code mailed for the old attempt dies with it.
					m.admin, m.code, m.skip, m.skipDetail = nil, nil, "", ""
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
		m.working = "Sending a recovery code…"
		user, rc, osUser, op := m.authUser, m.recovery, m.osUser, m.operation
		return m, tea.Batch(m.sp.Tick, func() tea.Msg {
			start, err := beginRecovery(m.ctx, m.store, rc, user, osUser, op)
			return owAuthMsg{start: start, err: err}
		})
	case owCode:
		typed := strings.TrimSpace(m.codeInput)
		m.codeInput = ""
		if typed == breakGlassOverrideToken {
			m.skip, m.skipDetail = otpSkipByOperator, ""
			m.code = nil
			return m.proceedAsRoot()
		}
		switch m.code.check(typed, m.recovery.clock()) {
		case codeAccepted:
			m.mode = "recovery"
			m.accountable = m.admin.Username
			m.verifiedBy = verifiedByEmailOTP
			m.codeSentTo = m.admin.Email
			m.code = nil
			m.step = owProvision
			m.form = m.sized(m.buildProvisionForm())
			return m, m.form.Init()
		case codeWrong:
			m.codeNote = fmt.Sprintf("That code is wrong. %d attempts left.", m.code.attemptsLeft())
			m.form = m.sized(m.buildCodeForm())
			return m, m.form.Init()
		case codeExpired:
			return m.toOverride(otpSkipCodeExpired, "")
		default:
			return m.toOverride(otpSkipCodeRejected, fmt.Sprintf("%d wrong codes", recoveryCodeAttempts))
		}
	case owOverride:
		return m.proceedAsRoot()
	case owProvision:
		m.username = strings.TrimSpace(m.ownerUser)
		m.step = owWorking
		m.working = "Provisioning " + m.subject() + " account…"
		return m, tea.Batch(m.sp.Tick, m.provisionCmd())
	}
	return m, nil
}

// toOverride records why no code proved an admin and asks for the typed OVERRIDE.
func (m *ownerModel) toOverride(skip, detail string) (tea.Model, tea.Cmd) {
	m.skip, m.skipDetail = skip, detail
	m.code = nil
	m.step = owOverride
	m.form = m.sized(m.buildOverrideForm())
	return m, m.form.Init()
}

// proceedAsRoot is the typed OVERRIDE: the run goes on as the OS user, unverified.
func (m *ownerModel) proceedAsRoot() (tea.Model, tea.Cmd) {
	m.mode = "root_override"
	m.accountable = m.osUser
	m.step = owProvision
	m.form = m.sized(m.buildProvisionForm())
	return m, m.form.Init()
}

func (m *ownerModel) provisionCmd() tea.Cmd {
	op := breakGlassOp{
		mode:           m.mode,
		accountable:    m.accountable,
		osUser:         m.osUser,
		ownerUsername:  m.username,
		ownerEmail:     m.ownerEmail,
		attemptedAdmin: m.attempt,
		verifiedBy:     m.verifiedBy,
		codeSentTo:     m.codeSentTo,
		otpSkipped:     m.skip,
		otpSkipDetail:  m.skipDetail,
	}
	// performAddOperator and performBreakGlass share a signature; the operation
	// discriminator selects which one runs. The operator path is insert-only and
	// never flips local auth (see performAddOperator); the Owner path upserts and
	// enables local session sign-in.
	perform := performBreakGlass
	if m.operation == bgAddOperator {
		perform = performAddOperator
	}
	return func() tea.Msg {
		out, err := perform(m.ctx, m.store, op)
		return owProvisionMsg{outcome: out, err: err}
	}
}

func (m *ownerModel) failCmd(err error) tea.Cmd {
	return func() tea.Msg { return ownerResultMsg{err: err} }
}

func (m *ownerModel) ownerResultCmd() tea.Cmd {
	return func() tea.Msg {
		return ownerResultMsg{
			username:      m.username,
			setupTokenURL: m.setupTokenURL,
			mode:          m.mode,
			accountable:   m.accountable,
			auditWarning:  m.auditWarning,
			isOperator:    m.operation == bgAddOperator,
		}
	}
}

// ---- form builders ----

func (m *ownerModel) buildAuthForm() *huh.Form {
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewNote().
			Title("Admin authentication").
			Description("A staff account already exists. Name yours: a one-time code goes to its verified email address."),
		huh.NewInput().
			Title("Admin username").
			Value(&m.authUser).
			Validate(requiredField("admin username")),
	)))
}

func (m *ownerModel) buildCodeForm() *huh.Form {
	desc := fmt.Sprintf("A recovery code went to %s, the verified address of %q. It works for %d minutes.\n\n"+
		"No mail? Type %s to go on as OS user %q with root authority; the audit log records that as an unverified override. Esc starts over.",
		maskEmail(m.admin.Email), m.admin.Username, int(recoveryCodeTTL/time.Minute), breakGlassOverrideToken, m.osUser)
	if m.codeNote != "" {
		desc = m.codeNote + "\n\n" + desc
	}
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewNote().Title("Email verification").Description(desc),
		huh.NewInput().
			Title("Recovery code").
			Value(&m.codeInput).
			Validate(func(s string) error {
				s = strings.TrimSpace(s)
				if s == breakGlassOverrideToken || isRecoveryCodeShape(s) {
					return nil
				}
				return errors.New("enter the 6-digit code, or " + breakGlassOverrideToken)
			}),
	)))
}

func isRecoveryCodeShape(s string) bool {
	if len(s) != 6 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// overrideReason says why no code proved an admin, first line of the override form.
func (m *ownerModel) overrideReason() string {
	switch m.skip {
	case otpSkipUnknownAdmin:
		return fmt.Sprintf("No staff account is named %q.", m.attempt)
	case otpSkipNoVerifiedEmail:
		return fmt.Sprintf("%q has no verified email address, so no recovery code can reach it.", m.admin.Username)
	case otpSkipNoRelay:
		return "No mail relay can send a recovery code: " + m.skipDetail + "."
	case otpSkipSendFailed:
		return "The recovery code could not be sent: " + m.skipDetail + "."
	case otpSkipCodeExpired:
		return "The recovery code expired."
	case otpSkipCodeRejected:
		return fmt.Sprintf("%d wrong codes; that code no longer works.", recoveryCodeAttempts)
	}
	return "No admin was verified."
}

func (m *ownerModel) buildOverrideForm() *huh.Form {
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewNote().
			Title("Root override").
			Description(m.overrideReason()+"\n\n"+fmt.Sprintf("Proceed as OS user %q with root authority by typing the confirmation token. The audit log records this run as an unverified root override and the reason above. Esc starts over.", m.osUser)),
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
	subject := m.subject() // "Owner" | "Operator"
	lower := strings.ToLower(subject)

	desc := fmt.Sprintf("Create the first Owner — recorded as OS user %q.", m.osUser)
	switch m.mode {
	case "recovery":
		desc = fmt.Sprintf("Verified as %q by an email code.", m.accountable)
	case "root_override":
		desc = "Root override — the Owner will be reset."
	}
	if m.operation == bgAddOperator {
		switch m.mode {
		case "recovery":
			desc = fmt.Sprintf("Add an Operator — verified as %q by an email code.", m.accountable)
		case "root_override":
			desc = "Add an Operator (root override)."
		}
	}
	if m.provisionErr != nil {
		// Recoverable refusals routed back here: the seat refusal already names the
		// username to enter, so show it verbatim; the operator-name clash gets the
		// generic retry prompt.
		note := "That username is already taken — choose a different one."
		var seatErr *ownerSeatTakenError
		if errors.As(m.provisionErr, &seatErr) {
			note = seatErr.Error()
		}
		desc = note + "\n\n" + desc
	}

	fields := []huh.Field{
		huh.NewNote().Title(subject + " account").Description(desc),
		huh.NewInput().
			Title(subject + " username").
			Value(&m.ownerUser).
			Validate(requiredField(lower + " username")),
		huh.NewInput().
			Title(subject + " email").
			Description("optional").
			Placeholder("you@example.com").
			Value(&m.ownerEmail),
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
	b.WriteString(tuiSuccessBanner(m.subject()+" account is ready.") + "\n\n")

	var box strings.Builder
	box.WriteString(tuiLabel.Render("username  ") + m.username + "\n")
	if m.setupTokenURL != "" {
		box.WriteString("\n" + tuiLabel.Render("setup URL  ") + "\n")
		for _, line := range wrapDisplayURL(m.setupTokenURL, 70) {
			box.WriteString(tuiPassword.Render(line) + "\n")
		}
		box.WriteString("\n")
		box.WriteString(tuiWarn.Render("Open this URL to complete passwordless login setup. It is shown only once."))
	}
	if m.auditWarning != "" {
		box.WriteString("\n\n" + tuiWarn.Render("Audit warning: "+m.auditWarning))
	}
	b.WriteString(tuiCardStyle.Render(box.String()) + "\n\n")
	b.WriteString(tuiAction("enter", "continue"))
	return b.String()
}
