package main

import (
	"reflect"
	"strings"
	"testing"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/mail"
	"sigs.k8s.io/yaml"
)

// TestSMTPSecretManifestCarriesTargetNamespace pins the fix for the
// workload-namespace replica: kubectl refuses a manifest whose namespace
// conflicts with -n ("the namespace from the provided object ... does not
// match"), so the mirror must render felis-smtp with the TARGET namespace —
// otherwise the "configure email" refresh fails on the first apply and the
// felis-config mirror never runs at all.
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

// TestSMTPRelayCarriesTLSPosture: the relay felis api, the reaper and the
// watchdog send through refuses plaintext for a remote host and allows it for
// one on this host, as [smtp] says.
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

// TestSetupSMTPConfigKeepsHandSetKeys: re-running the email screen replaces the
// relay but keeps require_tls and max_per_hour, which only an operator sets.
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
