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

// connectChooserModel presents the ways to reach the panel as peer choices.
// None is privileged: "Local" installs nothing, "Cloudflare Tunnel" is a
// turnkey integration, and "Reverse proxy" just records hostnames and hands the
// operator a copy-paste guide. The admin console is gated by the Owner's
// local session (passwordless sign-in) regardless; Cloudflare Access is an
// *additional* layer.
type connectChooserModel struct {
	rootDomain string
	adminHost  string
	panelHost  string

	form          *huh.Form
	choice        connectMethod
	width, height int
}

func newConnectChooserModel(rootDomain, adminHost, panelHost string) *connectChooserModel {
	m := &connectChooserModel{rootDomain: rootDomain, adminHost: adminHost, panelHost: panelHost}
	m.form = m.build()
	return m
}

func (m *connectChooserModel) build() *huh.Form {
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewSelect[connectMethod]().
			Title("How should people reach the panel?").
			Description("You can change this later in the panel.").
			Value(&m.choice).
			Options(
				huh.NewOption("Local only · nothing installed", connectLocal),
				huh.NewOption("Cloudflare Tunnel + Access · no open ports", connectCloudflare),
				huh.NewOption("Reverse proxy (bring your own) · guided", connectReverseProxy),
			),
		// A dim, untitled footnote — deliberately subordinate to the picker above
		// so the screen reads as a menu, not an info page.
		huh.NewNote().Description(
			"⚠  Local / reverse proxy gate the admin console on your Owner sign-in alone "+
				"(passkey / email code). Cloudflare Access adds an edge check in front."),
	)))
}

func (m *connectChooserModel) sized(f *huh.Form) *huh.Form {
	if m.width > 0 {
		return f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

func (m *connectChooserModel) setSize(w, h int) {
	m.width, m.height = w, h
	if m.form != nil {
		m.form = m.form.WithWidth(w).WithHeight(h)
	}
}

func (m *connectChooserModel) Init() tea.Cmd { return m.form.Init() }

func (m *connectChooserModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			// Skipping is choosing local — the operator can change this later.
			return m, m.chooseLocal()
		}
	}

	form, cmd := m.form.Update(msg)
	if f, ok := form.(*huh.Form); ok {
		m.form = f
	}
	switch m.form.State {
	case huh.StateCompleted:
		return m.onComplete()
	case huh.StateAborted:
		return m, tea.Quit
	}
	return m, cmd
}

func (m *connectChooserModel) onComplete() (tea.Model, tea.Cmd) {
	switch m.choice {
	case connectCloudflare:
		return newEdgeModel(m.rootDomain, m.adminHost, m.panelHost), nil
	case connectReverseProxy:
		return newReverseProxyModel(m.rootDomain, m.adminHost, m.panelHost), nil
	default:
		return m, m.chooseLocal()
	}
}

func (m *connectChooserModel) chooseLocal() tea.Cmd {
	panel := defaultPanelHostname(m.rootDomain, m.panelHost)
	admin := defaultAdminHostname(m.rootDomain, m.adminHost)
	return func() tea.Msg {
		return connectResultMsg{method: connectLocal, panelHostname: panel, adminHostname: admin}
	}
}

func (m *connectChooserModel) View() string { return m.form.View() }

// arrowNavOK lets the root repurpose ←/→ to walk the step rail: this screen
// navigates its options with ↑/↓, so the horizontal arrows are free.
func (m *connectChooserModel) arrowNavOK() bool { return true }

// ---- Reverse proxy: collect hostnames, record them, render a guide ----

type rpStep int

const (
	rpForm rpStep = iota
	rpWorking
	rpGuide
	rpError
)

type rpApplyMsg struct{ err error }

type reverseProxyModel struct {
	rootDomain string

	step          rpStep
	form          *huh.Form
	sp            spinner.Model
	err           error
	panelHost     string
	adminHost     string
	width, height int
}

func newReverseProxyModel(rootDomain, adminHost, panelHost string) *reverseProxyModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = tuiLabel

	m := &reverseProxyModel{
		rootDomain: rootDomain,
		step:       rpForm,
		sp:         sp,
		panelHost:  defaultPanelHostname(rootDomain, panelHost),
		adminHost:  defaultAdminHostname(rootDomain, adminHost),
	}
	m.form = m.build()
	return m
}

func (m *reverseProxyModel) build() *huh.Form {
	return m.sized(newFelisForm(huh.NewGroup(
		huh.NewNote().
			Title("Reverse proxy").
			Description("Enter the public hostnames your reverse proxy will serve. We record them and show you the config — you point the proxy at the origin."),
		huh.NewInput().
			Title("Player console hostname").
			Value(&m.panelHost).
			Validate(func(s string) error {
				return validateEdgeHostname("player console", normalizeEdgeHostname(s), false)
			}),
		huh.NewInput().
			Title("Admin console hostname").
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
	)))
}

func (m *reverseProxyModel) sized(f *huh.Form) *huh.Form {
	if m.width > 0 {
		return f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

func (m *reverseProxyModel) setSize(w, h int) {
	m.width, m.height = w, h
	if m.form != nil {
		m.form = m.form.WithWidth(w).WithHeight(h)
	}
}

func (m *reverseProxyModel) Init() tea.Cmd { return m.form.Init() }

func (m *reverseProxyModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case rpApplyMsg:
		if msg.err != nil {
			m.step, m.err = rpError, msg.err
			return m, nil
		}
		m.step = rpGuide
		return m, nil

	case spinner.TickMsg:
		if m.step == rpWorking {
			var cmd tea.Cmd
			m.sp, cmd = m.sp.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		switch m.step {
		case rpForm:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				return m, goBack()
			}
		case rpGuide:
			switch msg.String() {
			case "ctrl+c", "esc", "enter":
				return m, func() tea.Msg {
					return connectResultMsg{
						method:        connectReverseProxy,
						panelHostname: m.panelHost,
						adminHostname: m.adminHost,
						guide:         reverseProxyGuide(m.panelHost, m.adminHost),
					}
				}
			}
			return m, nil
		case rpError:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.step, m.err = rpForm, nil
				m.form = m.build()
				return m, m.form.Init()
			case "enter":
				return m, m.apply()
			}
			return m, nil
		case rpWorking:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		}
	}

	if m.step == rpForm && m.form != nil {
		form, cmd := m.form.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			m.form = f
		}
		switch m.form.State {
		case huh.StateCompleted:
			m.panelHost = normalizeEdgeHostname(m.panelHost)
			m.adminHost = normalizeEdgeHostname(m.adminHost)
			m.step = rpWorking
			return m, tea.Batch(m.sp.Tick, m.apply())
		case huh.StateAborted:
			return m, goBack()
		}
		return m, cmd
	}
	return m, nil
}

func (m *reverseProxyModel) apply() tea.Cmd {
	panel, admin := m.panelHost, m.adminHost
	return func() tea.Msg {
		return rpApplyMsg{err: applyReverseProxy(context.Background(), panel, admin)}
	}
}

func (m *reverseProxyModel) View() string {
	switch m.step {
	case rpWorking:
		return "  " + m.sp.View() + " " + tuiHint.Render("Recording hostnames and rolling the API…") + "\n"
	case rpGuide:
		var b strings.Builder
		b.WriteString(tuiSuccessBanner("Hostnames recorded. Now point your reverse proxy at the origin.") + "\n\n")
		b.WriteString(reverseProxyGuideView(m.panelHost, m.adminHost))
		b.WriteString("\n" + tuiAction("enter", "done"))
		return b.String()
	case rpError:
		var b strings.Builder
		b.WriteString(tuiErrorBanner("Could not record hostnames.") + "\n\n")
		if m.err != nil {
			b.WriteString(tuiHint.Render(m.err.Error()) + "\n")
		}
		b.WriteString("\n" + tuiAction("enter", "retry", "esc", "edit"))
		return b.String()
	default:
		if m.form == nil {
			return ""
		}
		return m.form.View()
	}
}

// reverseProxyGuideView renders the operator-facing guide with styled snippets.
func reverseProxyGuideView(panelHost, adminHost string) string {
	var b strings.Builder
	origin := localPanelOrigin()
	b.WriteString(tuiInfo("Origin: "+origin+"  ·  self-signed cert (skip upstream TLS verify)  ·  forward the Host header") + "\n\n")
	b.WriteString(tuiGuideBlock("Caddyfile", caddySnippet(adminHost, origin)))
	if panelHost != "" && !strings.EqualFold(panelHost, adminHost) {
		b.WriteString("\n" + tuiGuideBlock("", caddySnippet(panelHost, origin)))
	}
	return b.String()
}

// reverseProxyGuide returns the same guidance as plain text for the post-TUI
// stdout summary (e.g. when piped to a log).
func reverseProxyGuide(panelHost, adminHost string) string {
	origin := localPanelOrigin()
	var b strings.Builder
	fmt.Fprintf(&b, "Origin: %s (self-signed — skip upstream TLS verification; forward the Host header)\n\n", origin)
	b.WriteString("Caddy example:\n")
	b.WriteString(caddySnippet(adminHost, origin))
	if panelHost != "" && !strings.EqualFold(panelHost, adminHost) {
		b.WriteString("\n")
		b.WriteString(caddySnippet(panelHost, origin))
	}
	return b.String()
}

func caddySnippet(host, origin string) string {
	if host == "" {
		host = "your-hostname"
	}
	return fmt.Sprintf(`%s {
    reverse_proxy %s {
        transport http { tls_insecure_skip_verify }
        header_up Host {host}
    }
}`, host, origin)
}
