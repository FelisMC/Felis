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
