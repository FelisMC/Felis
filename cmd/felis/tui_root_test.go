package main

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// These tests exercise the root wizard's state machine directly by injecting the
// inter-screen messages into rootModel.Update. They bypass all real I/O (DB,
// migrations, panel probe, provisioning) — the screens emit those messages via
// commands we don't run — so the only thing under test is the orchestration:
// which screen the root advances to, and what it records on the result. This is
// the part the dashboard→wizard refactor actually put at risk, and it needs no
// root, k3s, or database.

func drive(t *testing.T, m *rootModel, msg tea.Msg) *rootModel {
	t.Helper()
	next, _ := m.Update(msg)
	rm, ok := next.(*rootModel)
	if !ok {
		t.Fatalf("Update returned %T, want *rootModel", next)
	}
	return rm
}

func newTestRoot(adminExists bool, mode consoleMode, accessAud string) *rootModel {
	return newRootModel(
		context.Background(),
		&fakeOwnerStore{},
		"postgres://localhost/felis",
		"felis.example.com",
		"admin.felis.example.com",
		"panel.felis.example.com",
		accessAud,
		"minecraft",
		"root",
		adminExists,
		mode,
	)
}

func TestRootSetupHappyPath(t *testing.T) {
	m := newTestRoot(false, consoleModeSetup, "")

	// First-run setup begins at preflight.
	if m.stage != stagePreflight {
		t.Fatalf("initial stage = %v, want stagePreflight", m.stage)
	}
	if _, ok := m.screen.(*preflightModel); !ok {
		t.Fatalf("initial screen = %T, want *preflightModel", m.screen)
	}

	// Preflight done → MC-bind (setup mode establishes the Owner by binding a
	// Minecraft account, not by typing a username/password). The stage label is
	// still stageOwner; only the screen differs by mode.
	m = drive(t, m, preflightDoneMsg{})
	if m.stage != stageOwner {
		t.Fatalf("after preflight, stage = %v, want stageOwner", m.stage)
	}
	if _, ok := m.screen.(*mcBindModel); !ok {
		t.Fatalf("after preflight, screen = %T, want *mcBindModel", m.screen)
	}

	// Owner provisioned → Connection chooser.
	m = drive(t, m, ownerResultMsg{username: "owner", setupTokenURL: "https://op.console.example.com/setup?token=t0ken"})
	if m.stage != stageConnect {
		t.Fatalf("after owner, stage = %v, want stageConnect", m.stage)
	}
	if _, ok := m.screen.(*connectChooserModel); !ok {
		t.Fatalf("after owner, screen = %T, want *connectChooserModel", m.screen)
	}
	if !m.result.provisioned || m.result.username != "owner" || m.result.setupTokenURL != "https://op.console.example.com/setup?token=t0ken" {
		t.Fatalf("owner result not recorded: %+v", m.result)
	}

	// Reverse-proxy chosen → Storage chooser, with the connection recorded.
	guide := "caddy config…"
	m = drive(t, m, connectResultMsg{
		method:        connectReverseProxy,
		panelHostname: "panel.felis.example.com",
		adminHostname: "admin.felis.example.com",
		guide:         guide,
	})
	if m.stage != stageStorage {
		t.Fatalf("after connect, stage = %v, want stageStorage", m.stage)
	}
	if _, ok := m.screen.(*storageChooserModel); !ok {
		t.Fatalf("after connect, screen = %T, want *storageChooserModel", m.screen)
	}
	if !m.result.connectConfigured {
		t.Fatalf("connectConfigured not set")
	}
	if m.result.connectMethod != connectReverseProxy {
		t.Fatalf("connectMethod = %v, want connectReverseProxy", m.result.connectMethod)
	}
	if m.result.reverseProxyGuide != guide {
		t.Fatalf("reverseProxyGuide = %q, want %q", m.result.reverseProxyGuide, guide)
	}

	// Storage chosen → Summary, with both the connection and storage recorded.
	m = drive(t, m, storageResultMsg{method: storageS3, detail: "s3://bucket  ·  minio:9000"})
	if m.stage != stageSummary {
		t.Fatalf("after storage, stage = %v, want stageSummary", m.stage)
	}
	sum, ok := m.screen.(*summaryModel)
	if !ok {
		t.Fatalf("after storage, screen = %T, want *summaryModel", m.screen)
	}
	if m.result.storageMethod != storageS3 || m.result.storageDetail == "" {
		t.Fatalf("storage result not recorded: %+v", m.result)
	}
	if sum.storageLabel != m.result.storageDetail {
		t.Fatalf("summary storageLabel = %q, want %q", sum.storageLabel, m.result.storageDetail)
	}
	if want := "https://panel.felis.example.com"; sum.panelURL != want {
		t.Fatalf("summary panelURL = %q, want %q", sum.panelURL, want)
	}
	if want := "https://op.console.example.com/setup?token=t0ken"; sum.setupTokenURL != want {
		t.Fatalf("summary setupTokenURL = %q, want %q", sum.setupTokenURL, want)
	}
	if sum.alreadySetUp {
		t.Fatalf("first-run summary should not be marked alreadySetUp")
	}
}

func TestRootSetupLocalSummary(t *testing.T) {
	m := newTestRoot(false, consoleModeSetup, "")
	m = drive(t, m, preflightDoneMsg{})
	m = drive(t, m, ownerResultMsg{username: "owner"})
	m = drive(t, m, connectResultMsg{method: connectLocal, panelHostname: "panel.felis.example.com"})
	m = drive(t, m, storageResultMsg{method: storageLocal, detail: "local disk · /var/lib/felis/uploads"})

	sum, ok := m.screen.(*summaryModel)
	if !ok {
		t.Fatalf("screen = %T, want *summaryModel", m.screen)
	}
	if !sum.localHint {
		t.Fatalf("local connection summary should set localHint")
	}
	// Local never points at the public hostname.
	if sum.panelURL == "https://panel.felis.example.com" {
		t.Fatalf("local summary panelURL should be the local origin, got %q", sum.panelURL)
	}
}

// TestRootReconfigureConnectSkipsStorage locks the flow guard: from the finished
// summary, "change connection" re-enters only the connection chooser and returns
// straight to the summary — storage was already configured, so the operator is not
// dragged back through it, and the earlier storage recap is preserved.
func TestRootReconfigureConnectSkipsStorage(t *testing.T) {
	m := newTestRoot(false, consoleModeSetup, "")
	m = drive(t, m, preflightDoneMsg{})
	m = drive(t, m, ownerResultMsg{username: "owner", setupTokenURL: "https://op.console.example.com/setup?token=t0ken"})
	m = drive(t, m, connectResultMsg{method: connectLocal, panelHostname: "panel.felis.example.com"})
	m = drive(t, m, storageResultMsg{method: storageS3, detail: "s3://bucket"})
	if _, ok := m.screen.(*summaryModel); !ok {
		t.Fatalf("after first run, screen = %T, want *summaryModel", m.screen)
	}

	// "change connection" re-enters the connection chooser.
	m = drive(t, m, reconfigureConnectMsg{})
	if m.stage != stageConnect {
		t.Fatalf("reconfigure stage = %v, want stageConnect", m.stage)
	}
	if _, ok := m.screen.(*connectChooserModel); !ok {
		t.Fatalf("reconfigure screen = %T, want *connectChooserModel", m.screen)
	}

	// Completing it returns straight to the summary — NOT the storage chooser —
	// with the original storage recap intact.
	m = drive(t, m, connectResultMsg{method: connectReverseProxy, panelHostname: "panel.felis.example.com", guide: "caddy…"})
	if m.stage != stageSummary {
		t.Fatalf("after reconfigure connect, stage = %v, want stageSummary", m.stage)
	}
	sum, ok := m.screen.(*summaryModel)
	if !ok {
		t.Fatalf("after reconfigure connect, screen = %T, want *summaryModel", m.screen)
	}
	if sum.storageLabel != "s3://bucket" {
		t.Fatalf("reconfigure summary storageLabel = %q, want preserved %q", sum.storageLabel, "s3://bucket")
	}
	if m.result.connectMethod != connectReverseProxy {
		t.Fatalf("reconfigure did not update connectMethod: %v", m.result.connectMethod)
	}
}

// TestRootReconfigureStorageReEntersChooser locks the post-install "change storage"
// path: from the finished summary it re-enters the storage chooser (not the
// connection one) and returns to the summary carrying the new storage recap.
func TestRootReconfigureStorageReEntersChooser(t *testing.T) {
	m := newTestRoot(false, consoleModeSetup, "")
	m = drive(t, m, preflightDoneMsg{})
	m = drive(t, m, ownerResultMsg{username: "owner"})
	m = drive(t, m, connectResultMsg{method: connectLocal, panelHostname: "panel.felis.example.com"})
	m = drive(t, m, storageResultMsg{method: storageLocal, detail: "local disk · /var/lib/felis/uploads"})
	if _, ok := m.screen.(*summaryModel); !ok {
		t.Fatalf("after first run, screen = %T, want *summaryModel", m.screen)
	}

	// "change storage" re-enters the storage chooser.
	m = drive(t, m, reconfigureStorageMsg{})
	if m.stage != stageStorage {
		t.Fatalf("reconfigure-storage stage = %v, want stageStorage", m.stage)
	}
	if _, ok := m.screen.(*storageChooserModel); !ok {
		t.Fatalf("reconfigure-storage screen = %T, want *storageChooserModel", m.screen)
	}

	// Completing it returns to the summary with the updated storage recap.
	m = drive(t, m, storageResultMsg{method: storageS3, detail: "s3://newbucket"})
	if m.stage != stageSummary {
		t.Fatalf("after reconfigure-storage, stage = %v, want stageSummary", m.stage)
	}
	sum, ok := m.screen.(*summaryModel)
	if !ok {
		t.Fatalf("after reconfigure-storage, screen = %T, want *summaryModel", m.screen)
	}
	if sum.storageLabel != "s3://newbucket" {
		t.Fatalf("summary storageLabel = %q, want updated %q", sum.storageLabel, "s3://newbucket")
	}
}

func TestRootRerunLandsOnStatus(t *testing.T) {
	// adminExists at start of a setup run = re-run: preflight should skip straight
	// to the "manage in panel" status screen, never touching owner/connect.
	m := newTestRoot(true, consoleModeSetup, "")
	if _, ok := m.screen.(*preflightModel); !ok {
		t.Fatalf("re-run initial screen = %T, want *preflightModel", m.screen)
	}

	m = drive(t, m, preflightDoneMsg{})
	sum, ok := m.screen.(*summaryModel)
	if !ok {
		t.Fatalf("re-run after preflight, screen = %T, want *summaryModel", m.screen)
	}
	if !sum.alreadySetUp {
		t.Fatalf("re-run summary should be marked alreadySetUp")
	}
	if m.result.provisioned {
		t.Fatalf("re-run must not provision an owner")
	}
}

func TestRootBreakGlassQuitsAfterOwner(t *testing.T) {
	// Break-glass with a staff account present opens on the operation menu; choosing
	// "provision/reset the Owner" lands on the owner screen, which must quit on
	// completion without entering the connection chooser (that step is setup-only).
	m := newTestRoot(true, consoleModeBreakGlass, "")
	if m.stage != stageMenu {
		t.Fatalf("break-glass initial stage = %v, want stageMenu", m.stage)
	}
	m = drive(t, m, menuChoiceMsg{op: bgProvisionOwner})
	if m.stage != stageOwner {
		t.Fatalf("after the menu choice, stage = %v, want stageOwner", m.stage)
	}
	if _, ok := m.screen.(*ownerModel); !ok {
		t.Fatalf("after the menu choice, screen = %T, want *ownerModel", m.screen)
	}

	next, cmd := m.Update(ownerResultMsg{username: "owner", setupTokenURL: "https://op.console.example.com/setup?token=t0ken", mode: "recovery"})
	rm := next.(*rootModel)
	if _, ok := rm.screen.(*connectChooserModel); ok {
		t.Fatalf("break-glass must not enter the connection chooser")
	}
	if cmd == nil {
		t.Fatalf("break-glass owner completion should return a command (tea.Quit)")
	}
	if msg := cmd(); !isQuit(msg) {
		t.Fatalf("break-glass owner completion command = %T, want tea.Quit", msg)
	}
	if !rm.result.provisioned || rm.result.username != "owner" {
		t.Fatalf("break-glass owner result not recorded: %+v", rm.result)
	}
}

func key(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

// TestRootRailReviewNavigation locks the ←/→ rail-walk added for ergonomics:
// from a yielding screen ← steps back through completed stages read-only,
// → / esc return to the live screen, and ← is ignored on text-input screens
// (which need the arrow for their cursor). A screenshot can't verify this — the
// keys only matter live — so the contract lives here.
func TestRootRailReviewNavigation(t *testing.T) {
	m := newTestRoot(false, consoleModeSetup, "")
	m = drive(t, m, preflightDoneMsg{})

	// On the Owner screen (text inputs) ← must NOT hijack the arrow: it stays
	// with the field, so we remain on the live screen.
	m = drive(t, m, key(tea.KeyLeft))
	if m.reviewing != -1 {
		t.Fatalf("← on the owner (text-input) screen entered review (%d); arrows belong to the field", m.reviewing)
	}

	// Advance to the Connection chooser (a select — it yields ←/→).
	m = drive(t, m, ownerResultMsg{username: "owner", setupTokenURL: "https://op.console.example.com/setup?token=t0ken"})
	if m.reviewing != -1 {
		t.Fatalf("fresh chooser should start live, reviewing = %d", m.reviewing)
	}

	// ← walks back to Owner (read-only recap), then Preflight, then clamps.
	m = drive(t, m, key(tea.KeyLeft))
	if m.reviewing != int(stageOwner) {
		t.Fatalf("first ← = stage %d, want stageOwner %d", m.reviewing, stageOwner)
	}
	if v := m.View(); !strings.Contains(v, "Owner account") || !strings.Contains(v, "username") {
		t.Fatalf("owner review body missing recap, got:\n%s", v)
	}
	m = drive(t, m, key(tea.KeyLeft))
	if m.reviewing != int(stagePreflight) {
		t.Fatalf("second ← = stage %d, want stagePreflight %d", m.reviewing, stagePreflight)
	}
	m = drive(t, m, key(tea.KeyLeft))
	if m.reviewing != int(stagePreflight) {
		t.Fatalf("← past the first step should clamp, got %d", m.reviewing)
	}

	// → walks forward; stepping past the last completed step returns to live.
	m = drive(t, m, key(tea.KeyRight))
	if m.reviewing != int(stageOwner) {
		t.Fatalf("→ = stage %d, want stageOwner %d", m.reviewing, stageOwner)
	}
	m = drive(t, m, key(tea.KeyRight))
	if m.reviewing != -1 {
		t.Fatalf("→ past the last completed step should return live, reviewing = %d", m.reviewing)
	}
	if v := m.View(); !strings.Contains(v, "reach the panel") {
		t.Fatalf("returning live should show the chooser, got:\n%s", v)
	}

	// esc is an immediate escape hatch back to the live screen.
	m = drive(t, m, key(tea.KeyLeft))
	if m.reviewing < 0 {
		t.Fatalf("← should re-enter review")
	}
	m = drive(t, m, key(tea.KeyEsc))
	if m.reviewing != -1 {
		t.Fatalf("esc should return to the live screen, reviewing = %d", m.reviewing)
	}
}

// TestSetupRailSpansBootstrap locks the cross-program progress rail: the
// host-bootstrap screen shows Bootstrap as the live step 1, and once the wizard
// takes over Bootstrap is carried as a completed (✓) step ahead of the live one.
// This is what makes the rail read as one continuous bar across the two separate
// bubbletea programs instead of restarting when the wizard launches.
func TestSetupRailSpansBootstrap(t *testing.T) {
	boot := newHostBootstrapModel(context.Background())
	if v := boot.View(); !strings.Contains(v, "1. Bootstrap") || !strings.Contains(v, "Preflight") {
		t.Fatalf("bootstrap screen should show the shared rail with Bootstrap as step 1, got:\n%s", v)
	}

	m := newTestRoot(false, consoleModeSetup, "")
	m = drive(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	m = drive(t, m, preflightDoneMsg{})
	if _, ok := m.screen.(*mcBindModel); !ok {
		t.Fatalf("expected MC-bind screen after preflight, got %T", m.screen)
	}
	if v := m.View(); !strings.Contains(v, "✓ Bootstrap") {
		t.Fatalf("wizard rail should carry Bootstrap as a completed step, got:\n%s", v)
	}
}

// isQuit reports whether a command's message is tea.Quit's sentinel. tea.Quit
// returns an unexported tea.quitMsg, so compare against the documented value
// produced by calling tea.Quit itself.
func isQuit(msg tea.Msg) bool {
	_, ok := msg.(tea.QuitMsg)
	return ok
}
