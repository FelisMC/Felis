package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"felis.lolicon.best/internal/cfsetup"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type egStep int

const (
	egIntro egStep = iota
	egAuth
	egConfig
	egWorking
	egDone
	egError
)

type egAuthDoneMsg struct {
	token   string
	account string
	err     error
}

type egLoginDoneMsg struct{ err error }

type egInstallDoneMsg struct{ err error }

type egSetupDoneMsg struct {
	result *cfsetup.Result
	err    error
}

type edgeModel struct {
	step egStep

	rootDomain    string
	adminHostname string
	panelHostname string

	// cloudflared detection
	cloudflaredPath string
	certExists      bool
	installing      bool
	loginNote       string

	// auth step inputs
	authInputs  []textinput.Model
	authFocus   int
	authErr     string
	authToken   string
	authAccount string

	// config step inputs
	cfgInputs []textinput.Model
	cfgFocus  int
	cfgErr    string

	// working / result
	working  string
	lastErr  error
	prog     []string
	result   *cfsetup.Result
	panelSet string
	adminSet string
}

func newEdgeModel(rootDomain, adminHost, panelHost string) *edgeModel {
	m := &edgeModel{
		step:          egIntro,
		rootDomain:    rootDomain,
		adminHostname: adminHost,
		panelHostname: panelHost,
	}
	m.detectCloudflared()
	return m
}

func (m *edgeModel) detectCloudflared() {
	pre := cfsetup.DetectPreconditions("")
	m.cloudflaredPath = pre.CloudflaredPath
	m.certExists = pre.CertExists
}

func (m *edgeModel) Init() tea.Cmd { return nil }

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

	case egAuthDoneMsg:
		if msg.err != nil {
			m.authErr = msg.err.Error()
			return m, nil
		}
		m.authToken = msg.token
		m.authAccount = msg.account
		m.step = egConfig
		return m, m.enterConfig()

	case egSetupDoneMsg:
		m.working = ""
		if msg.err != nil {
			m.step = egError
			m.lastErr = msg.err
			return m, nil
		}
		m.step = egDone
		m.result = msg.result
		if msg.result != nil {
			m.prog = msg.result.Progress
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *edgeModel) View() string {
	var b strings.Builder
	b.WriteString(tuiHeader("Cloudflare Edge"))

	switch m.step {
	case egIntro:
		b.WriteString(tuiHint.Render("Publish the panel via Cloudflare Tunnel + Access — no open ports, TLS and admin identity handled by Cloudflare. Press esc to pick a different method.") + "\n\n")
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
		if m.loginNote != "" {
			b.WriteString(tuiHint.Render(m.loginNote) + "\n\n")
		}
		if panel := defaultPanelHostname(m.rootDomain, m.panelHostname); panel != "" {
			b.WriteString(tuiHint.Render("Player console: "+panel) + "\n")
		}
		if admin := defaultAdminHostname(m.rootDomain, m.adminHostname); admin != "" {
			b.WriteString(tuiHint.Render("Admin console: "+admin) + " (Access-guarded)\n")
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

	case egAuth:
		b.WriteString(tuiHint.Render("Step 1/2: Enter your Cloudflare credentials.") + "\n\n")
		b.WriteString(tuiWizardCard("API Token & Account ID",
			"Create a Bearer token at: "+cloudflareAccessTokenTemplateURL,
			tuiFormField("API token", m.authInputs[0])+"\n\n"+
				tuiFormField("Account ID", m.authInputs[1])))
		b.WriteString("\n" + tuiInfo("Account ID is in the Cloudflare Dashboard URL: dash.cloudflare.com/<this-part>") + "\n")
		if m.authErr != "" {
			b.WriteString("\n" + tuiErrorBanner(m.authErr) + "\n")
		}
		b.WriteString("\n" + tuiSeparator() + "\n")
		b.WriteString(tuiAction("tab/↑↓", "move", "enter", "continue", "esc", "back"))

	case egConfig:
		b.WriteString(tuiHint.Render("Step 2/2: Choose hostnames and who gets access.") + "\n\n")
		labels := []string{"Admit (your email, or @your-domain)", "Player console hostname", "Admin console hostname", "Tunnel name", "Config path"}
		var fields string
		for i, lbl := range labels {
			if i > 0 {
				fields += "\n\n"
			}
			fields += tuiFormField(lbl, m.cfgInputs[i])
		}
		b.WriteString(tuiWizardCard("Hostnames & Identity", "", fields))
		if m.cfgErr != "" {
			b.WriteString("\n" + tuiErrorBanner(m.cfgErr) + "\n")
		}
		b.WriteString("\n" + tuiSeparator() + "\n")
		b.WriteString(tuiAction("tab/↑↓", "move", "enter", "configure", "esc", "back"))

	case egWorking:
		b.WriteString(tuiHint.Render("Configuring Cloudflare Tunnel + Access edge…") + "\n\n")
		for _, s := range m.prog {
			b.WriteString("  " + tuiOK.Render("✓") + " " + s + "\n")
		}
		if m.working != "" {
			b.WriteString("  " + tuiIconSpin + " " + m.working + "\n")
		}
		if len(m.prog) == 0 && m.working == "" {
			b.WriteString(tuiHint.Render("Starting…") + "\n")
		}

	case egDone:
		b.WriteString(tuiSuccessBanner("Cloudflare edge configured.") + "\n\n")
		var box string
		if m.result != nil {
			box = tuiLabel.Render("access_jwt_aud  ") + m.result.AccessAud + "\n"
			if len(m.result.RoutedHostnames) > 0 {
				box += tuiLabel.Render("routed          ") + strings.Join(m.result.RoutedHostnames, ", ") + "\n"
			}
			if m.result.ConfigPath != "" {
				box += tuiLabel.Render("tunnel config   ") + m.result.ConfigPath + "\n"
			}
		}
		b.WriteString(tuiCardStyle.Render(box) + "\n\n")
		b.WriteString(tuiOK.Render("✓") + " Felis config, Kubernetes Secret, API rollout and cloudflared service updated.\n")
		b.WriteString("\n" + tuiSeparator() + "\n")
		b.WriteString(tuiAction("enter/esc", "back"))

	case egError:
		b.WriteString(tuiErrorBanner("Edge setup failed.") + "\n\n")
		if len(m.prog) > 0 {
			b.WriteString(tuiHint.Render("Completed before failure:") + "\n")
			for _, s := range m.prog {
				b.WriteString("  " + tuiOK.Render("✓") + " " + s + "\n")
			}
			b.WriteString("\n")
		}
		if m.lastErr != nil {
			b.WriteString(tuiHint.Render(m.lastErr.Error()) + "\n")
		}
		b.WriteString("\n" + tuiSeparator() + "\n")
		b.WriteString(tuiAction("enter", "retry", "esc", "back"))
	}
	return b.String()
}

func (m *edgeModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.step {
	case egIntro:
		return m.handleIntroKey(msg)
	case egAuth:
		return m.handleAuthKey(msg)
	case egConfig:
		return m.handleCfgKey(msg)
	case egDone, egError:
		switch msg.String() {
		case "ctrl+c", "esc":
			if m.step == egDone {
				return m, m.sendEdgeResult()
			}
			return m, goBack()
		case "enter":
			if m.step == egDone {
				return m, m.sendEdgeResult()
			}
			if m.step == egError {
				m.step = egConfig
				m.lastErr = nil
				m.prog = nil
				return m, nil
			}
			return m, nil
		}
	default:
	}
	return m, nil
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
			nm, cmd := m.startLogin()
			return nm, cmd
		}
	case "enter":
		if m.cloudflaredPath != "" && m.certExists {
			m.step = egAuth
			return m, m.enterAuth()
		}
	}
	return m, nil
}

func (m *edgeModel) handleAuthKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.step = egIntro
		m.authErr = ""
		return m, nil
	case "tab", "down":
		m.authFocus = (m.authFocus + 1) % 2
		return m, m.focusAuthInput(m.authFocus)
	case "shift+tab", "up":
		m.authFocus = (m.authFocus + 1) % 2
		return m, m.focusAuthInput(m.authFocus)
	case "enter":
		return m.submitAuth()
	}
	return m, m.updateAuthInputs(msg)
}

func (m *edgeModel) handleCfgKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.step = egAuth
		m.cfgErr = ""
		return m, nil
	case "tab", "down":
		m.cfgFocus = (m.cfgFocus + 1) % 5
		return m, m.focusCfgInput(m.cfgFocus)
	case "shift+tab", "up":
		m.cfgFocus = (m.cfgFocus + 4) % 5
		return m, m.focusCfgInput(m.cfgFocus)
	case "enter":
		return m.submitConfig()
	}
	return m, m.updateCfgInputs(msg)
}

func (m *edgeModel) enterAuth() tea.Cmd {
	token := tuiInput("cfat_…", 200, true)
	account := tuiInput("32-char account ID", 64, false)
	m.authInputs = []textinput.Model{token, account}
	m.authFocus = 0
	return m.focusAuthInput(0)
}

func (m *edgeModel) focusAuthInput(i int) tea.Cmd {
	var cmd tea.Cmd
	for j := range m.authInputs {
		if j == i {
			cmd = m.authInputs[j].Focus()
		} else {
			m.authInputs[j].Blur()
		}
	}
	return cmd
}

func (m *edgeModel) updateAuthInputs(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, len(m.authInputs))
	for i := range m.authInputs {
		m.authInputs[i], cmds[i] = m.authInputs[i].Update(msg)
	}
	return tea.Batch(cmds...)
}

func (m *edgeModel) submitAuth() (tea.Model, tea.Cmd) {
	token := strings.TrimSpace(m.authInputs[0].Value())
	account := strings.TrimSpace(m.authInputs[1].Value())
	if token == "" {
		m.authErr = "a Cloudflare API token is required"
		return m, nil
	}
	if account == "" {
		m.authErr = "the Cloudflare account ID is required"
		return m, nil
	}
	if !isHex32(account) {
		if strings.HasPrefix(account, "cfat_") {
			m.authErr = "that looks like an API token — the Account ID is a 32-char hex string"
		} else {
			m.authErr = "the Account ID must be 32 hex characters"
		}
		return m, nil
	}
	m.authErr = ""
	return m, func() tea.Msg { return egAuthDoneMsg{token: token, account: account} }
}

func (m *edgeModel) enterConfig() tea.Cmd {
	identity := tuiInput("you@example.com  or  @your-domain", 254, false)
	panelHost := tuiInput(defaultPanelHostname(m.rootDomain, m.panelHostname), 253, false)
	panelHost.SetValue(defaultPanelHostname(m.rootDomain, m.panelHostname))
	adminHost := tuiInput(defaultAdminHostname(m.rootDomain, m.adminHostname), 253, false)
	adminHost.SetValue(defaultAdminHostname(m.rootDomain, m.adminHostname))
	tunnel := tuiInput(defaultTunnelName, 64, false)
	tunnel.SetValue(defaultTunnelName)
	cfgPath := tuiInput(defaultTunnelConfigPath, 256, false)
	cfgPath.SetValue(defaultTunnelConfigPath)
	m.cfgInputs = []textinput.Model{identity, panelHost, adminHost, tunnel, cfgPath}
	m.cfgFocus = 0
	return m.focusCfgInput(0)
}

func (m *edgeModel) focusCfgInput(i int) tea.Cmd {
	var cmd tea.Cmd
	for j := range m.cfgInputs {
		if j == i {
			cmd = m.cfgInputs[j].Focus()
		} else {
			m.cfgInputs[j].Blur()
		}
	}
	return cmd
}

func (m *edgeModel) updateCfgInputs(msg tea.Msg) tea.Cmd {
	cmds := make([]tea.Cmd, len(m.cfgInputs))
	for i := range m.cfgInputs {
		m.cfgInputs[i], cmds[i] = m.cfgInputs[i].Update(msg)
	}
	return tea.Batch(cmds...)
}

func (m *edgeModel) submitConfig() (tea.Model, tea.Cmd) {
	identity := strings.TrimSpace(m.cfgInputs[0].Value())
	panelHost := normalizeEdgeHostname(m.cfgInputs[1].Value())
	adminHost := normalizeEdgeHostname(m.cfgInputs[2].Value())
	tunnel := strings.TrimSpace(m.cfgInputs[3].Value())
	cfgPath := strings.TrimSpace(m.cfgInputs[4].Value())

	if identity == "" {
		m.cfgErr = "enter who Access should admit"
		m.cfgFocus = 0
		return m, nil
	}
	if strings.HasPrefix(identity, "@") && strings.TrimPrefix(identity, "@") == "" {
		m.cfgErr = "enter a domain after the @, e.g. @your-domain"
		m.cfgFocus = 0
		return m, nil
	}
	if err := validateEdgeHostname("player console", panelHost, false); err != nil {
		m.cfgErr = err.Error()
		m.cfgFocus = 1
		return m, nil
	}
	if err := validateEdgeHostname("admin console", adminHost, true); err != nil {
		m.cfgErr = err.Error()
		m.cfgFocus = 2
		return m, nil
	}
	if panelHost != "" && strings.EqualFold(panelHost, adminHost) {
		m.cfgErr = "player console and admin console hostnames must be different"
		m.cfgFocus = 2
		return m, nil
	}
	if tunnel == "" {
		tunnel = defaultTunnelName
	}
	if cfgPath == "" {
		cfgPath = defaultTunnelConfigPath
	}

	m.panelSet = panelHost
	m.adminSet = adminHost
	m.cfgErr = ""
	m.step = egWorking
	m.working = "Starting…"

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
		PanelHostname:  panelHost,
		AdminHostname:  adminHost,
		PanelOrigin:    localPanelOrigin(),
		TunnelName:     tunnel,
		ConfigPath:     cfgPath,
		AccessIdentity: id,
		Pre:            cfsetup.DetectPreconditions(m.authToken),
	}

	return m, m.runEdgeSetup(runner, p)
}

func (m *edgeModel) runEdgeSetup(runner cfsetup.Runner, p cfsetup.Params) tea.Cmd {
	var progress []string
	p.OnProgress = func(step string) {
		progress = append(progress, step)
	}
	return func() tea.Msg {
		result, err := cfsetup.Setup(context.Background(), runner, p)
		if err != nil {
			return egSetupDoneMsg{err: err}
		}
		progress = append(progress, "Applying Felis config and starting cloudflared…")
		if err := applyCloudflareEdge(context.Background(), result, p.PanelHostname, p.AdminHostname, m.cloudflaredPath); err != nil {
			return egSetupDoneMsg{err: err}
		}
		progress = append(progress, "Updated Felis config and started cloudflared")
		result.Progress = progress
		return egSetupDoneMsg{result: result}
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
