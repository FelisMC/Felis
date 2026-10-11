package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"felis.lolicon.best/internal/heartbeatsettings"
	"felis.lolicon.best/internal/mailsettings"
)

// TestPanelSettingsSaysWhatThePanelHolds pins felis panel-settings: yes or no for the relay
// and the heartbeat, never a value, and exit 1 when the database cannot be read, so the
// installer warns as it would with nothing saved.
func TestPanelSettingsSaysWhatThePanelHolds(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "felis.host.toml")
	writeTestFile(t, cfg, testWatchdogConfig, 0o600)
	defer func() { readPanelSettings = panelSettings }()
	for _, tc := range []struct {
		relay *mailsettings.Settings
		beat  *heartbeatsettings.Settings
		want  string
	}{
		{nil, nil, "relay=no\nheartbeat=no\n"},
		{&mailsettings.Settings{Host: "smtp.panel.example", Port: 587, Password: "secret-pw"}, nil, "relay=yes\nheartbeat=no\n"},
		{nil, &heartbeatsettings.Settings{URL: "https://hc.example/ping/secret-key"}, "relay=no\nheartbeat=yes\n"},
	} {
		readPanelSettings = func(context.Context, string) (*mailsettings.Settings, *heartbeatsettings.Settings, error) {
			return tc.relay, tc.beat, nil
		}
		var out, errOut bytes.Buffer
		if code := cmdPanelSettings([]string{"-config", cfg}, &out, &errOut); code != 0 || out.String() != tc.want {
			t.Errorf("exit %d, %q (%s), want %q", code, out.String(), errOut.String(), tc.want)
		}
		if strings.Contains(out.String(), "secret") || strings.Contains(out.String(), "smtp.panel") {
			t.Errorf("printed a value: %q", out.String())
		}
	}
	readPanelSettings = func(context.Context, string) (*mailsettings.Settings, *heartbeatsettings.Settings, error) {
		return nil, nil, errors.New("connection refused")
	}
	var out, errOut bytes.Buffer
	if code := cmdPanelSettings([]string{"-config", cfg}, &out, &errOut); code != 1 || out.Len() != 0 {
		t.Errorf("an unreadable database: exit %d, %q", code, out.String())
	}
}
