package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
// The richer break-glass operations (OP create, halt, sync, S3) land in a later
// phase; this file is intentionally scoped to the one provisioning operation that
// makes the local-auth thin thread reachable.

// localAuthEnabledSettingKey is the platform_settings key the live API reads to
// decide whether local-password sessions are honored. It MUST match the
// (unexported) localAuthEnabledKey the api package consults per-request; the VM
// smoke test (login after break-glass) is what guarantees they stay in sync.
const localAuthEnabledSettingKey = "local_auth_enabled"

// bootstrapPasswordAlphabet excludes visually ambiguous glyphs (0/O, 1/I/l) so a
// human can transcribe the one-time password off a terminal without error.
const bootstrapPasswordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// ownerStore is the minimal repo surface break-glass provisioning needs.
// *api.PGRepo satisfies it; the unit tests drive a fake, so the core provisioning
// logic is exercised without a database or a terminal.
type ownerStore interface {
	UpsertOwner(ctx context.Context, id, username, email, passwordHash string, mustChange bool) error
	SetSetting(ctx context.Context, key string, value []byte) error
}

// cmdBreakGlass is the `felis breakGlass` entrypoint: the root gate, config load,
// database open, and the interactive TUI. Everything below runBreakGlassTUI is
// I/O at the edge; the provisioning logic itself is plain functions over
// ownerStore so it stays testable off a terminal.
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
	res, err := runBreakGlassTUI(ctx, repo, cfg.Server.RootDomain)
	if err != nil {
		fmt.Fprintf(stderr, "felis breakGlass: %v\n", err)
		return 1
	}

	if !res.provisioned {
		fmt.Fprintln(stdout, "felis breakGlass: cancelled — no changes made.")
		return 0
	}

	// The TUI runs on the alternate screen, which is torn down on exit and takes the
	// in-console password display with it. Re-print a durable summary to the normal
	// screen so the one-time bootstrap password survives in scrollback long enough
	// for the operator to log in. It is shown, never persisted: only the bcrypt hash
	// reached the database.
	fmt.Fprintf(stdout, "\nfelis breakGlass: Owner account %q provisioned; local-password login is ENABLED.\n", res.username)
	fmt.Fprintf(stdout, "One-time bootstrap password (you MUST change it on first login):\n\n    %s\n\n", res.password)
	if res.rootDomain != "" {
		fmt.Fprintf(stdout, "Log in at https://op.console.%s with that username and password.\n", res.rootDomain)
	}
	return 0
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

// provisionOwner mints or resets the single Owner account direct-to-Postgres and
// returns the one-time bootstrap password to display. The account is created with
// must_change_password=true, which is load-bearing: it is what arms the API's
// lockdown middleware so the Owner can do nothing but change the password on first
// login. The returned plaintext exists ONLY to be shown once on this terminal — it
// is never logged or persisted; only its bcrypt hash reaches the database.
func provisionOwner(ctx context.Context, s ownerStore, username, email string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", errors.New("username is required")
	}
	id := newOwnerID()
	if id == "" {
		return "", errors.New("generate owner id: entropy source failed")
	}
	password, err := generateBootstrapPassword()
	if err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash bootstrap password: %w", err)
	}
	if err := s.UpsertOwner(ctx, id, username, strings.TrimSpace(email), string(hash), true); err != nil {
		return "", fmt.Errorf("write owner: %w", err)
	}
	return password, nil
}

// enableLocalAuth flips the runtime local_auth_enabled toggle on
// direct-to-Postgres. It is the second load-bearing write of break-glass: without
// it handleLogin returns 403 and the freshly provisioned Owner cannot log in, so a
// successful provisionOwner with local auth off is not a usable thin thread.
func enableLocalAuth(ctx context.Context, s ownerStore) error {
	// The setting is read back with json.Unmarshal into a bool, so the stored jsonb
	// value must be the literal true.
	if err := s.SetSetting(ctx, localAuthEnabledSettingKey, []byte("true")); err != nil {
		return fmt.Errorf("enable local auth: %w", err)
	}
	return nil
}

// ---- interactive TUI (the untested shell over the tested core) ----

// breakGlassResult is what the TUI hands back to cmdBreakGlass for the durable
// post-exit summary. provisioned is false on cancel.
type breakGlassResult struct {
	provisioned bool
	username    string
	password    string
	rootDomain  string
}

type bgState int

const (
	stateForm bgState = iota
	stateWorking
	stateDone
	stateError
)

// provisionedMsg is delivered to the model when the background provisioning
// command finishes.
type provisionedMsg struct {
	password string
	err      error
}

var (
	bgTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("88")).Padding(0, 1)
	bgLabelStyle = lipgloss.NewStyle().Bold(true)
	bgHintStyle  = lipgloss.NewStyle().Faint(true)
	bgErrStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	bgOKStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	bgPwStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("11")).Padding(0, 1)
	bgBoxStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 3)
)

// bgModel is the bubbletea model for the provisioning console. It is a pointer
// model so Update can mutate in place; the only field touched from the background
// command is read after the command returns via provisionedMsg.
type bgModel struct {
	ctx        context.Context
	store      ownerStore
	rootDomain string

	inputs  []textinput.Model
	focus   int
	state   bgState
	formErr string

	username string
	password string
	err      error
}

func newBGModel(ctx context.Context, s ownerStore, rootDomain string) *bgModel {
	user := textinput.New()
	user.Placeholder = "owner"
	user.SetValue("owner")
	user.CharLimit = 64
	user.Width = 40
	user.Prompt = ""
	user.Focus()

	email := textinput.New()
	email.Placeholder = "(optional)"
	email.CharLimit = 254
	email.Width = 40
	email.Prompt = ""

	return &bgModel{
		ctx:        ctx,
		store:      s,
		rootDomain: rootDomain,
		inputs:     []textinput.Model{user, email},
		focus:      0,
		state:      stateForm,
	}
}

func (m *bgModel) Init() tea.Cmd { return textinput.Blink }

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
	case provisionedMsg:
		if msg.err != nil {
			m.state, m.err = stateError, msg.err
		} else {
			m.state, m.password = stateDone, msg.password
		}
		return m, nil

	case tea.KeyMsg:
		switch m.state {
		case stateDone, stateError:
			// Any key dismisses the terminal screen.
			return m, tea.Quit
		case stateWorking:
			// Ignore input while the database write is in flight.
			return m, nil
		case stateForm:
			switch msg.String() {
			case "ctrl+c", "esc":
				return m, tea.Quit
			case "tab", "down":
				return m, m.focusInput(m.focus + 1)
			case "shift+tab", "up":
				return m, m.focusInput(m.focus - 1)
			case "enter":
				if strings.TrimSpace(m.inputs[0].Value()) == "" {
					m.formErr = "username is required"
					return m, m.focusInput(0)
				}
				m.username = strings.TrimSpace(m.inputs[0].Value())
				m.state, m.formErr = stateWorking, ""
				return m, provisionCmd(m.ctx, m.store, m.username, m.inputs[1].Value())
			}
		}
	}

	cmd := m.updateInputs(msg)
	return m, cmd
}

func (m *bgModel) View() string {
	var b strings.Builder
	b.WriteString(bgTitleStyle.Render("⚠  FELIS BREAK-GLASS — LOCAL EMERGENCY CONSOLE") + "\n\n")

	switch m.state {
	case stateForm:
		b.WriteString("Provision (or reset) the Owner account and enable local-password login.\n")
		b.WriteString("This writes directly to Postgres, bypassing the web Zero-Trust path.\n\n")
		b.WriteString(bgLabelStyle.Render("Owner username") + "\n")
		b.WriteString(m.inputs[0].View() + "\n\n")
		b.WriteString(bgLabelStyle.Render("Owner email (optional)") + "\n")
		b.WriteString(m.inputs[1].View() + "\n\n")
		if m.formErr != "" {
			b.WriteString(bgErrStyle.Render(m.formErr) + "\n\n")
		}
		b.WriteString(bgHintStyle.Render("tab/↑↓ move · enter provision · esc cancel") + "\n")

	case stateWorking:
		b.WriteString("Provisioning the Owner account…\n")

	case stateDone:
		b.WriteString(bgOKStyle.Render("✓ Owner provisioned · local-password login ENABLED") + "\n\n")
		box := bgLabelStyle.Render("username  ") + m.username + "\n" +
			bgLabelStyle.Render("password  ") + bgPwStyle.Render(m.password)
		b.WriteString(bgBoxStyle.Render(box) + "\n\n")
		b.WriteString(bgErrStyle.Render("Record this password now — it is shown only once.") + "\n")
		b.WriteString("You will be required to change it on first login.\n\n")
		if m.rootDomain != "" {
			b.WriteString("Log in at " + bgLabelStyle.Render("https://op.console."+m.rootDomain) + "\n\n")
		}
		b.WriteString(bgHintStyle.Render("press any key to exit") + "\n")

	case stateError:
		b.WriteString(bgErrStyle.Render("✗ Provisioning failed") + "\n\n")
		b.WriteString(m.err.Error() + "\n\n")
		b.WriteString(bgHintStyle.Render("press any key to exit") + "\n")
	}
	return b.String()
}

// provisionCmd runs the two load-bearing writes off the UI goroutine and reports
// the outcome back as a provisionedMsg. The Owner row is written first, then local
// auth is enabled. If enabling local auth fails, the message carries the error and
// Update routes to stateError: the generated plaintext in msg.password is dropped —
// never displayed and never stored anywhere — so a partial run leaks nothing. This
// is safe because the Owner row is unreachable for login while local_auth_enabled is
// unset, and re-running break-glass is idempotent (UpsertOwner is ON CONFLICT, so it
// overwrites the hash and re-issues a fresh password) and converges the toggle.
func provisionCmd(ctx context.Context, s ownerStore, username, email string) tea.Cmd {
	return func() tea.Msg {
		pw, err := provisionOwner(ctx, s, username, email)
		if err == nil {
			err = enableLocalAuth(ctx, s)
		}
		return provisionedMsg{password: pw, err: err}
	}
}

// runBreakGlassTUI drives the bubbletea program and projects the final model onto a
// breakGlassResult. It is the thin, untested shell; the logic it invokes
// (provisionOwner / enableLocalAuth) is unit-tested directly.
func runBreakGlassTUI(ctx context.Context, s ownerStore, rootDomain string) (breakGlassResult, error) {
	final, err := tea.NewProgram(newBGModel(ctx, s, rootDomain), tea.WithAltScreen()).Run()
	if err != nil {
		return breakGlassResult{}, err
	}
	m, ok := final.(*bgModel)
	if !ok {
		return breakGlassResult{}, errors.New("unexpected final model")
	}
	if m.state == stateError {
		return breakGlassResult{}, m.err
	}
	return breakGlassResult{
		provisioned: m.state == stateDone,
		username:    m.username,
		password:    m.password,
		rootDomain:  rootDomain,
	}, nil
}
