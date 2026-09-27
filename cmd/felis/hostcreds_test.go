package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/mail"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/watchdog"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestHostCredentialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "smtp-password")

	if _, ok, err := readHostCredential(path); ok || err != nil {
		t.Fatalf("a missing file read as ok=%v err=%v; want not there", ok, err)
	}
	// A copy an operator put there by hand, readable by everyone, is tightened.
	if err := os.WriteFile(path, []byte("by hand"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"first secret", "a \"quoted\" $second\nsecret", ""} {
		if err := writeHostCredential(path, v); err != nil {
			t.Fatal(err)
		}
		got, ok, err := readHostCredential(path)
		if err != nil || !ok || got != v {
			t.Fatalf("read back (%q, %v, %v); want (%q, true, nil)", got, ok, err, v)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Fatalf("mode %v; want 0600", mode)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the directory holds %d entries; want the credential alone, no leftover temporary file", len(entries))
	}

	if _, _, err := readHostCredential(dir); err == nil {
		t.Fatal("an unreadable credential read as fine")
	}
}

func smtpSecretClient(t *testing.T, password string) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(haltScheme(t)).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "felis", Name: platform.SMTPSecretName},
		Data:       map[string][]byte{platform.SMTPSecretPasswordKey: []byte(password)},
	}).Build()
}

func TestRelayPassword(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	host := filepath.Join(dir, "smtp-password")
	cl := smtpSecretClient(t, "from-secret")

	if pw, err := relayPassword(ctx, host, cl, "felis"); err != nil || pw != "from-secret" {
		t.Fatalf("without a host copy = (%q, %v); want the Secret's", pw, err)
	}
	if _, err := relayPassword(ctx, host, nil, "felis"); !errors.Is(err, errClusterUnreachable) {
		t.Fatalf("without a host copy or a cluster err = %v; want errClusterUnreachable", err)
	}
	if err := writeHostCredential(host, "from-host"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []client.Client{cl, nil} {
		if pw, err := relayPassword(ctx, host, c, "felis"); err != nil || pw != "from-host" {
			t.Fatalf("with a host copy (cluster %v) = (%q, %v); want the host copy", c != nil, pw, err)
		}
	}
	if _, err := relayPassword(ctx, dir, cl, "felis"); err == nil {
		t.Fatal("an unreadable host copy fell through to the Secret")
	}
}

// The watchdog mails the most while the cluster is down: the host copy must
// reach it then, and without one the password the last good run cached stays.
func TestRefreshSMTPPassword(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	host := filepath.Join(dir, "smtp-password")
	var stderr bytes.Buffer

	state := &watchdog.State{SMTPPassword: "cached"}
	refreshSMTPPassword(ctx, host, nil, "felis", state, &stderr)
	if state.SMTPPassword != "cached" || stderr.Len() != 0 {
		t.Fatalf("cluster down, no host copy: password %q, stderr %q; want the cached one kept quietly", state.SMTPPassword, stderr.String())
	}
	refreshSMTPPassword(ctx, host, smtpSecretClient(t, "from-secret"), "felis", state, &stderr)
	if state.SMTPPassword != "from-secret" {
		t.Fatalf("cluster up, no host copy: password %q; want the Secret's", state.SMTPPassword)
	}
	if err := writeHostCredential(host, "from-host"); err != nil {
		t.Fatal(err)
	}
	refreshSMTPPassword(ctx, host, nil, "felis", state, &stderr)
	if state.SMTPPassword != "from-host" {
		t.Fatalf("cluster down, host copy: password %q; want the host copy", state.SMTPPassword)
	}
	refreshSMTPPassword(ctx, dir, nil, "felis", state, &stderr)
	if state.SMTPPassword != "from-host" || !strings.Contains(stderr.String(), "keeping the cached one") {
		t.Fatalf("unreadable host copy: password %q, stderr %q; want the cached one kept and the failure said", state.SMTPPassword, stderr.String())
	}
}

func TestHostRecoveryMailerHostCopy(t *testing.T) {
	ctx := context.Background()
	// No kubeconfig anywhere: reaching for the cluster fails, so a pass proves
	// the host copy was enough.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-kubeconfig"))
	dir := t.TempDir()
	host := filepath.Join(dir, "smtp-password")
	if err := writeHostCredential(host, "from-host"); err != nil {
		t.Fatal(err)
	}
	off := false
	c := config.SMTPConfig{Host: "mail.example.com", Port: 2525, From: "felis@example.com", Username: "felis", PasswordRef: "FELIS_TEST_UNSET_RELAY_PW", RequireTLS: &off}

	got, err := hostRecoveryMailer(c, host, "felis")(ctx)
	if err != nil {
		t.Fatalf("with the host copy: %v", err)
	}
	if relay, ok := got.(*mail.SMTP); !ok || relay.Password != "from-host" {
		t.Fatalf("relay = %#v; want the host copy's password", got)
	}
	if _, err := hostRecoveryMailer(c, dir, "felis")(ctx); err == nil || !strings.Contains(err.Error(), "read the relay password") {
		t.Fatalf("unreadable host copy: err = %v; want it named", err)
	}
	if _, err := hostRecoveryMailer(c, filepath.Join(dir, "none"), "felis")(ctx); err == nil || !strings.Contains(err.Error(), "reach the cluster") {
		t.Fatalf("no host copy and no cluster: err = %v; want the cluster named", err)
	}
}

// A whole watchdog run with the API server and PostgreSQL both down still
// takes the relay password from the host copy, so the outage mail can
// authenticate even when no earlier run cached it.
func TestWatchdogReadsHostCopyWhileClusterDown(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KUBECONFIG", filepath.Join(dir, "no-kubeconfig"))
	cfgPath := filepath.Join(dir, "felis.toml")
	if err := os.WriteFile(cfgPath, []byte(`[database]
url = "postgres://felis:pw@127.0.0.1:1/felis?sslmode=disable&connect_timeout=2"
[server]
root_domain = "example.com"
[archive]
store = "tarLocal"
[k8s]
egress_mode = "nodeport"
[smtp]
host = "127.0.0.1"
port = 1
from = "felis@example.com"
username = "felis"
password_ref = "FELIS_TEST_UNSET_RELAY_PW"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	pwPath := filepath.Join(dir, "smtp-password")
	if err := writeHostCredential(pwPath, "from-host"); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	var stdout, stderr bytes.Buffer
	cmdWatchdog([]string{
		"-config", cfgPath, "-state", statePath, "-quiet-file", filepath.Join(dir, "quiet"),
		"-backup-dir", "", "-disk-paths", dir, "-smtp-password-file", pwPath, "-heartbeat-file", filepath.Join(dir, "no-heartbeat"),
	}, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "kube-api") {
		t.Fatalf("the run found the API server up; the test needs it down (stdout %s)", stdout.String())
	}
	state, err := watchdog.LoadState(statePath)
	if err != nil {
		t.Fatalf("load state: %v (stderr %s)", err, stderr.String())
	}
	if state.SMTPPassword != "from-host" {
		t.Fatalf("cached relay password %q; want the host copy (stdout %s, stderr %s)", state.SMTPPassword, stdout.String(), stderr.String())
	}
}
