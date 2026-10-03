package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSetupOwnerWaitsForPanelBeforeMinting(t *testing.T) {
	store := &fakeOwnerStore{}
	m := newSetupOwnerModel(context.Background(), store, "https://op.console.example.com:30443", "root")
	m.probe = func() error { return errors.New("panel not ready") }
	batch := m.Init()().(tea.BatchMsg)
	_, cmd := m.Update(batch[1]())
	if cmd != nil || len(store.tokens) != 0 || !strings.Contains(m.View(), "panel not ready") {
		t.Fatal("unready panel issued login")
	}
	m.probe = func() error { return nil }
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	batch = cmd().(tea.BatchMsg)
	_, cmd = m.Update(batch[1]())
	res := cmd().(ownerResultMsg)
	if res.setupTokenURL == "" || res.username != "owner" || len(store.tokens) != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestReportSetupWithoutOwnerPointsAtBrowserSetup(t *testing.T) {
	var b bytes.Buffer
	reportSetupResult(&b, breakGlassResult{}, false, false, "https://op.console.example.com")
	if !strings.Contains(b.String(), "sudo felis setup") || strings.Contains(b.String(), "join ") {
		t.Fatalf("output = %s", b.String())
	}
}

func TestLocalPanelSetupUsesPasskeyHostname(t *testing.T) {
	t.Setenv("FELIS_PANEL_NODEPORT", "30445")
	if got := localPanelURL("10.211.55.6.nip.io", ""); got != "https://op.console.10.211.55.6.nip.io:30445" {
		t.Fatalf("local setup URL = %q; a bare IP cannot enroll the panel passkey", got)
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
}
