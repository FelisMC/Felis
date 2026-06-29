package main

import (
	"context"

	"felis.lolicon.best/internal/cfsetup"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// sizeable is implemented by screens that need to know the terminal area
// available to them (after the root reserves space for the step rail). huh-based
// screens forward this to form.WithWidth/WithHeight; custom screens use it to
// avoid overflowing the frame.
type sizeable interface {
	setSize(width, height int)
}

// ---- Connection methods ----

type connectMethod int

const (
	connectLocal connectMethod = iota
	connectCloudflare
	connectReverseProxy
)

func connectMethodLabel(m connectMethod) string {
	switch m {
	case connectCloudflare:
		return "Cloudflare Tunnel + Access"
	case connectReverseProxy:
		return "Reverse proxy (your own front)"
	default:
		return "Local only (NodePort + self-signed TLS)"
	}
}

// ---- Messages: sub-model → root ----

type preflightDoneMsg struct{}

type ownerResultMsg struct {
	username        string
	displayPassword string
	mode            string
	accountable     string
	auditWarning    string
	err             error
}

// connectResultMsg is emitted by every connection method (the chooser for
// local, the edge model for Cloudflare, the reverse-proxy model for BYO). It is
// the single, method-agnostic outcome the root advances on.
type connectResultMsg struct {
	method        connectMethod
	panelHostname string
	adminHostname string
	edge          *cfsetup.Result // Cloudflare only
	guide         string          // reverse-proxy only
}

// goBackMsg returns from a connection sub-screen to the chooser.
type goBackMsg struct{}

func goBack() tea.Cmd { return func() tea.Msg { return goBackMsg{} } }

// reconfigureConnectMsg is sent from the re-run status screen to re-enter the
// connection chooser.
type reconfigureConnectMsg struct{}

// ---- rootModel: top-level session ----

type wizardStage int

const (
	stagePreflight wizardStage = iota
	stageOwner
	stageConnect
	stageSummary
)

type rootModel struct {
	ctx context.Context

	screen tea.Model
	stage  wizardStage

	width  int
	height int

	result breakGlassResult
	err    error

	mode        consoleMode
	dbURL       string
	store       ownerStore
	osUser      string
	rootDomain  string
	adminHost   string
	panelHost   string
	accessAud   string
	adminExists bool
}

func newRootModel(ctx context.Context, store ownerStore, dbURL, rootDomain, adminHostname, panelHostname, accessAud, osUser string, adminExists bool, mode consoleMode) *rootModel {
	rm := &rootModel{
		ctx:         ctx,
		dbURL:       dbURL,
		store:       store,
		osUser:      osUser,
		rootDomain:  rootDomain,
		adminHost:   adminHostname,
		panelHost:   panelHostname,
		accessAud:   accessAud,
		adminExists: adminExists,
		mode:        mode,
		result: breakGlassResult{
			osUser:        osUser,
			rootDomain:    rootDomain,
			adminHostname: adminHostname,
		},
	}
	if mode == consoleModeBreakGlass {
		rm.stage = stageOwner
		rm.screen = newOwnerModel(ctx, store, osUser, adminExists)
	} else {
		rm.stage = stagePreflight
		rm.screen = newPreflightModel(dbURL, rootDomain)
	}
	return rm
}

func (m *rootModel) Init() tea.Cmd {
	return m.screen.Init()
}

func (m *rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.pushSize()
		return m, nil

	case preflightDoneMsg:
		if m.adminExists {
			// Re-run: setup already happened. Land on the status screen.
			return m.showStatus()
		}
		m.stage = stageOwner
		return m.adopt(newOwnerModel(m.ctx, m.store, m.osUser, false))

	case ownerResultMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, tea.Quit
		}
		m.result.provisioned = true
		m.result.username = msg.username
		m.result.displayPassword = msg.displayPassword
		m.result.mode = msg.mode
		m.result.accountable = msg.accountable
		m.result.auditWarning = msg.auditWarning
		if m.mode == consoleModeBreakGlass {
			return m, tea.Quit
		}
		m.adminExists = true
		m.stage = stageConnect
		return m.adopt(newConnectChooserModel(m.rootDomain, m.adminHost, m.panelHost))

	case connectResultMsg:
		m.applyConnectResult(msg)
		return m.showSummary()

	case goBackMsg:
		m.stage = stageConnect
		return m.adopt(newConnectChooserModel(m.rootDomain, m.adminHost, m.panelHost))

	case reconfigureConnectMsg:
		m.stage = stageConnect
		return m.adopt(newConnectChooserModel(m.rootDomain, m.adminHost, m.panelHost))
	}

	if m.screen != nil {
		newScreen, cmd := m.screen.Update(msg)
		if newScreen != nil && newScreen != m.screen {
			adopted, initCmd := m.adopt(newScreen)
			return adopted, tea.Batch(cmd, initCmd)
		}
		return m, cmd
	}
	return m, nil
}

// adopt installs a new screen, hands it the current size, and returns its Init.
// Centralizing screen swaps here guarantees every screen is sized before its
// first View — the fix for the rail being clipped off the top of the frame.
func (m *rootModel) adopt(s tea.Model) (tea.Model, tea.Cmd) {
	m.screen = s
	m.pushSize()
	return m, s.Init()
}

// pushSize gives the active screen the area left after the step rail.
func (m *rootModel) pushSize() {
	if m.height == 0 {
		return
	}
	if s, ok := m.screen.(sizeable); ok {
		// Reserve one extra row: huh renders a hair taller than its WithHeight
		// budget (the help line sits outside it). Clipping the rail off the top is
		// the bug we're fixing, so we round the budget down — a blank bottom row is
		// invisible, an overflowed top is not.
		avail := m.height - m.chromeHeight() - 1
		if avail < 1 {
			avail = 1
		}
		s.setSize(m.width, avail)
	}
}

// chromeHeight is the number of rows the root paints around the screen — the
// rail plus its blank separator, or 0 when the rail is hidden.
func (m *rootModel) chromeHeight() int {
	if m.mode != consoleModeSetup || m.adminExistsAtStart() {
		return 0
	}
	return lipgloss.Height(m.rail()) + 1
}

func (m *rootModel) View() string {
	if m.screen == nil {
		return ""
	}
	if m.mode == consoleModeSetup && !m.adminExistsAtStart() {
		return m.rail() + "\n\n" + m.screen.View()
	}
	return m.screen.View()
}

// adminExistsAtStart reports whether this run began with an Owner already
// present (a re-run). The rail only makes sense for the first-run linear wizard.
func (m *rootModel) adminExistsAtStart() bool {
	// adminExists flips true once we provision the Owner mid-run; the rail should
	// keep showing through the connect/summary stages of that same first run. So
	// only suppress the rail when the Owner pre-existed AND we never provisioned.
	return m.adminExists && !m.result.provisioned
}

func (m *rootModel) rail() string {
	return tuiStepRail([]string{"Preflight", "Owner", "Connection", "Done"}, int(m.stage))
}

// applyConnectResult records the chosen connection outcome onto the result.
func (m *rootModel) applyConnectResult(msg connectResultMsg) {
	m.result.connectMethod = msg.method
	if msg.panelHostname != "" {
		m.result.panelHostname = msg.panelHostname
	}
	if msg.adminHostname != "" {
		m.result.adminHostname = msg.adminHostname
	}
	switch msg.method {
	case connectCloudflare:
		m.result.connectConfigured = true
		m.result.edgeConfigured = true
		if msg.edge != nil {
			m.result.edgeAud = msg.edge.AccessAud
			m.result.edgeRoutedHosts = msg.edge.RoutedHostnames
			m.result.edgeConfigPath = msg.edge.ConfigPath
		}
	case connectReverseProxy:
		m.result.connectConfigured = true
		m.result.reverseProxyGuide = msg.guide
	}
	m.result.panelURL = panelURLFor(msg.method, msg.panelHostname, m.rootDomain)
}

func (m *rootModel) showSummary() (tea.Model, tea.Cmd) {
	m.stage = stageSummary
	routed := m.result.edgeRoutedHosts
	if len(routed) == 0 && m.result.connectMethod == connectReverseProxy && m.result.panelHostname != "" {
		routed = []string{m.result.panelHostname}
	}
	return m.adopt(&summaryModel{
		panelURL:      m.result.panelURL,
		ownerUsername: m.result.username,
		ownerPassword: m.result.displayPassword,
		accessLabel:   connectMethodLabel(m.result.connectMethod),
		routedHosts:   routed,
		localHint:     m.result.connectMethod == connectLocal,
	})
}

// showStatus is the re-run landing: prove the backend is up, then point the
// operator at the panel without forcing any reconfiguration.
func (m *rootModel) showStatus() (tea.Model, tea.Cmd) {
	m.stage = stageSummary
	method := connectLocal
	accessLabel := "configured (manage in panel)"
	if m.accessAud != "" {
		method = connectCloudflare
		accessLabel = connectMethodLabel(connectCloudflare)
	}
	m.result.panelURL = panelURLFor(method, m.panelHost, m.rootDomain)
	return m.adopt(&summaryModel{
		panelURL:     m.result.panelURL,
		accessLabel:  accessLabel,
		alreadySetUp: true,
		localHint:    m.accessAud == "" && rootDomainEmbeddedIP(m.rootDomain) != "",
	})
}

func panelURLFor(method connectMethod, panelHostname, rootDomain string) string {
	if method != connectLocal && panelHostname != "" {
		return "https://" + panelHostname
	}
	return localPanelURL(rootDomain)
}
