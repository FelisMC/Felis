package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"felis.lolicon.best/internal/api"
)

// cmdMsgs runs cmd and the commands of any batch it returns, and collects the
// messages.
func cmdMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, cmdMsgs(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// settle hands m the messages cmd produces, as the program would, and returns
// them. A huh form draws its fields only once it has handled a message.
func settle(m *mcBindModel, cmd tea.Cmd) []tea.Msg {
	msgs := cmdMsgs(cmd)
	for _, msg := range msgs {
		m.Update(msg)
	}
	return msgs
}

// openBind is the bind screen as the wizard first shows it.
func openBind(store ownerStore, gameAddr string) *mcBindModel {
	m := newMCBindModel(context.Background(), store, "console.example.com", "root", gameAddr)
	m.setSize(100, 60)
	settle(m, m.Init())
	return m
}

// leavesBindScreen reports whether any of msgs ends the Owner step.
func leavesBindScreen(msgs []tea.Msg) bool {
	for _, msg := range msgs {
		switch msg.(type) {
		case ownerResultMsg, ownerSkippedMsg, tea.QuitMsg:
			return true
		}
	}
	return false
}

func TestMCBindSaysWhereToJoinAndWhatTheCodeIs(t *testing.T) {
	view := openBind(&fakeOwnerStore{}, "10.0.0.5").View()
	for _, want := range []string{"join  10.0.0.5", "8 characters", "10 minutes", "/link", "Leave it empty"} {
		if !strings.Contains(view, want) {
			t.Errorf("bind screen does not say %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "IP address while") {
		t.Errorf("an IP address needs no IP fallback:\n%s", view)
	}

	named := newMCBindModel(context.Background(), &fakeOwnerStore{}, "console.example.com", "root", "play.example.net:25570")
	if got := named.instructions(); !strings.Contains(got, "join  play.example.net:25570\n   (or this host's IP address") {
		t.Errorf("a hostname should come with the IP fallback:\n%s", got)
	}
	unnamed := newMCBindModel(context.Background(), &fakeOwnerStore{}, "console.example.com", "root", "")
	if got := unnamed.instructions(); !strings.Contains(got, "join  this server\n") {
		t.Errorf("no address should still say where to join:\n%s", got)
	}
}

func TestValidLinkCode(t *testing.T) {
	for _, tc := range []struct {
		in, wantErr string
	}{
		{"", ""}, // skip
		{"   ", ""},
		{"K7M2QX9P", ""},
		{"k7m2qx9p", ""},
		{" K7M2-QX9P ", ""},
		{"K7M2 QX9P", ""},
		{"ABCD12", "a link code is 8 characters; this is 6"},
		{"K7M2QX9PZ", "a link code is 8 characters; this is 9"},
		{"K7M2QX9O", `never contains 'O'`},
		{"K7M2QX90", `never contains '0'`},
		{"K7M2QX9É", `never contains 'É'`},
	} {
		err := validLinkCode(tc.in)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("validLinkCode(%q) = %v, want nil", tc.in, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("validLinkCode(%q) = %v, want an error with %q", tc.in, err, tc.wantErr)
		}
	}
}

// A refused code is the operator's most likely mistake; it must leave them on the
// form with the reason, never end setup.
func TestMCBindRefusedCodeStaysOnTheForm(t *testing.T) {
	store := &fakeOwnerStore{redeemErr: api.ErrLinkCodeInvalid}
	m := openBind(store, "10.0.0.5")
	m.linkCode = "k7m2-qx9p"
	_, cmd := m.onFormComplete()
	if m.step != mcBindWorking {
		t.Fatalf("step = %v after submit, want mcBindWorking", m.step)
	}
	var result tea.Msg
	for _, msg := range cmdMsgs(cmd) {
		if r, ok := msg.(mcBindMsg); ok {
			result = r
		}
	}
	if result == nil {
		t.Fatal("submitting a code did not try to bind it")
	}
	next, cmd := m.Update(result)
	if next != m || m.step != mcBindForm {
		t.Fatalf("after a refused code: model %T step %v, want the bind form", next, m.step)
	}
	if leavesBindScreen(settle(m, cmd)) {
		t.Fatal("a refused code ended the Owner step")
	}
	view := m.View()
	for _, want := range []string{"That code was not accepted", "older than 10 minutes", "Type /link", "join  10.0.0.5"} {
		if !strings.Contains(view, want) {
			t.Errorf("refused-code form does not say %q:\n%s", want, view)
		}
	}
	if m.linkCode != "" {
		t.Errorf("the refused code %q is still in the field", m.linkCode)
	}

	// The next code goes through, and the refusal leaves with it.
	store.redeemErr = nil
	m.linkCode = "K7M2QX9P"
	_, cmd = m.onFormComplete()
	for _, msg := range cmdMsgs(cmd) {
		if r, ok := msg.(mcBindMsg); ok {
			m.Update(r)
		}
	}
	if m.step != mcBindDone {
		t.Fatalf("step = %v after a good code, want mcBindDone", m.step)
	}
	if got := store.redeems[len(store.redeems)-1].code; got != "K7M2QX9P" {
		t.Errorf("bound code %q, want K7M2QX9P", got)
	}
}

func TestMCBindOtherFailureStaysOnTheForm(t *testing.T) {
	m := openBind(&fakeOwnerStore{}, "10.0.0.5")
	_, cmd := m.Update(mcBindMsg{err: fmt.Errorf("complete owner setup: %w", errors.New("connection refused"))})
	if m.step != mcBindForm || leavesBindScreen(settle(m, cmd)) {
		t.Fatalf("a failed bind left the form: step %v", m.step)
	}
	view := m.View()
	for _, want := range []string{"Binding failed: complete owner setup: connection refused", "Nothing was changed"} {
		if !strings.Contains(view, want) {
			t.Errorf("failed-bind form does not say %q:\n%s", want, view)
		}
	}
}

// An empty code skips only once the operator confirms; "Enter a code" goes back.
func TestMCBindEmptyCodeSkipsOnlyWhenConfirmed(t *testing.T) {
	m := newMCBindModel(context.Background(), &fakeOwnerStore{}, "console.example.com", "root", "10.0.0.5")
	m.linkCode, m.skip = "  ", false
	_, cmd := m.onFormComplete()
	if m.step != mcBindForm || leavesBindScreen(cmdMsgs(cmd)) {
		t.Fatalf("declining the skip left the form: step %v", m.step)
	}

	m.linkCode, m.skip = "", true
	_, cmd = m.onFormComplete()
	skipped := false
	for _, msg := range cmdMsgs(cmd) {
		if _, ok := msg.(ownerSkippedMsg); ok {
			skipped = true
		}
	}
	if !skipped {
		t.Fatal("a confirmed skip did not leave the Owner step")
	}
}

func TestRootGoesOnWhenTheOwnerIsSkipped(t *testing.T) {
	m := newTestRoot(false, consoleModeSetup, "")
	m.gameAddr = "10.0.0.5"
	m = drive(t, m, preflightDoneMsg{})
	bind, ok := m.screen.(*mcBindModel)
	if !ok || bind.gameAddr != "10.0.0.5" {
		t.Fatalf("bind screen = %T without the game address", m.screen)
	}

	m = drive(t, m, ownerSkippedMsg{})
	if m.stage != stageConnect {
		t.Fatalf("after skipping, stage = %v, want stageConnect", m.stage)
	}
	if _, ok := m.screen.(*connectChooserModel); !ok {
		t.Fatalf("after skipping, screen = %T, want *connectChooserModel", m.screen)
	}
	if !m.result.ownerSkipped || m.result.provisioned {
		t.Fatalf("skip not recorded: %+v", m.result)
	}
	if got := m.reviewBody(int(stageOwner)); !strings.Contains(got, "Owner account skipped") {
		t.Errorf("review of the Owner step = %q", got)
	}

	m = drive(t, m, connectResultMsg{method: connectLocal})
	m = drive(t, m, storageResultMsg{method: storageLocal, detail: "local disk"})
	sum, ok := m.screen.(*summaryModel)
	if !ok {
		t.Fatalf("screen = %T, want *summaryModel", m.screen)
	}
	view := sum.View()
	for _, want := range []string{"Setup finished without an Owner", "not bound", "run  sudo felis setup  again and join 10.0.0.5"} {
		if !strings.Contains(view, want) {
			t.Errorf("summary does not say %q:\n%s", want, view)
		}
	}
	for _, unwanted := range []string{"Setup complete", "You won't need this console again"} {
		if strings.Contains(view, unwanted) {
			t.Errorf("summary without an Owner says %q:\n%s", unwanted, view)
		}
	}
}

func TestReportSetupResultWithoutAnOwner(t *testing.T) {
	const hint = "No Owner is bound yet, so nobody can sign in to the panel."
	const bindLast = hint + " To bind one, run\n  sudo felis setup\nand join 10.0.0.5 in Minecraft when it asks.\n"
	report := func(res breakGlassResult, adminExisted bool) string {
		var b bytes.Buffer
		reportSetupResult(&b, res, false, adminExisted, "https://10.0.0.5:30443", "10.0.0.5")
		return b.String()
	}

	out := report(breakGlassResult{ownerSkipped: true, connectMethod: connectLocal}, false)
	if !strings.Contains(out, "finished without an Owner") || strings.Contains(out, "cancelled") {
		t.Errorf("a skipped Owner reads as:\n%s", out)
	}
	if !strings.HasSuffix(out, bindLast) {
		t.Errorf("a skipped Owner does not end on how to bind one:\n%s", out)
	}

	out = report(breakGlassResult{}, false)
	if !strings.Contains(out, "cancelled — no changes made.") || !strings.HasSuffix(out, bindLast) {
		t.Errorf("quitting before an Owner is bound reads as:\n%s", out)
	}

	out = report(breakGlassResult{ownerSkipped: true, connectMethod: connectReverseProxy, connectConfigured: true, reverseProxyGuide: "proxy guide"}, false)
	if !strings.Contains(out, "proxy guide") || !strings.HasSuffix(out, bindLast) {
		t.Errorf("a skipped Owner with a configured front reads as:\n%s", out)
	}

	for name, res := range map[string]breakGlassResult{
		"re-run":      {alreadySetUp: true},
		"provisioned": {provisioned: true, username: "mc-uuid-1"},
	} {
		adminExisted := name == "re-run"
		if out := report(res, adminExisted); strings.Contains(out, hint) {
			t.Errorf("%s: says no Owner is bound:\n%s", name, out)
		}
	}
}

func TestSetupGameAddress(t *testing.T) {
	for _, tc := range []struct {
		root string
		port int
		want string
	}{
		{"10.211.55.6.nip.io", 0, "10.211.55.6"},
		{"10.211.55.6.nip.io", 25565, "10.211.55.6"},
		{"10.211.55.6.sslip.io.", 25570, "10.211.55.6:25570"},
		{"play.example.net", 0, "play.example.net"},
		{"play.example.net", 25570, "play.example.net:25570"},
		{"", 25570, ""},
	} {
		if got := setupGameAddress(tc.root, tc.port); got != tc.want {
			t.Errorf("setupGameAddress(%q, %d) = %q, want %q", tc.root, tc.port, got, tc.want)
		}
	}
	for addr, want := range map[string]bool{
		"10.0.0.5": true, "10.0.0.5:25570": true, "play.example.net": false, "play.example.net:25570": false,
	} {
		if got := gameAddrIsIP(addr); got != want {
			t.Errorf("gameAddrIsIP(%q) = %v, want %v", addr, got, want)
		}
	}
}

// press sends key to m and runs a few rounds of the commands that follow, as the
// program would, so huh can move between fields and groups.
func press(m *mcBindModel, key tea.KeyMsg) {
	_, cmd := m.Update(key)
	for round := 0; round < 4 && cmd != nil; round++ {
		var next []tea.Cmd
		for _, msg := range cmdMsgs(cmd) {
			if _, c := m.Update(msg); c != nil {
				next = append(next, c)
			}
		}
		cmd = tea.Batch(next...)
	}
}

// The skip question comes only for an empty code: a typed code goes straight to
// the bind.
func TestMCBindFormAsksBeforeSkipping(t *testing.T) {
	m := openBind(&fakeOwnerStore{}, "10.0.0.5")
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if view := m.View(); m.step != mcBindForm || !strings.Contains(view, "Skip the Owner for now?") {
		t.Fatalf("enter on an empty code should ask before skipping: step %v\n%s", m.step, view)
	}

	m = openBind(&fakeOwnerStore{}, "10.0.0.5")
	press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("K7M2QX9P")})
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.step == mcBindForm {
		t.Fatalf("a typed code did not go to the bind:\n%s", m.View())
	}
}
