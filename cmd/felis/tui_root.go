package main

import (
	"context"
	"strings"

	"felis.lolicon.best/internal/cfsetup"
	"felis.lolicon.best/internal/platform"

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

// arrowNavigable is implemented by screens that don't need ←/→ for their own
// input (selects, summaries), so the root may repurpose those keys to walk back
// through completed steps. Text-input screens omit it and keep the arrows for
// cursor movement — that's the "don't fight the input fields" rule.
type arrowNavigable interface {
	arrowNavOK() bool
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
	username      string
	setupTokenURL string
	mode          string
	accountable   string
	auditWarning  string
	isOperator    bool // true when an Operator was added rather than the Owner provisioned
	err           error
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

// reconfigureStorageMsg is sent from the summary/status screen to re-enter the
// storage chooser — the supported way to fix a mistyped S3 detail or switch
// backends after install, without hand-editing felis.toml and the Secret.
type reconfigureStorageMsg struct{}

// ---- rootModel: top-level session ----

type wizardStage int

const (
	stagePreflight wizardStage = iota
	stageOwner
	stageConnect
	stageStorage
	stageSummary
	// stageMenu is the break-glass operation menu. It is appended last so the
	// setup-flow rail indices (Preflight…Done) are unshifted; the rail is suppressed
	// in break-glass mode, so this stage never reaches it.
	stageMenu
)

// setupRailSteps is the one progress rail shared by the whole first-run flow,
// spanning both bubbletea programs: the host-bootstrap installer is rail cell 0,
// and the post-install wizard owns cells 1–4. Defining it once keeps the two
// programs' breadcrumbs identical so the rail reads as a single continuous bar
// rather than restarting when the wizard takes over.
var setupRailSteps = []string{"Bootstrap", "Preflight", "Owner", "Connection", "Storage", "Done"}

type rootModel struct {
	ctx context.Context

	screen tea.Model
	stage  wizardStage

	// reviewing is the index of a completed step the operator is looking back at
	// (read-only), or -1 when the live screen is in front. Driven by ←/→.
	reviewing int

	// reconfiguringConnect is set while re-entering the connection chooser from the
	// summary's "change connection" (or the re-run status screen). In that flow the
	// storage backend is already configured, so completing the connection returns
	// straight to the summary instead of forcing the operator back through storage.
	reconfiguringConnect bool

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
	namespace   string // minecraft workload namespace (cfg.K8s.Namespace); target of the halt op
	adminExists bool
}

func newRootModel(ctx context.Context, store ownerStore, dbURL, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser string, adminExists bool, mode consoleMode) *rootModel {
	rm := &rootModel{
		ctx:         ctx,
		reviewing:   -1,
		dbURL:       dbURL,
		store:       store,
		osUser:      osUser,
		rootDomain:  rootDomain,
		adminHost:   adminHostname,
		panelHost:   panelHostname,
		accessAud:   accessAud,
		namespace:   namespace,
		adminExists: adminExists,
		mode:        mode,
		result: breakGlassResult{
			osUser:        osUser,
			rootDomain:    rootDomain,
			adminHostname: adminHostname,
		},
	}
	if mode == consoleModeBreakGlass {
		if adminExists {
			// A staff account exists, so account operations are peers: open on the menu
			// (provision/reset Owner, or add Operator).
			rm.stage = stageMenu
			rm.screen = newMenuModel()
		} else {
			// Fresh machine: bootstrapping the first Owner is the only sensible op, so skip
			// the menu and go straight to it (offering "add Operator" here would mint a
			// staff account the login gate still rejects).
			rm.stage = stageOwner
			rm.screen = newOwnerModel(ctx, store, osUser, adminExists)
		}
	} else {
		rm.stage = stagePreflight
		rm.screen = newPreflightModel(dbURL, rootDomain, adminHostname)
	}
	return rm
}

func (m *rootModel) Init() tea.Cmd {
	return m.screen.Init()
}

func (m *rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Rail navigation claims ←/→ before anything else sees them. While reviewing
	// it owns every key so nothing leaks into the live screen underneath;
	// otherwise it only takes ← (to enter review) and lets the rest fall through.
	if key, ok := msg.(tea.KeyMsg); ok {
		if handled, model, cmd := m.handleRailKey(key); handled {
			return model, cmd
		}
	}

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
		if m.mode == consoleModeSetup {
			return m.adopt(newMCBindModel(m.ctx, m.store, defaultAdminHostname(m.rootDomain, m.adminHost), m.osUser))
		}
		return m.adopt(newOwnerModel(m.ctx, m.store, m.osUser, false))

	case menuChoiceMsg:
		// The break-glass menu picked an account operation; build its screen. Both reuse
		// stageOwner (the rail is suppressed in break-glass, so the stage is only a label).
		m.stage = stageOwner
		switch msg.op {
		case bgAddOperator:
			return m.adopt(newOperatorModel(m.ctx, m.store, m.osUser))
		case bgHaltServer:
			return m.adopt(newHaltModel(m.ctx, m.store, m.namespace, m.osUser))
		case bgSyncBackup:
			// The picker lists workload servers (m.namespace); the felis-api-internal
			// Service + service token the peer dials live in the control namespace.
			return m.adopt(newBackupModel(m.ctx, m.namespace, platform.DefaultControlNamespace, m.osUser))
		default:
			return m.adopt(newOwnerModel(m.ctx, m.store, m.osUser, m.adminExists))
		}

	case haltResultMsg:
		// Halt is terminal in break-glass: record the durable summary (so cmdBreakGlass
		// can re-print it past the alt-screen teardown) and quit. A load/perform failure
		// routes through the root's error path; an empty fleet or cancel leaves halted
		// false, so the summary reports "no changes made".
		if msg.err != nil {
			m.err = msg.err
			return m, tea.Quit
		}
		if msg.done {
			m.result.halted = true
			m.result.haltServer = msg.outcome.name
			m.result.haltNamespace = msg.outcome.namespace
			m.result.haltAlreadyStopped = msg.outcome.alreadyStopped
			m.result.haltSystemServer = msg.outcome.system
			if msg.outcome.auditErr != nil {
				m.result.haltAuditWarning = msg.outcome.auditErr.Error()
			}
		}
		return m, tea.Quit

	case backupResultMsg:
		// Sync backup is terminal in break-glass, mirroring halt: record the durable
		// summary and quit. A resolve/HTTP failure routes through the error path; an
		// empty fleet or cancel leaves backedUp false.
		if msg.err != nil {
			m.err = msg.err
			return m, tea.Quit
		}
		if msg.done {
			m.result.backedUp = true
			m.result.backupServer = msg.outcome.name
			m.result.backupStatus = msg.outcome.status
		}
		return m, tea.Quit

	case ownerResultMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, tea.Quit
		}
		m.result.provisioned = true
		m.result.isOperator = msg.isOperator
		m.result.username = msg.username
		m.result.setupTokenURL = msg.setupTokenURL
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
		if m.reconfiguringConnect {
			// Changing only the connection — storage is already set, so skip it.
			m.reconfiguringConnect = false
			return m.showSummary()
		}
		m.stage = stageStorage
		return m.adopt(newStorageChooserModel(m.rootDomain, storageLocal, s3Inputs{}))

	case storageResultMsg:
		m.result.storageMethod = msg.method
		m.result.storageDetail = msg.detail
		return m.showSummary()

	case storageBackMsg:
		m.stage = stageStorage
		return m.adopt(newStorageChooserModel(m.rootDomain, storageLocal, s3Inputs{}))

	case reconfigureStorageMsg:
		// Fixing/switching storage after install: re-enter the chooser pre-selected on
		// the current backend, with the non-secret S3 fields pre-filled.
		method, prefill := currentStorageInputs()
		m.stage = stageStorage
		return m.adopt(newStorageChooserModel(m.rootDomain, method, prefill))

	case goBackMsg:
		m.stage = stageConnect
		return m.adopt(newConnectChooserModel(m.rootDomain, m.adminHost, m.panelHost))

	case reconfigureConnectMsg:
		m.reconfiguringConnect = true
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
	m.reviewing = -1 // every stage change drops back to the live screen
	m.pushSize()
	return m, s.Init()
}

// handleRailKey implements ←/→ navigation of the step rail. While reviewing a
// completed step it owns every key (so nothing leaks into the live screen);
// otherwise it claims only ← to enter review, and only when the active screen
// doesn't need the arrows for its own text input.
func (m *rootModel) handleRailKey(k tea.KeyMsg) (bool, tea.Model, tea.Cmd) {
	if m.reviewing >= 0 {
		switch k.String() {
		case "ctrl+c":
			return true, m, tea.Quit
		case "left":
			if m.reviewing > 0 {
				m.reviewing--
			}
			return true, m, nil
		case "right":
			m.reviewing++
			if m.reviewing >= int(m.stage) {
				m.reviewing = -1 // caught up to the live step
			}
			return true, m, nil
		case "esc", "enter":
			m.reviewing = -1
			return true, m, nil
		default:
			return true, m, nil // swallow everything else while reviewing
		}
	}
	if k.String() == "left" && m.canEnterReview() {
		m.reviewing = int(m.stage) - 1
		return true, m, nil
	}
	return false, m, nil
}

// canEnterReview reports whether the live screen will yield ←/→ to the rail.
func (m *rootModel) canEnterReview() bool {
	if m.mode != consoleModeSetup || m.adminExistsAtStart() || m.stage == 0 {
		return false
	}
	n, ok := m.screen.(arrowNavigable)
	return ok && n.arrowNavOK()
}

// displayStage is the rail position currently shown — the reviewed step when
// looking back, otherwise the live stage.
func (m *rootModel) displayStage() int {
	if m.reviewing >= 0 {
		return m.reviewing
	}
	return int(m.stage)
}

// reviewBody renders a read-only recap of an already-completed step. Steps in
// this wizard commit as you finish them (the Owner account and its one-time
// password are created on submit), so review is deliberately look-only — there
// is no re-editing a step you've passed.
func (m *rootModel) reviewBody(stage int) string {
	var b strings.Builder
	switch wizardStage(stage) {
	case stagePreflight:
		b.WriteString(tuiOK.Render("✓ Preflight") + "\n")
		b.WriteString(tuiHint.Render("Control plane verified before configuration."))
	case stageOwner:
		b.WriteString(tuiOK.Render("✓ Owner account") + "\n")
		if m.result.username != "" {
			b.WriteString(tuiLabel.Render("username  ") + m.result.username + "\n")
		}
		b.WriteString(tuiHint.Render("Created and recorded. The one-time setup URL was shown on the Owner step."))
	case stageConnect:
		b.WriteString(tuiOK.Render("✓ Connection") + "\n")
		b.WriteString(tuiLabel.Render("method    ") + connectMethodLabel(m.result.connectMethod) + "\n")
		if m.result.panelURL != "" {
			b.WriteString(tuiLabel.Render("panel     ") + m.result.panelURL)
		}
	case stageStorage:
		b.WriteString(tuiOK.Render("✓ Storage") + "\n")
		b.WriteString(tuiLabel.Render("backend   ") + storageMethodLabel(m.result.storageMethod) + "\n")
		if m.result.storageDetail != "" {
			b.WriteString(tuiHint.Render(m.result.storageDetail))
		}
	}
	b.WriteString("\n\n" + tuiHint.Render("read-only · ") + tuiLabel.Render("←/→") +
		tuiHint.Render(" walk steps · ") + tuiLabel.Render("esc") + tuiHint.Render(" back"))
	return b.String()
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
	return lipgloss.Height(m.railWithHint()) + 1
}

func (m *rootModel) View() string {
	if m.screen == nil {
		return ""
	}
	if m.mode == consoleModeSetup && !m.adminExistsAtStart() {
		body := m.screen.View()
		if m.reviewing >= 0 {
			body = m.reviewBody(m.reviewing)
		}
		return m.railWithHint() + "\n\n" + body
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
	// Bootstrap is rail cell 0 and is always done by the time the wizard runs (an
	// open DB is the proof), so the wizard's own stages render starting at cell 1.
	return tuiStepRail(setupRailSteps, m.displayStage()+1)
}

// railWithHint appends a discoverability hint when ←/→ can walk the rail — while
// reviewing, or on a live screen that yields the arrows.
func (m *rootModel) railWithHint() string {
	r := m.rail()
	switch {
	case m.reviewing >= 0:
		// Mid-review both directions move; → eventually returns to the live step.
		r += tuiRailSep.Render("    ") + tuiRailTodo.Render("←/→ review steps")
	case m.canEnterReview():
		// At the live frontier only ← does anything — there's nothing ahead, so
		// don't advertise → and have it silently no-op.
		r += tuiRailSep.Render("    ") + tuiRailTodo.Render("← review steps")
	}
	return r
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
	m.result.panelURL = panelURLFor(msg.method, msg.panelHostname, m.rootDomain, m.adminHost)
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
		setupTokenURL: m.result.setupTokenURL,
		accessLabel:   connectMethodLabel(m.result.connectMethod),
		storageLabel:  m.result.storageDetail,
		routedHosts:   routed,
		localHint:     m.result.connectMethod == connectLocal,
	})
}

// showStatus is the re-run landing: prove the backend is up, then point the
// operator at the panel without forcing any reconfiguration.
func (m *rootModel) showStatus() (tea.Model, tea.Cmd) {
	m.stage = stageSummary
	m.result.alreadySetUp = true
	method := connectLocal
	accessLabel := "configured (manage in panel)"
	if m.accessAud != "" {
		method = connectCloudflare
		accessLabel = connectMethodLabel(connectCloudflare)
	}
	m.result.panelURL = panelURLFor(method, m.panelHost, m.rootDomain, m.adminHost)
	return m.adopt(&summaryModel{
		panelURL:     m.result.panelURL,
		accessLabel:  accessLabel,
		alreadySetUp: true,
		localHint:    m.accessAud == "" && rootDomainEmbeddedIP(m.rootDomain) != "",
	})
}

func panelURLFor(method connectMethod, panelHostname, rootDomain, adminHostname string) string {
	if method != connectLocal && panelHostname != "" {
		return "https://" + panelHostname
	}
	return localPanelURL(rootDomain, adminHostname)
}
