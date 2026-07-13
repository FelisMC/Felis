package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"felis.lolicon.best/internal/api"

	tea "github.com/charmbracelet/bubbletea"
)

// These tests cover the break-glass operator path that the menu added: the new
// menuModel, the operation discriminator on ownerModel, and the root routing that
// turns a menu choice into the right screen. Like tui_root_test.go they inject the
// inter-screen messages directly and run the screens' outgoing commands by hand, so
// no database, terminal, or huh key-replay is involved — only the wiring is under
// test (the core provisioning logic is covered in breakglass_test.go).

func TestNewOperatorModelDefaults(t *testing.T) {
	m := newOperatorModel(context.Background(), &fakeOwnerStore{}, "root")
	if m.operation != bgAddOperator {
		t.Errorf("operation = %v, want bgAddOperator", m.operation)
	}
	if !m.adminExists {
		t.Error("adminExists = false, want true — adding an Operator presupposes an existing admin")
	}
	if m.step != owAuth {
		t.Errorf("step = %v, want owAuth — the operator path always authenticates first", m.step)
	}
	// The username must NOT default to "owner" the way the Owner flow does: the
	// operator path is insert-only, so a defaulted name would hit ErrConflict on the
	// happy path every single time.
	if m.ownerUser != "" {
		t.Errorf("ownerUser = %q, want empty (no default for the insert-only operator path)", m.ownerUser)
	}
	if m.subject() != "Operator" {
		t.Errorf("subject() = %q, want Operator", m.subject())
	}

	// The Owner flow is the zero value and is unchanged by the discriminator.
	ow := newOwnerModel(context.Background(), &fakeOwnerStore{}, "root", true)
	if ow.operation != bgProvisionOwner {
		t.Errorf("owner operation = %v, want bgProvisionOwner", ow.operation)
	}
	if ow.ownerUser != "owner" || ow.subject() != "Owner" {
		t.Errorf("owner defaults changed: ownerUser=%q subject=%q", ow.ownerUser, ow.subject())
	}
}

func TestProvisionCmdSelectsPathByOperation(t *testing.T) {
	ctx := context.Background()

	t.Run("operator path inserts and leaves local auth untouched", func(t *testing.T) {
		f := &fakeOwnerStore{}
		m := newOperatorModel(ctx, f, "root")
		m.mode, m.accountable, m.username, m.attempt = "recovery", "root", "ops-new", "root"

		out := m.provisionCmd()()
		msg, ok := out.(owProvisionMsg)
		if !ok {
			t.Fatalf("provisionCmd produced %T, want owProvisionMsg", out)
		}
		if msg.err != nil {
			t.Fatalf("operator provision: %v", msg.err)
		}
		// performAddOperator is insert-only and never uses the Owner upsert path.
		if len(f.inserts) != 1 {
			t.Fatalf("want 1 insert (performAddOperator), got %d", len(f.inserts))
		}
		if len(f.upserts) != 0 {
			t.Errorf("want 0 owner upserts, got %d — the operator path must not use UpsertOwner", len(f.upserts))
		}
		// The operator path must NOT flip the global local-auth gate (Owner-only).
		if _, ok := f.settings[api.LocalAuthEnabledKey]; ok {
			t.Error("the operator path flipped local auth — only the Owner thread may")
		}
		// The provision is fully done: the audit row landed (no recoverable audit error).
		if msg.outcome.auditErr != nil {
			t.Errorf("operator provision recorded an audit error: %v", msg.outcome.auditErr)
		}
	})

	t.Run("owner path upserts and enables local auth", func(t *testing.T) {
		f := &fakeOwnerStore{}
		m := newOwnerModel(ctx, f, "root", true)
		m.mode, m.accountable, m.username = "recovery", "root", "owner"

		out := m.provisionCmd()()
		msg, ok := out.(owProvisionMsg)
		if !ok {
			t.Fatalf("provisionCmd produced %T, want owProvisionMsg", out)
		}
		if msg.err != nil {
			t.Fatalf("owner provision: %v", msg.err)
		}
		// performBreakGlass upserts the single Owner and enables local-password login.
		if len(f.upserts) != 1 || len(f.inserts) != 0 {
			t.Fatalf("want 1 upsert and 0 inserts (performBreakGlass), got upserts=%d inserts=%d", len(f.upserts), len(f.inserts))
		}
		if _, ok := f.settings[api.LocalAuthEnabledKey]; !ok {
			t.Error("the owner path did not enable local auth — the thin thread requires it")
		}
	})
}

func TestOwnerModelProvisionErrorRouting(t *testing.T) {
	ctx := context.Background()
	// A wrapped conflict, exactly as provisionOperator surfaces it.
	conflict := fmt.Errorf("operator %q already exists: %w", "owner", api.ErrConflict)

	t.Run("an operator name clash returns to the provision form with a prompt", func(t *testing.T) {
		m := newOperatorModel(ctx, &fakeOwnerStore{}, "root")
		m.mode, m.accountable = "recovery", "root"

		next, _ := m.Update(owProvisionMsg{err: conflict})
		om, ok := next.(*ownerModel)
		if !ok {
			t.Fatalf("Update returned %T, want *ownerModel", next)
		}
		if om.step != owProvision {
			t.Fatalf("step = %v, want owProvision — a taken name is recoverable, not fatal", om.step)
		}
		// provisionErr is what buildProvisionForm keys the "already taken — choose
		// another name" prompt off of, so a still-matchable conflict here is what makes
		// the retry legible to the operator.
		if !errors.Is(om.provisionErr, api.ErrConflict) {
			t.Errorf("provisionErr = %v, want it to wrap api.ErrConflict so the form can prompt for another name", om.provisionErr)
		}
	})

	t.Run("a non-conflict error tears the console down", func(t *testing.T) {
		m := newOperatorModel(ctx, &fakeOwnerStore{}, "root")

		next, cmd := m.Update(owProvisionMsg{err: errors.New("boom")})
		om := next.(*ownerModel)
		if om.step == owProvision {
			t.Error("a generic store fault must not be treated as a retryable name clash")
		}
		if cmd == nil {
			t.Fatal("a fatal provision error should return a failure command")
		}
		res, ok := cmd().(ownerResultMsg)
		if !ok {
			t.Fatalf("failure cmd produced %T, want ownerResultMsg", cmd())
		}
		if res.err == nil {
			t.Error("ownerResultMsg.err = nil, want the surfaced failure")
		}
	})

	t.Run("a conflict on the Owner path is not a retry", func(t *testing.T) {
		// Defensive: the Owner upserts and so never conflicts, but were one ever to
		// surface it must end the session rather than loop the form — only the
		// insert-only operator path is retryable.
		m := newOwnerModel(ctx, &fakeOwnerStore{}, "root", true)

		next, cmd := m.Update(owProvisionMsg{err: conflict})
		om := next.(*ownerModel)
		if om.provisionErr != nil {
			t.Error("the Owner path recorded a retryable conflict; only the operator path retries")
		}
		if res, ok := cmd().(ownerResultMsg); !ok || res.err == nil {
			t.Error("an Owner-path conflict should tear down via an error result")
		}
	})
}

func TestOwnerResultCmdCarriesIsOperator(t *testing.T) {
	ctx := context.Background()

	op := newOperatorModel(ctx, &fakeOwnerStore{}, "root")
	op.username, op.mode, op.accountable = "ops", "recovery", "root"
	if res := op.ownerResultCmd()().(ownerResultMsg); !res.isOperator {
		t.Error("operator result.isOperator = false, want true")
	}

	ow := newOwnerModel(ctx, &fakeOwnerStore{}, "root", true)
	ow.username, ow.mode, ow.accountable = "owner", "recovery", "root"
	if res := ow.ownerResultCmd()().(ownerResultMsg); res.isOperator {
		t.Error("owner result.isOperator = true, want false")
	}
}

func TestMCBindCarriesAuditWarning(t *testing.T) {
	m := newMCBindModel(context.Background(), &fakeOwnerStore{}, "op.console.example.com", "root")
	next, _ := m.Update(mcBindMsg{outcome: breakGlassOutcome{
		ownerIdentity: "mc-uuid-1",
		setupTokenURL: "https://op.console.example.com/setup?token=t0ken",
		auditErr:      errors.New("audit insert failed"),
	}})
	bound := next.(*mcBindModel)
	if !strings.Contains(bound.doneView(), "audit insert failed") {
		t.Fatalf("done view did not surface audit warning:\n%s", bound.doneView())
	}
	res := bound.resultCmd()().(ownerResultMsg)
	if res.username != "mc-uuid-1" {
		t.Fatalf("result username = %q, want verified Minecraft UUID", res.username)
	}
	if res.auditWarning != "audit insert failed" {
		t.Fatalf("result audit warning = %q", res.auditWarning)
	}
}

func TestRootBreakGlassOpensMenuWhenAdminExists(t *testing.T) {
	m := newTestRoot(true, consoleModeBreakGlass, "")
	if m.stage != stageMenu {
		t.Fatalf("stage = %v, want stageMenu", m.stage)
	}
	if _, ok := m.screen.(*menuModel); !ok {
		t.Fatalf("screen = %T, want *menuModel", m.screen)
	}

	// Picking "add Operator" adopts an ownerModel wired for the operator path.
	m = drive(t, m, menuChoiceMsg{op: bgAddOperator})
	if m.stage != stageOwner {
		t.Fatalf("after the menu choice, stage = %v, want stageOwner", m.stage)
	}
	om, ok := m.screen.(*ownerModel)
	if !ok {
		t.Fatalf("screen = %T, want *ownerModel", m.screen)
	}
	if om.operation != bgAddOperator {
		t.Errorf("operation = %v, want bgAddOperator", om.operation)
	}
}

func TestRootBreakGlassMenuOwnerChoice(t *testing.T) {
	m := newTestRoot(true, consoleModeBreakGlass, "")
	m = drive(t, m, menuChoiceMsg{op: bgProvisionOwner})
	om, ok := m.screen.(*ownerModel)
	if !ok {
		t.Fatalf("screen = %T, want *ownerModel", m.screen)
	}
	if om.operation != bgProvisionOwner {
		t.Errorf("operation = %v, want bgProvisionOwner", om.operation)
	}
}

func TestRootBreakGlassSkipsMenuOnFreshMachine(t *testing.T) {
	// No admin yet: bootstrapping the first Owner is the only sensible op, so the menu
	// is skipped and the console opens straight on the Owner screen.
	m := newTestRoot(false, consoleModeBreakGlass, "")
	if m.stage != stageOwner {
		t.Fatalf("stage = %v, want stageOwner (no menu on a fresh machine)", m.stage)
	}
	om, ok := m.screen.(*ownerModel)
	if !ok {
		t.Fatalf("screen = %T, want *ownerModel", m.screen)
	}
	if om.operation != bgProvisionOwner {
		t.Errorf("operation = %v, want bgProvisionOwner", om.operation)
	}
}

func TestMenuModelCancelQuits(t *testing.T) {
	if m := newMenuModel(); m.choice != bgProvisionOwner {
		t.Errorf("default choice = %v, want bgProvisionOwner (Owner reset is the common path)", m.choice)
	}
	// Backing out of the top-level menu cancels the whole console.
	for _, kt := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlC} {
		m := newMenuModel()
		_, cmd := m.Update(key(kt))
		if cmd == nil {
			t.Fatalf("%v on the menu should return a command (tea.Quit)", kt)
		}
		if msg := cmd(); !isQuit(msg) {
			t.Errorf("%v on the menu = %T, want tea.Quit", kt, msg)
		}
	}
}
