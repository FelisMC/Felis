package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// readYAML parses a paper-global.yml into a nested map for assertions.
func readYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

// velocityBlock digs out proxies.velocity from a parsed paper-global.yml.
func velocityBlock(t *testing.T, root map[string]any) map[string]any {
	t.Helper()
	proxies, ok := root["proxies"].(map[string]any)
	if !ok {
		t.Fatalf("no proxies map: %v", root)
	}
	vel, ok := proxies["velocity"].(map[string]any)
	if !ok {
		t.Fatalf("no proxies.velocity map: %v", proxies)
	}
	return vel
}

// An empty secret must touch nothing: a proxy that is not in modern mode
// provisions no Secret, and the init must not wedge the pod over it.
func TestWritePaperForwardingEmptySecretIsNoop(t *testing.T) {
	dir := t.TempDir()
	if err := writePaperForwarding(dir, ""); err != nil {
		t.Fatalf("writePaperForwarding: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "paper-global.yml")); !os.IsNotExist(err) {
		t.Errorf("paper-global.yml should not exist, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "server.properties")); !os.IsNotExist(err) {
		t.Errorf("server.properties should not exist, stat err = %v", err)
	}
}

// A fresh data dir gets a complete velocity block and an offline server.properties.
func TestWritePaperForwardingFreshDir(t *testing.T) {
	dir := t.TempDir()
	if err := writePaperForwarding(dir, "s3cr3t"); err != nil {
		t.Fatalf("writePaperForwarding: %v", err)
	}

	vel := velocityBlock(t, readYAML(t, filepath.Join(dir, "config", "paper-global.yml")))
	if vel["enabled"] != true {
		t.Errorf("velocity.enabled = %v, want true", vel["enabled"])
	}
	if vel["online-mode"] != true {
		t.Errorf("velocity.online-mode = %v, want true", vel["online-mode"])
	}
	if vel["secret"] != "s3cr3t" {
		t.Errorf("velocity.secret = %v, want s3cr3t", vel["secret"])
	}

	props, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatalf("read server.properties: %v", err)
	}
	if !strings.Contains(string(props), "online-mode=false") {
		t.Errorf("server.properties missing online-mode=false:\n%s", props)
	}
}

// A pre-existing paper-global.yml (as Paper expands it on first boot) must keep
// all of its other keys — clobbering them would silently reset the user's tuning
// on every restart. This is the whole reason the writer merges rather than
// overwrites.
func TestWritePaperGlobalPreservesExistingKeys(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := []byte("proxies:\n  velocity:\n    enabled: false\n    secret: OLD\nchunk-loading:\n  autoconfig-send-distance: true\nmisc:\n  max-joins-per-tick: 5\n")
	if err := os.WriteFile(filepath.Join(cfg, "paper-global.yml"), existing, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writePaperGlobal(dir, "NEW"); err != nil {
		t.Fatalf("writePaperGlobal: %v", err)
	}

	root := readYAML(t, filepath.Join(cfg, "paper-global.yml"))
	vel := velocityBlock(t, root)
	if vel["enabled"] != true || vel["secret"] != "NEW" {
		t.Errorf("velocity not updated: %v", vel)
	}
	// Unrelated trees survive.
	if _, ok := root["chunk-loading"].(map[string]any); !ok {
		t.Errorf("chunk-loading tree lost: %v", root)
	}
	misc, ok := root["misc"].(map[string]any)
	if !ok {
		t.Fatalf("misc tree lost: %v", root)
	}
	// sigs.k8s.io/yaml decodes numbers via JSON, so 5 arrives as float64(5).
	if misc["max-joins-per-tick"] != float64(5) {
		t.Errorf("misc.max-joins-per-tick = %v, want 5", misc["max-joins-per-tick"])
	}
}

// online-mode=false must be forced while every other property line — comments
// included — is left untouched.
func TestForceServerPropertyOfflinePreservesOthers(t *testing.T) {
	dir := t.TempDir()
	existing := "#Minecraft server properties\nmotd=Hello World\nonline-mode=true\ndifficulty=hard\n"
	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := forceServerPropertyOffline(dir); err != nil {
		t.Fatalf("forceServerPropertyOffline: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Contains(s, "online-mode=true") {
		t.Errorf("online-mode=true not replaced:\n%s", s)
	}
	if !strings.Contains(s, "online-mode=false") {
		t.Errorf("online-mode=false not set:\n%s", s)
	}
	for _, keep := range []string{"#Minecraft server properties", "motd=Hello World", "difficulty=hard"} {
		if !strings.Contains(s, keep) {
			t.Errorf("lost line %q:\n%s", keep, s)
		}
	}
}

// upsertProperty appends when the key is absent and does not grow blank lines.
func TestUpsertPropertyAppends(t *testing.T) {
	got := string(upsertProperty([]byte("motd=hi"), "online-mode", "false"))
	if got != "motd=hi\nonline-mode=false\n" {
		t.Errorf("append form wrong: %q", got)
	}
	empty := string(upsertProperty(nil, "online-mode", "false"))
	if empty != "online-mode=false\n" {
		t.Errorf("empty form wrong: %q", empty)
	}
	// A commented key must not count as present.
	commented := string(upsertProperty([]byte("#online-mode=true\n"), "online-mode", "false"))
	if !strings.Contains(commented, "#online-mode=true") || !strings.Contains(commented, "\nonline-mode=false\n") {
		t.Errorf("comment mishandled: %q", commented)
	}
}

// The written files land at fwdFileMode: owner-writable for the game uid the init
// shares with the server container, and no longer world-writable. chmod semantics
// are POSIX-only, so this asserts on non-Windows.
func TestWriteForwardingFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes not represented on Windows")
	}
	dir := t.TempDir()
	if err := writePaperForwarding(dir, "x"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(dir, "config", "paper-global.yml"),
		filepath.Join(dir, "server.properties"),
	} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != fwdFileMode {
			t.Errorf("%s mode = %o, want %o", p, fi.Mode().Perm(), fwdFileMode)
		}
	}
}
