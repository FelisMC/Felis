package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"felis.lolicon.best/internal/cfsetup"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

// edgeModel publishes the panel via Cloudflare Tunnel + Access. It shares the
// wizard's chrome with every other connection screen: no banner of its own (the
// root paints the step rail), one huh form for input (the two credential/host
// groups), and the same locked, clear-screen styling as Local and Reverse-proxy.
// The intro/working/done/error states stay custom — they show status and actions
// rather than collect input — but they use the shared widgets so nothing reads
// as a separate popup.
type egStep int

const (
	egIntro egStep = iota
	egForm
	egWorking
	egDone
	egError
)

type egLoginDoneMsg struct{ err error }

type egInstallDoneMsg struct{ err error }

type egSetupDoneMsg struct {
	result   *cfsetup.Result
	progress []string
	err      error
}

type edgeModel struct {
	step egStep

	rootDomain    string
	adminHostname string
	panelHostname string

	// cloudflared detection (intro gate)
	cloudflaredPath string
	certExists      bool
	installing      bool
	loginNote       string

	// form-bound inputs (one huh form, two groups: credentials + hosts)
	form        *huh.Form
	authToken   string
	authAccount string
	identity    string
	panelHost   string
	adminHost   string
	tunnelName  string
	cfgPath     string

	// working / result
	sp       spinner.Model
	working  string
	lastErr  error
	prog     []string
	result   *cfsetup.Result
	panelSet string
	adminSet string

	width, height int
}

func newEdgeModel(rootDomain, adminHost, panelHost string) *edgeModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiLabel

	m := &edgeModel{
		step:          egIntro,
		rootDomain:    rootDomain,
		adminHostname: adminHost,
		panelHostname: panelHost,
		sp:            sp,
		// Prefill the host/identity fields so the common case is enter-through.
		panelHost:  defaultPanelHostname(rootDomain, panelHost),
		adminHost:  defaultAdminHostname(rootDomain, adminHost),
		tunnelName: defaultTunnelName,
		cfgPath:    defaultTunnelConfigPath,
	}
	m.detectCloudflared()
	return m
}

func (m *edgeModel) detectCloudflared() {
	pre := cfsetup.DetectPreconditions("")
	m.cloudflaredPath = pre.CloudflaredPath
	m.certExists = pre.CertExists
}

// build assembles the single two-group form. Group 1 collects the API
// credentials; group 2 the hostnames and who Access admits. huh owns the
// inter-group navigation natively — enter/tab advances (and runs that group's
// validators first), shift+tab from the top of group 2 steps back to group 1,
// and esc aborts the whole form, which we treat as "back to the intro". This is
// why edge no longer hand-manages two separate input screens.
func (m *edgeModel) build() *huh.Form {
	return m.sized(newFelisForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Cloudflare credentials").
				Description("Create a scoped Bearer token (Account › Access: Edit) at:\n"+cloudflareAccessTokenTemplateURL),
			huh.NewInput().
				Title("API token").
				Description("starts cfat_… — not your Global API Key").
				EchoMode(huh.EchoModePassword).
				CharLimit(200).
				Value(&m.authToken).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return errors.New("a Cloudflare API token is required")
					}
					return nil
				}),
			huh.NewInput().
				Title("Account ID").
				Description("32-char hex from the dashboard URL: dash.cloudflare.com/<this>").
				CharLimit(64).
				Value(&m.authAccount).
				Validate(validateAccountID),
		).Title("Step 1 · Credentials"),
		huh.NewGroup(
			huh.NewInput().
				Title("Admit").
				Description("Who Access lets in: your email, or @your-domain").
				CharLimit(254).
				Value(&m.identity).
				Validate(validateAccessIdentity),
			huh.NewInput().
				Title("Player console hostname").
				CharLimit(253).
				Value(&m.panelHost).
				Validate(func(s string) error {
					return validateEdgeHostname("player console", normalizeEdgeHostname(s), false)
				}),
			huh.NewInput().
				Title("Admin console hostname").
				Description("fronted by Cloudflare Access").
				CharLimit(253).
				Value(&m.adminHost).
				Validate(func(s string) error {
					admin := normalizeEdgeHostname(s)
					if err := validateEdgeHostname("admin console", admin, true); err != nil {
						return err
					}
					if panel := normalizeEdgeHostname(m.panelHost); panel != "" && strings.EqualFold(panel, admin) {
						return errors.New("player and admin console hostnames must be different")
					}
					return nil
				}),
			huh.NewInput().
				Title("Tunnel name").
				CharLimit(64).
				Value(&m.tunnelName),
			huh.NewInput().
				Title("Tunnel config path").
				CharLimit(256).
				Value(&m.cfgPath),
		).Title("Step 2 · Hostnames & identity"),
	))
}

// validateAccountID accepts the 32-char hex account id and gives a targeted nudge
// when the operator pastes the API token into the wrong field.
func validateAccountID(s string) error {
	a := strings.TrimSpace(s)
	if a == "" {
		return errors.New("the Cloudflare account ID is required")
	}
	if !isHex32(a) {
		if strings.HasPrefix(a, "cfat_") {
			return errors.New("that looks like an API token — the Account ID is a 32-char hex string")
		}
		return errors.New("the Account ID must be 32 hex characters")
	}
	return nil
}

// validateAccessIdentity accepts an email or an @domain wildcard.
func validateAccessIdentity(s string) error {
	id := strings.TrimSpace(s)
	if id == "" {
		return errors.New("enter who Access should admit")
	}
	if strings.HasPrefix(id, "@") && strings.TrimPrefix(id, "@") == "" {
		return errors.New("enter a domain after the @, e.g. @your-domain")
	}
	return nil
}

func (m *edgeModel) sized(f *huh.Form) *huh.Form {
	if m.width > 0 {
		return f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

func (m *edgeModel) setSize(w, h int) {
	m.width, m.height = w, h
	if m.form != nil {
		m.form = m.form.WithWidth(w).WithHeight(h)
	}
}

func (m *edgeModel) Init() tea.Cmd { return nil }

// arrowNavOK yields ←/→ to the root's step rail only when no text field is
// focused: the intro and the terminal states take single-key actions, so the
// horizontal arrows are free, but while the form is up they belong to the cursor.
func (m *edgeModel) arrowNavOK() bool {
	switch m.step {
	case egForm, egWorking:
		return false
	default:
		return true
	}
}

func (m *edgeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case egLoginDoneMsg:
		m.detectCloudflared()
		if msg.err != nil && !m.certExists {
			m.loginNote = "cloudflared login did not complete: " + msg.err.Error()
		} else if !m.certExists {
			m.loginNote = "login finished but cert not found — try again"
		} else {
			m.loginNote = ""
		}
		return m, nil

	case egInstallDoneMsg:
		m.installing = false
		if msg.err != nil {
			m.loginNote = "install failed: " + msg.err.Error()
		} else {
			m.detectCloudflared()
			m.loginNote = ""
		}
		return m, nil

	case egSetupDoneMsg:
		m.working = ""
		m.prog = msg.progress
		if msg.err != nil {
			m.step = egError
			m.lastErr = msg.err
			return m, nil
		}
		m.step = egDone
		m.result = msg.result
		return m, nil

	case spinner.TickMsg:
		if m.step == egWorking {
			var cmd tea.Cmd
			m.sp, cmd = m.sp.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		switch m.step {
		case egIntro:
			return m.handleIntroKey(msg)
		case egWorking:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		case egDone:
			switch msg.String() {
			case "ctrl+c", "esc", "enter":
				return m, m.sendEdgeResult()
			}
			return m, nil
		case egError:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "enter":
				// Re-run with the same (still-valid) inputs. Setup is idempotent,
				// so a retry recovers cleanly from a transient failure.
				return m, m.startSetup()
			case "esc":
				m.step = egForm
				m.lastErr, m.prog = nil, nil
				m.form = m.build()
				return m, m.form.Init()
			}
			return m, nil
		case egForm:
			// Intercept the exits before huh sees them: huh collapses esc and ctrl+c
			// into a single StateAborted, so we can't tell them apart afterward.
			// esc steps back to the intro/status screen; ctrl+c quits, matching every
			// sibling screen. Any other key falls through to the form below.
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.step = egIntro
				return m, nil
			}
		}
	}

	if m.step == egForm && m.form != nil {
		form, cmd := m.form.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			m.form = f
		}
		switch m.form.State {
		case huh.StateCompleted:
			return m, m.startSetup()
		case huh.StateAborted:
			// esc on the form steps back to the intro/status screen.
			m.step = egIntro
			return m, nil
		}
		return m, cmd
	}
	return m, nil
}

func (m *edgeModel) View() string {
	switch m.step {
	case egForm:
		if m.form == nil {
			return ""
		}
		return m.form.View()

	case egWorking:
		msg := m.working
		if msg == "" {
			msg = "Configuring Cloudflare Tunnel + Access edge…"
		}
		return "  " + m.sp.View() + " " + tuiHint.Render(msg) + "\n"

	case egDone:
		return m.doneView()

	case egError:
		return m.errorView()

	default:
		return m.introView()
	}
}

func (m *edgeModel) introView() string {
	var b strings.Builder
	b.WriteString(tuiHint.Render("Publish the panel via Cloudflare Tunnel + Access — no open ports; TLS and the admin identity check are handled at Cloudflare's edge.") + "\n\n")
	b.WriteString(tuiLabel.Render("Status") + "\n")
	if m.cloudflaredPath == "" {
		b.WriteString("  " + tuiErr.Render("✗ cloudflared not installed") + " — press i to install\n\n")
	} else {
		b.WriteString("  " + tuiOK.Render("✓ cloudflared") + " " + tuiHint.Render(m.cloudflaredPath) + "\n")
		if m.certExists {
			b.WriteString("  " + tuiOK.Render("✓ logged in") + "\n\n")
		} else {
			b.WriteString("  " + tuiWarn.Render("⟳ not logged in") + " — press l for browser login\n\n")
		}
	}
	if m.installing {
		b.WriteString("  " + tuiIconSpin + " " + tuiHint.Render("downloading cloudflared…") + "\n\n")
	}
	if m.loginNote != "" {
		b.WriteString(tuiHint.Render(m.loginNote) + "\n\n")
	}
	if panel := defaultPanelHostname(m.rootDomain, m.panelHostname); panel != "" {
		b.WriteString(tuiHint.Render("Player console: "+panel) + "\n")
	}
	if admin := defaultAdminHostname(m.rootDomain, m.adminHostname); admin != "" {
		b.WriteString(tuiHint.Render("Admin console: "+admin) + tuiHint.Render(" (Access-guarded)") + "\n")
	}
	b.WriteString("\n" + tuiSeparator() + "\n")
	switch {
	case m.cloudflaredPath != "" && m.certExists:
		b.WriteString(tuiAction("enter", "continue", "esc", "back"))
	case m.cloudflaredPath == "":
		b.WriteString(tuiAction("i", "install cloudflared", "esc", "back"))
	default:
		b.WriteString(tuiAction("l", "login", "esc", "back"))
	}
	return b.String()
}

func (m *edgeModel) doneView() string {
	var b strings.Builder
	b.WriteString(tuiOK.Render("✓ Cloudflare edge configured.") + "\n\n")
	var card strings.Builder
	if m.result != nil {
		card.WriteString(tuiLabel.Render("access_jwt_aud  ") + m.result.AccessAud + "\n")
		if len(m.result.RoutedHostnames) > 0 {
			card.WriteString(tuiLabel.Render("routed          ") + strings.Join(m.result.RoutedHostnames, ", ") + "\n")
		}
		if m.result.ConfigPath != "" {
			card.WriteString(tuiLabel.Render("tunnel config   ") + m.result.ConfigPath + "\n")
		}
	}
	b.WriteString(tuiCardStyle.Render(strings.TrimRight(card.String(), "\n")) + "\n\n")
	b.WriteString(tuiHint.Render("ℹ Felis config, Kubernetes Secret, API rollout and the cloudflared service were all updated.") + "\n")
	b.WriteString("\n" + tuiAction("enter/esc", "continue"))
	return b.String()
}

func (m *edgeModel) errorView() string {
	var b strings.Builder
	b.WriteString(tuiErr.Render("✗ Edge setup failed.") + "\n\n")
	if len(m.prog) > 0 {
		b.WriteString(tuiHint.Render("Completed before the failure:") + "\n")
		for _, s := range m.prog {
			b.WriteString("  " + tuiOK.Render("✓") + " " + tuiHint.Render(s) + "\n")
		}
		b.WriteString("\n")
	}
	if m.lastErr != nil {
		b.WriteString(tuiHint.Render(m.lastErr.Error()) + "\n")
	}
	// Nothing done above is rolled back, and nothing needs to be: every step finds what an
	// earlier attempt created (the tunnel, its DNS route, the Access app and policy) and
	// carries on from it.
	b.WriteString("\n" + tuiHint.Render("Retrying is safe: it reuses the tunnel, DNS record and Access app created so far instead of making duplicates.") + "\n")
	b.WriteString("\n" + tuiAction("enter", "retry", "esc", "edit"))
	return b.String()
}

func (m *edgeModel) handleIntroKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		return m, goBack()
	case "i", "I":
		if m.cloudflaredPath == "" {
			m.installing = true
			return m, m.installCloudflared()
		}
	case "l", "L":
		if m.cloudflaredPath != "" && !m.certExists {
			return m.startLogin()
		}
	case "enter":
		if m.cloudflaredPath != "" && m.certExists {
			m.step = egForm
			m.form = m.build()
			return m, m.form.Init()
		}
	}
	return m, nil
}

// startSetup reads the form-bound fields (already validated by huh), normalizes
// the hostnames, applies the tunnel/config defaults, and kicks off the live
// Cloudflare run. It is shared by the form-completed path and the error retry, so
// a retry re-runs against the same inputs without a re-entry.
func (m *edgeModel) startSetup() tea.Cmd {
	m.panelHost = normalizeEdgeHostname(m.panelHost)
	m.adminHost = normalizeEdgeHostname(m.adminHost)
	identity := strings.TrimSpace(m.identity)
	tunnel := strings.TrimSpace(m.tunnelName)
	if tunnel == "" {
		tunnel = defaultTunnelName
	}
	cfgPath := strings.TrimSpace(m.cfgPath)
	if cfgPath == "" {
		cfgPath = defaultTunnelConfigPath
	}
	m.tunnelName, m.cfgPath = tunnel, cfgPath

	m.panelSet, m.adminSet = m.panelHost, m.adminHost
	m.prog = nil
	m.lastErr = nil
	m.step = egWorking
	m.working = "Configuring Cloudflare Tunnel + Access edge…"

	var id cfsetup.AccessIdentity
	if strings.HasPrefix(identity, "@") {
		id.EmailDomains = []string{strings.TrimPrefix(identity, "@")}
	} else {
		id.Emails = []string{identity}
	}

	runner := &cfsetup.ExecRunner{
		Cloudflared: m.cloudflaredPath,
		APIToken:    m.authToken,
		AccountID:   m.authAccount,
	}
	p := cfsetup.Params{
		PanelHostname:  m.panelHost,
		AdminHostname:  m.adminHost,
		PanelOrigin:    localPanelOrigin(),
		TunnelName:     tunnel,
		ConfigPath:     cfgPath,
		AccessIdentity: id,
		Pre:            cfsetup.DetectPreconditions(m.authToken),
	}

	return tea.Batch(m.sp.Tick, m.runEdgeSetup(runner, p))
}

func (m *edgeModel) runEdgeSetup(runner cfsetup.Runner, p cfsetup.Params) tea.Cmd {
	var progress []string
	p.OnProgress = func(step string) {
		progress = append(progress, step)
	}
	return func() tea.Msg {
		result, err := cfsetup.Setup(context.Background(), runner, p)
		if err != nil {
			return egSetupDoneMsg{err: err, progress: progress}
		}
		progress = append(progress, "Applying Felis config and starting cloudflared…")
		if err := applyCloudflareEdge(context.Background(), result, p.PanelHostname, p.AdminHostname, m.cloudflaredPath); err != nil {
			return egSetupDoneMsg{err: err, progress: progress}
		}
		progress = append(progress, "Updated Felis config and started cloudflared")
		result.Progress = progress
		return egSetupDoneMsg{result: result, progress: progress}
	}
}

func (m *edgeModel) sendEdgeResult() tea.Cmd {
	return func() tea.Msg {
		return connectResultMsg{
			method:        connectCloudflare,
			edge:          m.result,
			panelHostname: m.panelSet,
			adminHostname: m.adminSet,
		}
	}
}

func (m *edgeModel) startLogin() (tea.Model, tea.Cmd) {
	c := exec.CommandContext(context.Background(), m.cloudflaredPath, "tunnel", "login")
	return m, tea.ExecProcess(c, func(err error) tea.Msg {
		return egLoginDoneMsg{err: err}
	})
}

func (m *edgeModel) installCloudflared() tea.Cmd {
	return func() tea.Msg {
		arch := "amd64"
		if out, err := exec.Command("uname", "-m").Output(); err == nil {
			if strings.TrimSpace(string(out)) == "aarch64" {
				arch = "arm64"
			}
		}
		url := "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-" + arch
		resp, err := http.Get(url)
		if err != nil {
			return egInstallDoneMsg{err: fmt.Errorf("download: %w", err)}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return egInstallDoneMsg{err: fmt.Errorf("download returned status %d", resp.StatusCode)}
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, resp.Body); err != nil {
			return egInstallDoneMsg{err: fmt.Errorf("read: %w", err)}
		}
		if err := os.WriteFile("/usr/local/bin/cloudflared", buf.Bytes(), 0o755); err != nil {
			return egInstallDoneMsg{err: fmt.Errorf("install: %w", err)}
		}
		return egInstallDoneMsg{}
	}
}
