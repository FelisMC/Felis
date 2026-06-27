package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/store"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/crypto/bcrypt"
)

// `felis breakGlass` is the local break-glass emergency console (spec §B). Its
// authority is local root, so it legitimately BYPASSES the web Zero-Trust + Passkey
// path: critical recovery runs direct-to-Postgres. The thin-thread operation it
// ships here is the one that bootstraps everything else — provision (or reset) the
// single Owner account and turn local-password login on — so that even with the web
// auth path unconfigured an operator can get into op.console. It is a genuine
// interactive TUI, NOT a CLI: bare `felis` prints CLI usage, while `felis breakGlass`
// opens this full-screen console. It refuses to run unless euid is 0 (sudo/root).
//
// Root is necessary but NOT sufficient for accountability: root is machine
// authority, not a human identity, so the console additionally captures WHO is
// breaking the glass. When a staff account already exists it asks the operator to
// authenticate as an existing admin (the verified identity is the accountable
// actor); when none exists yet it bootstraps the first Owner from the typed
// credential and attributes the act to the OS user. The audit row records the
// difference. This attribution is best-effort, not tamper-proof — whoever runs
// this is root and can edit Postgres directly — but it produces an honest trail
// for an honest operator, which is the point.
//
// The richer break-glass operations (OP create, halt, sync, S3) land in a later
// phase; this file is intentionally scoped to the one provisioning operation that
// makes the local-auth thin thread reachable.

// breakGlassOverrideToken is the literal an operator must type to proceed when no
// admin credential could be verified. Requiring an explicit, deliberate word (not a
// bare Enter) keeps the unverified root override from happening by reflex.
const breakGlassOverrideToken = "OVERRIDE"

// bootstrapPasswordAlphabet excludes visually ambiguous glyphs (0/O, 1/I/l) so a
// human can transcribe a generated one-time password off a terminal without error.
const bootstrapPasswordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// ownerStore is the minimal repo surface the break-glass console needs.
// *api.PGRepo satisfies it; the unit tests drive a fake, so the core logic
// (authentication, provisioning, accountability audit) is exercised without a
// database or a terminal.
type ownerStore interface {
	// AdminExists reports whether any authenticatable staff account already exists.
	// It is the bootstrap-vs-recovery switch.
	AdminExists(ctx context.Context) (bool, error)
	// UserByUsername loads a staff login projection for credential verification.
	UserByUsername(ctx context.Context, username string) (*api.StaffUser, error)
	UpsertOwner(ctx context.Context, id, username, email, passwordHash string, mustChange bool) error
	SetSetting(ctx context.Context, key string, value []byte) error
	// Audit records the break-glass accountability row.
	Audit(ctx context.Context, e api.AuditEntry) error
}

// cmdBreakGlass is the `felis breakGlass` entrypoint: the root gate, config load,
// database open, accountability detection, and the interactive TUI. Everything
// below runBreakGlassTUI is I/O at the edge; the authentication/provisioning/audit
// logic itself is plain functions over ownerStore so it stays testable off a
// terminal.
func cmdBreakGlass(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("breakGlass", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Root gate. The break-glass console's whole authority model is "you already
	// have local root", so anything less is refused rather than degraded. On
	// non-Unix os.Geteuid() returns -1, which also refuses — correct, since this is
	// a Linux-VM recovery tool.
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis breakGlass: refused — the break-glass emergency console must run as root (try: sudo felis breakGlass)")
		return 1
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis breakGlass: %v\n", err)
		return 1
	}

	ctx := context.Background()
	drv, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		fmt.Fprintf(stderr, "felis breakGlass: open database: %v\n", err)
		return 1
	}
	defer drv.Close()

	repo := api.NewPGRepo(drv.DB())

	// Decide bootstrap (no admin yet → typed credential mints the first Owner) vs
	// recovery (an admin exists → the operator must authenticate as one) BEFORE the
	// alt-screen TUI takes over, so a database fault surfaces as a plain error.
	adminExists, err := repo.AdminExists(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "felis breakGlass: detect existing admin: %v\n", err)
		return 1
	}

	res, err := runBreakGlassTUI(ctx, repo, cfg.Server.RootDomain, accountableOSUser(), adminExists)
	if err != nil {
		fmt.Fprintf(stderr, "felis breakGlass: %v\n", err)
		return 1
	}

	if !res.provisioned {
		fmt.Fprintln(stdout, "felis breakGlass: cancelled — no changes made.")
		return 0
	}

	// The TUI runs on the alternate screen, which is torn down on exit and takes its
	// display with it. Re-print a durable summary to the normal screen so the
	// outcome — and any generated one-time password — survives in scrollback long
	// enough for the operator to log in.
	fmt.Fprintf(stdout, "\nfelis breakGlass: Owner account %q provisioned; local-password login is ENABLED.\n", res.username)
	fmt.Fprintf(stdout, "Recorded as %q (mode: %s, os user: %s).\n", res.accountable, res.mode, res.osUser)
	if res.displayPassword != "" {
		// A one-time password was generated (recovery / root override). It is shown,
		// never persisted: only the bcrypt hash reached the database.
		fmt.Fprintf(stdout, "One-time password (you MUST change it on first login):\n\n    %s\n\n", res.displayPassword)
	} else {
		// Bootstrap: the operator typed the password themselves, so we do NOT echo it
		// back into scrollback.
		fmt.Fprintln(stdout, "Log in with the password you just entered (you MUST change it on first login).")
	}
	if res.auditWarning != "" {
		fmt.Fprintf(stdout, "WARNING: the accountability audit row was NOT written: %s\n", res.auditWarning)
	}
	if res.rootDomain != "" {
		fmt.Fprintf(stdout, "Log in at https://op.console.%s with that username and password.\n", res.rootDomain)
	}
	return 0
}

// accountableOSUser returns the human who escalated to root, best-effort, for the
// audit trail. sudo sets SUDO_USER to the invoking account; a direct root shell
// leaves it empty, in which case we record "root". This is attribution, not proof:
// the environment can be forged, so sudo's own syslog entry — not this value — is
// the tamper-evident record. The root gate is the real authority gate; this only
// answers "which human" for an honest operator.
func accountableOSUser() string {
	if u := strings.TrimSpace(os.Getenv("SUDO_USER")); u != "" {
		return u
	}
	return "root"
}

// newOwnerID returns a fresh, unguessable id for the Owner row. It mirrors the
// crypto/rand hex idiom used elsewhere (internal/submit, internal/build): 16 bytes
// give uuid-equivalent entropy and the value is path/argv-safe lowercase hex. A
// fresh id per run is fine because UpsertOwner preserves the existing id on a
// username conflict, so a reset keeps live sessions valid.
func newOwnerID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand only fails on a broken entropy source. Refuse rather than mint a
		// predictable id for a privileged account.
		return ""
	}
	return "usr-" + hex.EncodeToString(b[:])
}

// generateBootstrapPassword returns a fresh one-time password from the unambiguous
// alphabet. It rejection-samples to avoid modulo bias, so every position is uniform
// over the alphabet. 20 chars over a 57-symbol alphabet is ~116 bits — far more than
// the must-change credential needs, and it is rotated on first login regardless.
func generateBootstrapPassword() (string, error) {
	const n = 20
	// Largest multiple of the alphabet size that fits in a byte; bytes at or above it
	// are discarded so the surviving values map uniformly (no modulo bias).
	limit := byte(256 - (256 % len(bootstrapPasswordAlphabet)))
	out := make([]byte, 0, n)
	var b [1]byte
	for len(out) < n {
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("generate bootstrap password: %w", err)
		}
		if b[0] >= limit {
			continue
		}
		out = append(out, bootstrapPasswordAlphabet[int(b[0])%len(bootstrapPasswordAlphabet)])
	}
	return string(out), nil
}

// validateOwnerPassword mirrors api.validateNewPassword (handlers_auth.go): a
// break-glass credential must satisfy the SAME 8–72-byte rule the panel's own
// change-password enforces, so an operator can never set a password here that the
// web change-password flow would later reject. 72 is bcrypt's hard input limit.
func validateOwnerPassword(pw string) error {
	if len(pw) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	if len(pw) > 72 {
		return errors.New("password must be at most 72 bytes")
	}
	return nil
}

// authenticateAdmin verifies a typed credential against an existing admin account
// for recovery-mode attribution. matched is the stored username on success.
//
// ok==false with err==nil is NOT a failure to surface — it means the credential did
// not match any admin password. The caller offers an explicit root override instead
// of refusing, because break-glass must still recover when no admin credential can
// be produced (a forgotten password is the canonical reason the web login is
// unreachable in the first place). Only a real datastore fault returns err.
func authenticateAdmin(ctx context.Context, s ownerStore, username, password string) (matched string, ok bool, err error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return "", false, nil
	}
	u, err := s.UserByUsername(ctx, username)
	if errors.Is(err, api.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	// Only an admin row carrying a bcrypt hash is an authenticatable staff identity;
	// a player row (role=user, hash NULL → empty PasswordHash) can never attribute a
	// break-glass action.
	if u.Role != "admin" || u.PasswordHash == "" {
		return "", false, nil
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return "", false, nil
	}
	return u.Username, true, nil
}

// provisionOwner mints or resets the single Owner account direct-to-Postgres with
// the given (already-validated-by-the-caller) password. The account is created with
// must_change_password=true, which is load-bearing: it is what arms the API's
// lockdown middleware so the Owner can do nothing but change the password on first
// login. Only the bcrypt hash reaches the database; the plaintext never does.
func provisionOwner(ctx context.Context, s ownerStore, username, email, password string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("owner username is required")
	}
	if err := validateOwnerPassword(password); err != nil {
		return err
	}
	id := newOwnerID()
	if id == "" {
		return errors.New("generate owner id: entropy source failed")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash owner password: %w", err)
	}
	if err := s.UpsertOwner(ctx, id, username, strings.TrimSpace(email), string(hash), true); err != nil {
		return fmt.Errorf("write owner: %w", err)
	}
	return nil
}

// enableLocalAuth flips the runtime local_auth_enabled toggle on
// direct-to-Postgres. It is a load-bearing write of break-glass: without it
// handleLogin returns 403 and the freshly provisioned Owner cannot log in, so a
// successful provisionOwner with local auth off is not a usable thin thread.
func enableLocalAuth(ctx context.Context, s ownerStore) error {
	// The setting is read back with json.Unmarshal into a bool, so the stored jsonb
	// value must be the literal true.
	if err := s.SetSetting(ctx, api.LocalAuthEnabledKey, []byte("true")); err != nil {
		return fmt.Errorf("enable local auth: %w", err)
	}
	return nil
}

// breakGlassOp is a fully-resolved operation the TUI hands to the core once the
// operator has been identified (bootstrap) or authenticated (recovery / override).
type breakGlassOp struct {
	mode           string // "bootstrap" | "recovery" | "root_override"
	accountable    string // recorded as the audit actor (verified admin, or OS user)
	osUser         string // $SUDO_USER (or "root"); recorded in the payload
	ownerUsername  string
	ownerEmail     string
	ownerPassword  string // typed (bootstrap); "" => generate a one-time password
	attemptedAdmin string // recovery / override: the admin username the operator typed
}

// breakGlassOutcome is what performBreakGlass reports back to the TUI.
type breakGlassOutcome struct {
	displayPassword string // non-empty only when a one-time password was generated
	auditErr        error  // non-nil if the accountability row could not be written
}

// performBreakGlass executes a resolved break-glass operation: provision (or reset)
// the Owner, enable local-password login, then record a best-effort accountability
// audit row. A typed ownerPassword (bootstrap) is used as-is; an empty one (recovery
// / root override) is replaced with a generated one-time password returned for
// one-time display. The audit write is best-effort: a logging failure is reported
// via auditErr but does NOT fail the recovery — break-glass must still work when the
// audit sink is unhappy.
func performBreakGlass(ctx context.Context, s ownerStore, op breakGlassOp) (breakGlassOutcome, error) {
	password := op.ownerPassword
	generated := false
	if password == "" {
		p, err := generateBootstrapPassword()
		if err != nil {
			return breakGlassOutcome{}, err
		}
		password, generated = p, true
	}
	if err := provisionOwner(ctx, s, op.ownerUsername, op.ownerEmail, password); err != nil {
		return breakGlassOutcome{}, err
	}
	// Record accountability the instant the credential changes — BEFORE enabling
	// local auth, which can still fail. Auditing only after both writes would let a
	// failed enableLocalAuth leave a just-reset credential with no "who did it" row;
	// the audit is best-effort, so doing it first never blocks the recovery.
	out := breakGlassOutcome{auditErr: auditBreakGlass(ctx, s, op)}
	if generated {
		out.displayPassword = password
	}
	if err := enableLocalAuth(ctx, s); err != nil {
		return breakGlassOutcome{}, err
	}
	return out, nil
}

// auditBreakGlass writes the break-glass accountability row. The actor is the
// resolved human identity (a verified admin in recovery, the OS user otherwise);
// the payload carries the full who/what/how so an after-the-fact reader can tell a
// verified recovery from an unverified root override. It is best-effort — the caller
// does not fail the recovery if this write fails — and intentionally honest: it
// records attribution, it does not prove it (a malicious root can edit the row).
func auditBreakGlass(ctx context.Context, s ownerStore, op breakGlassOp) error {
	payload := map[string]any{
		"mode":     op.mode,
		"owner":    op.ownerUsername,
		"os_user":  op.osUser,
		"verified": op.mode == "recovery",
	}
	if op.attemptedAdmin != "" {
		payload["admin_account"] = op.attemptedAdmin
	}
	blob, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.Audit(ctx, api.AuditEntry{
		Actor:   op.accountable,
		Source:  "break-glass",
		Action:  "break_glass." + op.mode,
		Payload: blob,
	})
}

// ---- interactive TUI (the untested shell over the tested core) ----

// breakGlassResult is what the TUI hands back to cmdBreakGlass for the durable
// post-exit summary. provisioned is false on cancel.
type breakGlassResult struct {
	provisioned     bool
	mode            string
	accountable     string
	osUser          string
	username        string
	displayPassword string // empty when the operator typed their own bootstrap password
	auditWarning    string
	rootDomain      string
}

type bgStep int

const (
	stepAuth     bgStep = iota // recovery: authenticate as an existing admin
	stepOverride               // recovery: admin auth failed → deliberate root override
	stepProvision              // collect the Owner target (and password, in bootstrap)
	stepWorking
	stepDone
	stepError
)

// authResultMsg carries the outcome of the off-goroutine admin credential check.
type authResultMsg struct {
	matched string
	ok      bool
	err     error
}

// performedMsg carries the outcome of the off-goroutine break-glass writes.
type performedMsg struct {
	outcome breakGlassOutcome
	err     error
}

var (
	bgTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("88")).Padding(0, 1)
	bgLabelStyle = lipgloss.NewStyle().Bold(true)
	bgHintStyle  = lipgloss.NewStyle().Faint(true)
	bgErrStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	bgWarnStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	bgOKStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	bgPwStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("11")).Padding(0, 1)
	bgBoxStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 3)
)

// bgModel is the bubbletea model for the break-glass console. It is a pointer model
// so Update can mutate in place; fields touched from a background command are read
// only after that command returns via authResultMsg / performedMsg.
type bgModel struct {
	ctx         context.Context
	store       ownerStore
	rootDomain  string
	osUser      string
	adminExists bool

	step    bgStep
	inputs  []textinput.Model
	focus   int
	formErr string
	working string

	// resolved as the flow advances
	mode           string
	accountable    string
	attemptedAdmin string

	// result
	ownerUsername   string
	displayPassword string
	auditWarning    string
	err             error
}

func newBGModel(ctx context.Context, s ownerStore, rootDomain, osUser string, adminExists bool) *bgModel {
	m := &bgModel{
		ctx:         ctx,
		store:       s,
		rootDomain:  rootDomain,
		osUser:      osUser,
		adminExists: adminExists,
	}
	if adminExists {
		// Recovery: an admin already exists, so the operator must identify themselves
		// before the glass breaks.
		m.step = stepAuth
		m.buildAuth()
	} else {
		// Bootstrap: no staff account exists yet; the typed credential mints the first
		// Owner, attributed to the OS user.
		m.mode = "bootstrap"
		m.accountable = osUser
		m.step = stepProvision
		m.buildProvision(true)
	}
	return m
}

func (m *bgModel) Init() tea.Cmd { return textinput.Blink }

// bgInput builds a styled text input; password fields echo a mask, never the glyphs,
// because this is typed on a shared root console.
func bgInput(placeholder string, charLimit int, password bool) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.CharLimit = charLimit
	ti.Width = 44
	ti.Prompt = ""
	if password {
		ti.EchoMode = textinput.EchoPassword
		ti.EchoCharacter = '•'
	}
	return ti
}

// setInputs installs a fresh input set, focuses the first, and returns its blink cmd.
func (m *bgModel) setInputs(ins []textinput.Model) tea.Cmd {
	m.inputs = ins
	m.focus = 0
	var cmd tea.Cmd
	for i := range m.inputs {
		if i == 0 {
			cmd = m.inputs[i].Focus()
		} else {
			m.inputs[i].Blur()
		}
	}
	return cmd
}

func (m *bgModel) buildAuth() tea.Cmd {
	user := bgInput("admin username", 64, false)
	pass := bgInput("admin password", 128, true)
	return m.setInputs([]textinput.Model{user, pass})
}

func (m *bgModel) buildOverride() tea.Cmd {
	confirm := bgInput("type "+breakGlassOverrideToken, 16, false)
	return m.setInputs([]textinput.Model{confirm})
}

// buildProvision installs the Owner-target inputs. withPassword adds the password +
// confirm fields used only in bootstrap mode; in recovery/override a one-time
// password is generated, so the operator does not type one.
func (m *bgModel) buildProvision(withPassword bool) tea.Cmd {
	user := bgInput("owner", 64, false)
	user.SetValue("owner")
	email := bgInput("(optional)", 254, false)
	ins := []textinput.Model{user, email}
	if withPassword {
		ins = append(ins, bgInput("at least 8 characters", 128, true))
		ins = append(ins, bgInput("re-enter password", 128, true))
	}
	return m.setInputs(ins)
}

func (m *bgModel) enterProvision() tea.Cmd {
	m.step = stepProvision
	m.formErr = ""
	return m.buildProvision(m.mode == "bootstrap")
}

func (m *bgModel) focusInput(i int) tea.Cmd {
	if i < 0 {
		i = len(m.inputs) - 1
	}
	if i >= len(m.inputs) {
		i = 0
	}
	m.focus = i
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

func (m *bgModel) updateInputs(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, len(m.inputs))
	for i := range m.inputs {
		m.inputs[i], cmds[i] = m.inputs[i].Update(msg)
	}
	return tea.Batch(cmds...)
}

func (m *bgModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case authResultMsg:
		if msg.err != nil {
			m.step, m.err = stepError, msg.err
			return m, nil
		}
		if msg.ok {
			// Verified: this admin is the accountable identity for the recovery.
			m.mode = "recovery"
			m.accountable = msg.matched
			return m, m.enterProvision()
		}
		// The credential did not verify. Do NOT refuse — break-glass must still
		// recover when no admin password can be produced. Offer a deliberate root
		// override, attributed to the OS user and audited as unverified.
		m.step = stepOverride
		return m, m.buildOverride()

	case performedMsg:
		if msg.err != nil {
			m.step, m.err = stepError, msg.err
			return m, nil
		}
		m.step = stepDone
		m.displayPassword = msg.outcome.displayPassword
		if msg.outcome.auditErr != nil {
			m.auditWarning = msg.outcome.auditErr.Error()
		}
		return m, nil

	case tea.KeyMsg:
		switch m.step {
		case stepDone, stepError:
			// Any key dismisses the terminal screen.
			return m, tea.Quit
		case stepWorking:
			// Ignore input while a database write is in flight.
			return m, nil
		default:
			return m.handleFormKey(msg)
		}
	}

	return m, m.updateInputs(msg)
}

func (m *bgModel) handleFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.step == stepOverride {
			// Back out of the override to re-enter the admin credential.
			m.step, m.formErr = stepAuth, ""
			return m, m.buildAuth()
		}
		return m, tea.Quit
	case "tab", "down":
		return m, m.focusInput(m.focus + 1)
	case "shift+tab", "up":
		return m, m.focusInput(m.focus - 1)
	case "enter":
		return m.submit()
	}
	return m, m.updateInputs(msg)
}

func (m *bgModel) submit() (tea.Model, tea.Cmd) {
	switch m.step {
	case stepAuth:
		return m.submitAuth()
	case stepOverride:
		return m.submitOverride()
	case stepProvision:
		return m.submitProvision()
	}
	return m, nil
}

func (m *bgModel) submitAuth() (tea.Model, tea.Cmd) {
	user := strings.TrimSpace(m.inputs[0].Value())
	pass := m.inputs[1].Value()
	if user == "" || pass == "" {
		m.formErr = "enter the username and password of an existing admin"
		return m, nil
	}
	m.attemptedAdmin = user
	m.formErr, m.working = "", "Verifying the admin credential…"
	m.step = stepWorking
	return m, authCmd(m.ctx, m.store, user, pass)
}

func (m *bgModel) submitOverride() (tea.Model, tea.Cmd) {
	if m.inputs[0].Value() != breakGlassOverrideToken {
		m.formErr = "type " + breakGlassOverrideToken + " exactly to proceed, or esc to go back"
		return m, nil
	}
	m.mode = "root_override"
	m.accountable = m.osUser
	return m, m.enterProvision()
}

func (m *bgModel) submitProvision() (tea.Model, tea.Cmd) {
	owner := strings.TrimSpace(m.inputs[0].Value())
	if owner == "" {
		m.formErr = "owner username is required"
		return m, m.focusInput(0)
	}
	email := m.inputs[1].Value()
	password := "" // empty => performBreakGlass generates a one-time password
	if m.mode == "bootstrap" {
		pw := m.inputs[2].Value()
		confirm := m.inputs[3].Value()
		if err := validateOwnerPassword(pw); err != nil {
			m.formErr = err.Error()
			return m, m.focusInput(2)
		}
		if pw != confirm {
			m.formErr = "the two passwords do not match"
			return m, m.focusInput(3)
		}
		password = pw
	}
	m.ownerUsername = owner
	op := breakGlassOp{
		mode:           m.mode,
		accountable:    m.accountable,
		osUser:         m.osUser,
		ownerUsername:  owner,
		ownerEmail:     email,
		ownerPassword:  password,
		attemptedAdmin: m.attemptedAdmin,
	}
	m.formErr, m.working = "", "Provisioning the Owner account…"
	m.step = stepWorking
	return m, performCmd(m.ctx, m.store, op)
}

// authCmd runs the admin credential check off the UI goroutine.
func authCmd(ctx context.Context, s ownerStore, user, pass string) tea.Cmd {
	return func() tea.Msg {
		matched, ok, err := authenticateAdmin(ctx, s, user, pass)
		return authResultMsg{matched: matched, ok: ok, err: err}
	}
}

// performCmd runs the break-glass writes off the UI goroutine.
func performCmd(ctx context.Context, s ownerStore, op breakGlassOp) tea.Cmd {
	return func() tea.Msg {
		out, err := performBreakGlass(ctx, s, op)
		return performedMsg{outcome: out, err: err}
	}
}

func (m *bgModel) View() string {
	var b strings.Builder
	b.WriteString(bgTitleStyle.Render("⚠  FELIS BREAK-GLASS — LOCAL EMERGENCY CONSOLE") + "\n\n")

	switch m.step {
	case stepAuth:
		b.WriteString("A staff account already exists. Identify yourself before breaking the glass.\n")
		b.WriteString("Authenticate as an existing admin — this records WHO performed the recovery.\n")
		b.WriteString(bgHintStyle.Render("Best-effort attribution, not a second authority gate (root already let you in).") + "\n\n")
		b.WriteString(bgLabelStyle.Render("Admin username") + "\n")
		b.WriteString(m.inputs[0].View() + "\n\n")
		b.WriteString(bgLabelStyle.Render("Admin password") + "\n")
		b.WriteString(m.inputs[1].View() + "\n\n")
		if m.formErr != "" {
			b.WriteString(bgErrStyle.Render(m.formErr) + "\n\n")
		}
		b.WriteString(bgHintStyle.Render("tab/↑↓ move · enter verify · esc cancel") + "\n")

	case stepOverride:
		b.WriteString(bgErrStyle.Render("✗ That credential did not match any admin account.") + "\n\n")
		b.WriteString("You can still proceed under local-root authority. This is a ROOT OVERRIDE:\n")
		b.WriteString("it will be recorded as an UNVERIFIED break-glass attributed to the OS user\n")
		b.WriteString(bgLabelStyle.Render("\""+m.osUser+"\"") + ", not to a verified admin.\n\n")
		b.WriteString(bgLabelStyle.Render("Type "+breakGlassOverrideToken+" to proceed") + "\n")
		b.WriteString(m.inputs[0].View() + "\n\n")
		if m.formErr != "" {
			b.WriteString(bgErrStyle.Render(m.formErr) + "\n\n")
		}
		b.WriteString(bgHintStyle.Render("enter confirm · esc go back to admin login") + "\n")

	case stepProvision:
		if m.mode == "bootstrap" {
			b.WriteString("No staff account exists yet — bootstrapping the first Owner.\n")
			b.WriteString("You are recorded as OS user " + bgLabelStyle.Render("\""+m.osUser+"\"") + ".\n\n")
		} else if m.mode == "root_override" {
			b.WriteString(bgWarnStyle.Render("ROOT OVERRIDE") + " by OS user " + bgLabelStyle.Render("\""+m.osUser+"\"") + " — resetting the Owner account.\n")
			b.WriteString("A new one-time password will be generated and shown once.\n\n")
		} else {
			b.WriteString("Authenticated as admin " + bgLabelStyle.Render("\""+m.accountable+"\"") + " — resetting the Owner account.\n")
			b.WriteString("A new one-time password will be generated and shown once.\n\n")
		}
		b.WriteString(bgLabelStyle.Render("Owner username") + "\n")
		b.WriteString(m.inputs[0].View() + "\n\n")
		b.WriteString(bgLabelStyle.Render("Owner email (optional)") + "\n")
		b.WriteString(m.inputs[1].View() + "\n\n")
		if m.mode == "bootstrap" {
			b.WriteString(bgLabelStyle.Render("Owner password") + bgHintStyle.Render("  (you will change it on first login)") + "\n")
			b.WriteString(m.inputs[2].View() + "\n\n")
			b.WriteString(bgLabelStyle.Render("Confirm password") + "\n")
			b.WriteString(m.inputs[3].View() + "\n\n")
		}
		if m.formErr != "" {
			b.WriteString(bgErrStyle.Render(m.formErr) + "\n\n")
		}
		b.WriteString(bgHintStyle.Render("tab/↑↓ move · enter provision · esc cancel") + "\n")

	case stepWorking:
		msg := m.working
		if msg == "" {
			msg = "Working…"
		}
		b.WriteString(msg + "\n")

	case stepDone:
		b.WriteString(bgOKStyle.Render("✓ Owner provisioned · local-password login ENABLED") + "\n\n")
		box := bgLabelStyle.Render("username  ") + m.ownerUsername
		if m.displayPassword != "" {
			box += "\n" + bgLabelStyle.Render("password  ") + bgPwStyle.Render(m.displayPassword)
		}
		b.WriteString(bgBoxStyle.Render(box) + "\n\n")
		b.WriteString(bgHintStyle.Render("recorded as "+m.accountable+" · mode "+m.mode+" · os user "+m.osUser) + "\n\n")
		if m.displayPassword != "" {
			b.WriteString(bgErrStyle.Render("Record this password now — it is shown only once.") + "\n")
		} else {
			b.WriteString("Log in with the password you just entered.\n")
		}
		b.WriteString("You will be required to change it on first login.\n\n")
		if m.auditWarning != "" {
			b.WriteString(bgWarnStyle.Render("⚠ accountability record was NOT written: "+m.auditWarning) + "\n\n")
		}
		if m.rootDomain != "" {
			b.WriteString("Log in at " + bgLabelStyle.Render("https://op.console."+m.rootDomain) + "\n\n")
		}
		b.WriteString(bgHintStyle.Render("press any key to exit") + "\n")

	case stepError:
		b.WriteString(bgErrStyle.Render("✗ Break-glass failed") + "\n\n")
		b.WriteString(m.err.Error() + "\n\n")
		b.WriteString(bgHintStyle.Render("press any key to exit") + "\n")
	}
	return b.String()
}

// runBreakGlassTUI drives the bubbletea program and projects the final model onto a
// breakGlassResult. It is the thin, untested shell; the logic it invokes
// (authenticateAdmin / performBreakGlass) is unit-tested directly.
func runBreakGlassTUI(ctx context.Context, s ownerStore, rootDomain, osUser string, adminExists bool) (breakGlassResult, error) {
	final, err := tea.NewProgram(newBGModel(ctx, s, rootDomain, osUser, adminExists), tea.WithAltScreen()).Run()
	if err != nil {
		return breakGlassResult{}, err
	}
	m, ok := final.(*bgModel)
	if !ok {
		return breakGlassResult{}, errors.New("unexpected final model")
	}
	if m.step == stepError {
		return breakGlassResult{}, m.err
	}
	return breakGlassResult{
		provisioned:     m.step == stepDone,
		mode:            m.mode,
		accountable:     m.accountable,
		osUser:          m.osUser,
		username:        m.ownerUsername,
		displayPassword: m.displayPassword,
		auditWarning:    m.auditWarning,
		rootDomain:      rootDomain,
	}, nil
}
