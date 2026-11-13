package main

import (
	"reflect"
	"strings"
	"testing"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/mail"
	tea "github.com/charmbracelet/bubbletea"
	"sigs.k8s.io/yaml"
)

func TestSMTPSecretManifestCarriesTargetNamespace(t *testing.T) {
	for _, ns := range []string{"felis", "minecraft"} {
		b, err := smtpSecretManifest("pw", ns)
		if err != nil {
			t.Fatalf("render for %s: %v", ns, err)
		}
		var got struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal for %s: %v", ns, err)
		}
		if got.Metadata.Namespace != ns {
			t.Fatalf("manifest namespace = %q, want %q", got.Metadata.Namespace, ns)
		}
		if !strings.Contains(string(b), "name: felis-smtp") {
			t.Fatalf("manifest must still name felis-smtp: %s", b)
		}
	}
}

func TestSMTPRelayCarriesTLSPosture(t *testing.T) {
	remote := smtpRelay(config.SMTPConfig{Host: "smtp.example.net", Port: 587, From: "felis@example.net", Username: "felis"}, "pw")
	want := &mail.SMTP{Host: "smtp.example.net", Port: 587, From: "felis@example.net", Username: "felis", Password: "pw", RequireTLS: true}
	if !reflect.DeepEqual(remote, want) {
		t.Errorf("remote relay = %+v, want %+v", remote, want)
	}
	if local := smtpRelay(config.SMTPConfig{Host: "127.0.0.1", Port: 25, From: "felis@example.net"}, ""); local.RequireTLS {
		t.Error("a relay on this host must not require TLS by default")
	}
}

func TestMailStepSaysWhatSkippingLeavesOut(t *testing.T) {
	m := newTestRoot(false, consoleModeSetup, "")
	m = drive(t, m, preflightDoneMsg{})
	m = drive(t, m, connectResultMsg{method: connectLocal, panelHostname: "panel.felis.example.com"})
	m = drive(t, m, tea.WindowSizeMsg{Width: 400, Height: 80})
	m.screen.Init() // the form lays its fields out on Init, as the program does on adopting a screen
	v := m.View()
	for _, want := range []string{"Esc skips this step", "no email codes and no alerts", "passkey, which needs a trusted certificate"} {
		if !strings.Contains(v, want) {
			t.Errorf("the Mail step lacks %q:\n%s", want, v)
		}
	}
	configured := newMailStepModel(smtpInputs{host: "smtp.example.net", port: "587", from: "felis@example.net"})
	if !strings.Contains(configured.skipNote, "keeps the relay already configured") || strings.Contains(configured.skipNote, "no email codes") {
		t.Errorf("with a relay already set, skipping keeps it: %q", configured.skipNote)
	}
	if v := newSMTPModel(smtpInputs{}).View(); strings.Contains(v, "Esc skips") {
		t.Errorf("the summary's email form offers to skip a step:\n%s", v)
	}
}

func TestSetupSMTPConfigKeepsHandSetKeys(t *testing.T) {
	off := false
	prev := config.SMTPConfig{Host: "old.example.net", Port: 25, From: "old@example.net", MaxPerHour: 500, RequireTLS: &off}
	in := smtpInputs{host: "smtp.example.net", from: "felis@example.net", username: "felis"}
	got := setupSMTPConfig(in, 465, prev)
	if got.Host != "smtp.example.net" || got.Port != 465 || got.From != "felis@example.net" ||
		got.Username != "felis" || got.PasswordRef != "FELIS_SMTP_PASSWORD" {
		t.Errorf("relay fields = %+v", got)
	}
	if got.MaxPerHour != 500 {
		t.Errorf("max_per_hour = %d, want 500", got.MaxPerHour)
	}
	if got.RequireTLS == nil || *got.RequireTLS {
		t.Errorf("require_tls = %v, want the operator's false", got.RequireTLS)
	}
	if fresh := setupSMTPConfig(in, 587, config.SMTPConfig{}); fresh.RequireTLS != nil || fresh.MaxPerHour != 0 {
		t.Errorf("first setup = %+v, want require_tls and max_per_hour unset", fresh)
	}
}
