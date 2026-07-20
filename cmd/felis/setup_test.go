package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSetupConfigPathPrefersGeneratedHostConfig(t *testing.T) {
	dir := t.TempDir()
	requested := filepath.Join(dir, "felis.toml")
	host := filepath.Join(dir, "felis.host.toml")

	if got := setupConfigPathFor(requested, host, false); got != requested {
		t.Fatalf("without host config: got %q, want requested %q", got, requested)
	}
	if err := os.WriteFile(host, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := setupConfigPathFor(requested, host, false); got != host {
		t.Fatalf("with host config: got %q, want host %q", got, host)
	}
	if got := setupConfigPathFor(requested, host, true); got != requested {
		t.Fatalf("explicit config: got %q, want requested %q", got, requested)
	}
}

func TestEnsureDefaultConfigLinkBacksUpStaleDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links requires an optional Windows privilege")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "felis.toml")
	host := filepath.Join(dir, "felis.host.toml")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(host, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ensureDefaultConfigLink(target, host); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(target)
	if err != nil {
		t.Fatal(err)
	}
	if link != host {
		t.Fatalf("default config link = %q, want %q", link, host)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	foundBackup := false
	for _, e := range entries {
		foundBackup = foundBackup || strings.HasPrefix(e.Name(), "felis.toml.bak.")
	}
	if !foundBackup {
		t.Fatal("stale default config was not backed up")
	}
}

func TestHostBootstrapReadyRequiresMarkerAndArtifacts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows files do not expose Unix executable mode bits")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "bootstrap.done")
	hostConfig := filepath.Join(dir, "felis.host.toml")
	hostBin := filepath.Join(dir, "felis")
	kubeconfig := filepath.Join(dir, "k3s.yaml")

	if err := os.WriteFile(marker, []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hostBootstrapReady(marker, hostConfig, hostBin, kubeconfig) {
		t.Fatal("bootstrap should not be ready with marker only")
	}

	for _, path := range []string{hostConfig, kubeconfig} {
		if err := os.WriteFile(path, []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(hostBin, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !hostBootstrapReady(marker, hostConfig, hostBin, kubeconfig) {
		t.Fatal("bootstrap should be ready when marker and host artifacts exist")
	}
}

// --dev used to export FELIS_CHANNEL, which nothing reads, so `felis setup --dev`
// silently installed the RELEASE channel: the one outcome the operator did not ask
// for. Renaming the variable to the one bootstrap does read (FELIS_VERSION_BOOTSTRAP)
// would not have helped -- setup takes bootstrap's bootstrap_from_tui arm, where every
// reader of it is unreachable -- so the flag refuses instead of guessing. It has to
// refuse BEFORE the root check, or the message an unprivileged operator sees is about
// sudo rather than about the channel.
func TestSetupDevFlagRefusesInsteadOfSilentlyInstallingRelease(t *testing.T) {
	var stdout, stderr strings.Builder

	if code := cmdSetup([]string{"--dev"}, &stdout, &stderr); code != 2 {
		t.Fatalf("want exit 2 for an unsupported channel flag, got %d (stderr: %s)", code, stderr.String())
	}
	msg := stderr.String()
	if !strings.Contains(msg, "FELIS_VERSION_BOOTSTRAP=dev") {
		t.Errorf("the refusal must name the mechanism that actually works:\n%s", msg)
	}
	if strings.Contains(msg, "must run as root") {
		t.Errorf("the channel refusal must precede the root check:\n%s", msg)
	}
	// The dead variable is gone; setting it again would re-create a knob nothing reads.
	if _, ok := os.LookupEnv("FELIS_CHANNEL"); ok {
		t.Errorf("FELIS_CHANNEL has no reader anywhere and must not be exported")
	}
}
