package main

import (
	"context"
	"strings"
	"testing"

	"felis.lolicon.best/internal/config"

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
		config.DatabaseConfig{URL: "postgres://localhost/felis"},
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
	m = drive(t, m, preflightDoneMsg{})
	if m.stage != stageConnect {
		t.Fatalf("stage = %v, want Connection", m.stage)
	}
	m = drive(t, m, connectResultMsg{method: connectReverseProxy, panelHostname: "panel.felis.example.com", adminHostname: "new-admin.felis.example.com", guide: "caddy…"})
	if m.stage != stageStorage {
		t.Fatalf("stage = %v, want Storage", m.stage)
	}
	m = drive(t, m, storageResultMsg{method: storageS3, detail: "s3://bucket"})
	owner, ok := m.screen.(*setupOwnerModel)
	if !ok || owner.panelURL != "https://new-admin.felis.example.com" {
		t.Fatalf("owner setup = %#v", m.screen)
	}
	if m.result.provisioned {
		t.Fatal("Owner must not be minted before configuration")
	}
	m = drive(t, m, ownerResultMsg{username: "owner", setupTokenURL: "https://new-admin.felis.example.com/setup?token=t0ken"})
	sum, ok := m.screen.(*summaryModel)
	if !ok || sum.setupTokenURL != m.result.setupTokenURL || sum.panelURL != "https://new-admin.felis.example.com" || sum.storageLabel != "s3://bucket" {
		t.Fatalf("summary = %#v", m.screen)
	}
	if !strings.Contains(sum.View(), "Finish Owner login") || !strings.Contains(sum.View(), "Minecraft can be linked later") {
		t.Fatal(sum.View())
	}
}

func TestRootSetupLocalSummary(t *testing.T) {
	m := newTestRoot(false, consoleModeSetup, "")
	m = drive(t, m, preflightDoneMsg{})
	m = drive(t, m, connectResultMsg{method: connectLocal, panelHostname: "panel.felis.example.com"})
	m = drive(t, m, storageResultMsg{method: storageLocal, detail: "local disk · /var/lib/felis/uploads"})
	m = drive(t, m, ownerResultMsg{username: "owner"})

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
	m = drive(t, m, connectResultMsg{method: connectLocal, panelHostname: "panel.felis.example.com"})
	m = drive(t, m, storageResultMsg{method: storageS3, detail: "s3://bucket"})
	m = drive(t, m, ownerResultMsg{username: "owner", setupTokenURL: "https://op.console.example.com/setup?token=t0ken"})
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
	if _, ok := m.screen.(*setupOwnerModel); !ok {
		t.Fatalf("pending link should refresh, screen = %T", m.screen)
	}
	m = drive(t, m, ownerResultMsg{username: "owner", setupTokenURL: "https://admin.felis.example.com/setup?token=fresh"})
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
	m = drive(t, m, connectResultMsg{method: connectLocal, panelHostname: "panel.felis.example.com"})
	m = drive(t, m, storageResultMsg{method: storageLocal, detail: "local disk · /var/lib/felis/uploads"})
	m = drive(t, m, ownerResultMsg{username: "owner"})
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

// TestRootReconfigureSMTP locks the post-install "configure email" path: from
// the re-run status screen it opens the SMTP form, and finishing it lands back
// on the status screen (not the first-run summary, which would drop the
// alreadySetUp framing).
func TestRootReconfigureSMTP(t *testing.T) {
	m := newTestRoot(true, consoleModeSetup, "")
	m = drive(t, m, preflightDoneMsg{})
	m = drive(t, m, setupReadyMsg{username: "owner"})
	if _, ok := m.screen.(*summaryModel); !ok {
		t.Fatalf("re-run after preflight, screen = %T, want *summaryModel", m.screen)
	}

	m = drive(t, m, reconfigureSMTPMsg{})
	if _, ok := m.screen.(*smtpModel); !ok {
		t.Fatalf("reconfigure-smtp screen = %T, want *smtpModel", m.screen)
	}

	m = drive(t, m, smtpResultMsg{configured: true, detail: "smtp.example.net:587  ·  from felis@example.net"})
	sum, ok := m.screen.(*summaryModel)
	if !ok {
		t.Fatalf("after reconfigure-smtp, screen = %T, want *summaryModel", m.screen)
	}
	if !sum.alreadySetUp {
		t.Fatalf("after reconfigure-smtp, summary should still be the alreadySetUp status screen")
	}
}

// TestRootReconfigureStorageKeepsStatusFraming locks the same rule for the
// "change storage" path: on a re-run, completing it must land back on the
// alreadySetUp status framing (with the updated recap), not "Setup complete."
func TestRootReconfigureStorageKeepsStatusFraming(t *testing.T) {
	m := newTestRoot(true, consoleModeSetup, "")
	m = drive(t, m, preflightDoneMsg{})
	m = drive(t, m, setupReadyMsg{username: "owner"})
	if _, ok := m.screen.(*summaryModel); !ok {
		t.Fatalf("re-run after preflight, screen = %T, want *summaryModel", m.screen)
	}

	m = drive(t, m, reconfigureStorageMsg{})
	if _, ok := m.screen.(*storageChooserModel); !ok {
		t.Fatalf("reconfigure-storage screen = %T, want *storageChooserModel", m.screen)
	}

	m = drive(t, m, storageResultMsg{method: storageLocal, detail: "local disk · /var/lib/felis/uploads"})
	sum, ok := m.screen.(*summaryModel)
	if !ok {
		t.Fatalf("after reconfigure-storage, screen = %T, want *summaryModel", m.screen)
	}
	if !sum.alreadySetUp {
		t.Fatalf("after reconfigure-storage, summary should keep the alreadySetUp framing")
	}
	if sum.storageLabel != "local disk · /var/lib/felis/uploads" {
		t.Fatalf("storageLabel = %q, want the updated recap", sum.storageLabel)
	}
}

func TestRootRerunLandsOnStatus(t *testing.T) {
	m := newTestRoot(true, consoleModeSetup, "")
	m = drive(t, m, preflightDoneMsg{})
	if _, ok := m.screen.(*setupOwnerModel); !ok {
		t.Fatalf("screen = %T", m.screen)
	}
	m = drive(t, m, setupReadyMsg{username: "owner"})
	if m.stage != stageSummary || !m.result.alreadySetUp || m.result.provisioned {
		t.Fatalf("result = %+v", m.result)
	}
}

func TestRootRerunResumesUnfinishedLogin(t *testing.T) {
	m := newTestRoot(true, consoleModeSetup, "")
	m = drive(t, m, preflightDoneMsg{})
	m = drive(t, m, ownerResultMsg{username: "owner", setupTokenURL: "https://admin.felis.example.com:30443/setup?token=new"})
	sum := m.screen.(*summaryModel)
	if sum.setupTokenURL == "" || !sum.alreadySetUp {
		t.Fatalf("summary = %+v", sum)
	}
	if strings.Contains(m.View(), "Bootstrap") {
		t.Fatal("renewing a login link restarted the deployment rail")
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
	m = drive(t, m, key(tea.KeyLeft))
	if m.reviewing != int(stagePreflight) || !strings.Contains(m.View(), "Control plane verified") {
		t.Fatal("connection review should return to preflight")
	}
	m = drive(t, m, key(tea.KeyRight))
	if m.reviewing != -1 {
		t.Fatal("right should return to live connection chooser")
	}
	m = drive(t, m, key(tea.KeyLeft))
	m = drive(t, m, key(tea.KeyEsc))
	if m.reviewing != -1 {
		t.Fatal("escape should return to live screen")
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
	if _, ok := m.screen.(*connectChooserModel); !ok {
		t.Fatalf("expected connection screen after preflight, got %T", m.screen)
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
