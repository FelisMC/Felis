package main

import (
	"context"
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

	// Preflight done → Owner.
	m = drive(t, m, preflightDoneMsg{})
	if m.stage != stageOwner {
		t.Fatalf("after preflight, stage = %v, want stageOwner", m.stage)
	}
	if _, ok := m.screen.(*ownerModel); !ok {
		t.Fatalf("after preflight, screen = %T, want *ownerModel", m.screen)
	}

	// Owner provisioned → Connection chooser.
	m = drive(t, m, ownerResultMsg{username: "owner", displayPassword: "hunter2"})
	if m.stage != stageConnect {
		t.Fatalf("after owner, stage = %v, want stageConnect", m.stage)
	}
	if _, ok := m.screen.(*connectChooserModel); !ok {
		t.Fatalf("after owner, screen = %T, want *connectChooserModel", m.screen)
	}
	if !m.result.provisioned || m.result.username != "owner" || m.result.displayPassword != "hunter2" {
		t.Fatalf("owner result not recorded: %+v", m.result)
	}

	// Reverse-proxy chosen → Summary, with the connection recorded.
	guide := "caddy config…"
	m = drive(t, m, connectResultMsg{
		method:        connectReverseProxy,
		panelHostname: "panel.felis.example.com",
		adminHostname: "admin.felis.example.com",
		guide:         guide,
	})
	if m.stage != stageSummary {
		t.Fatalf("after connect, stage = %v, want stageSummary", m.stage)
	}
	sum, ok := m.screen.(*summaryModel)
	if !ok {
		t.Fatalf("after connect, screen = %T, want *summaryModel", m.screen)
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
	if want := "https://panel.felis.example.com"; sum.panelURL != want {
		t.Fatalf("summary panelURL = %q, want %q", sum.panelURL, want)
	}
	if sum.ownerPassword != "hunter2" {
		t.Fatalf("summary ownerPassword = %q, want %q", sum.ownerPassword, "hunter2")
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
	// Break-glass starts at owner and must quit on owner completion without
	// entering the connection chooser.
	m := newTestRoot(true, consoleModeBreakGlass, "")
	if m.stage != stageOwner {
		t.Fatalf("break-glass initial stage = %v, want stageOwner", m.stage)
	}
	if _, ok := m.screen.(*ownerModel); !ok {
		t.Fatalf("break-glass initial screen = %T, want *ownerModel", m.screen)
	}

	next, cmd := m.Update(ownerResultMsg{username: "owner", displayPassword: "pw", mode: "recovery"})
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

// isQuit reports whether a command's message is tea.Quit's sentinel. tea.Quit
// returns an unexported tea.quitMsg, so compare against the documented value
// produced by calling tea.Quit itself.
func isQuit(msg tea.Msg) bool {
	_, ok := msg.(tea.QuitMsg)
	return ok
}
