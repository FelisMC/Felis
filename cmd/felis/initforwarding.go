package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"felis.lolicon.best/internal/software"
	"github.com/BurntSushi/toml"
	"sigs.k8s.io/yaml"
)

// forwardingSecretEnv is the env var the operator injects the Velocity
// modern-forwarding secret under (mirrors internal/operator.envForwardingSecret).
const forwardingSecretEnv = "FELIS_FORWARDING_SECRET"

// Keep the forwarding data directory aligned with the operator's world mount.
const defaultForwardingDataDir = "/data"

// The game UID needs owner read/write access to regenerate these configuration files.
const (
	fwdFileMode os.FileMode = 0o644
	fwdDirMode  os.FileMode = 0o755
)

// Merge forwarding settings only; an empty optional secret leaves configuration unchanged.
func cmdInitForwarding(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init-forwarding", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", defaultForwardingDataDir, "server data directory (Paper working dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	secret := os.Getenv(forwardingSecretEnv)
	if os.Getenv(softwareEnv) != "" {
		spec, err := readSoftwareSpec()
		if err != nil {
			fmt.Fprintf(stderr, "felis init-forwarding: %v\n", err)
			return 2
		}
		wrote, err := writeSoftwareForwarding(*dataDir, spec, secret)
		if err != nil {
			fmt.Fprintf(stderr, "felis init-forwarding: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "felis init-forwarding: %s\n", wrote)
		return 0
	}
	if err := writePaperForwarding(*dataDir, secret); err != nil {
		fmt.Fprintf(stderr, "felis init-forwarding: %v\n", err)
		return 1
	}
	if secret == "" {
		fmt.Fprintln(stdout, "felis init-forwarding: no forwarding secret set; leaving config untouched")
	} else {
		fmt.Fprintln(stdout, "felis init-forwarding: wrote Velocity modern-forwarding config")
	}
	return 0
}

// writePaperForwarding writes both config surfaces (or nothing, when secret == "").
func writePaperForwarding(dataDir, secret string) error {
	if secret == "" {
		return nil
	}
	if err := writePaperGlobal(dataDir, secret); err != nil {
		return err
	}
	return forceServerPropertyOffline(dataDir)
}

// writePaperGlobal merges the proxies.velocity block into config/paper-global.yml,
// creating the file and its directory when absent and preserving every other key.
func writePaperGlobal(dataDir, secret string) error {
	dir := filepath.Join(dataDir, "config")
	if err := os.MkdirAll(dir, fwdDirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// MkdirAll honours the process umask; chmod does not, so a directory an older
	// release left at 0777 is brought back to fwdDirMode here.
	if err := os.Chmod(dir, fwdDirMode); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}

	path := filepath.Join(dir, "paper-global.yml")
	root := map[string]any{}
	if existing, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(existing, &root); err != nil {
			return fmt.Errorf("parse existing %s: %w", path, err)
		}
		if root == nil { // an empty or "null" document unmarshals to a nil map
			root = map[string]any{}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	setVelocity(root, secret)
	out, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	return writeFileMode(path, out)
}

// Paper trusts proxy verification; server.properties must disable backend reauthentication.
func setVelocity(root map[string]any, secret string) {
	velocity := childMap(childMap(root, "proxies"), "velocity")
	velocity["enabled"] = true
	velocity["online-mode"] = true
	velocity["secret"] = secret
}

// Replace absent or non-map child values using YAML's map[string]any representation.
func childMap(parent map[string]any, key string) map[string]any {
	if m, ok := parent[key].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	parent[key] = m
	return m
}

func forceServerPropertyOffline(dataDir string) error {
	path := filepath.Join(dataDir, "server.properties")
	var content []byte
	if b, err := os.ReadFile(path); err == nil {
		content = b
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return writeFileMode(path, upsertProperty(content, "online-mode", "false"))
}

// upsertProperty preserves unrelated lines and ignores commented assignments.
func upsertProperty(content []byte, key, value string) []byte {
	want := key + "=" + value
	prefix := key + "="
	lines := strings.Split(string(content), "\n")
	found := false
	for i, ln := range lines {
		if strings.HasPrefix(ln, prefix) {
			lines[i] = want
			found = true
		}
	}
	if found {
		return []byte(strings.Join(lines, "\n"))
	}
	body := string(content)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return []byte(body + want + "\n")
}

// Force the final mode because WriteFile preserves existing permissions and applies umask to new files.
func writeFileMode(path string, data []byte) error {
	if err := os.WriteFile(path, data, fwdFileMode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(path, fwdFileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// Write Paper/Purpur Velocity settings or Forge/NeoForge proxy-compatible forwarding settings.
// Fabric/Quilt read forwarding secrets from the environment; all proxy backends run offline mode.
func writeSoftwareForwarding(dataDir string, s software.Spec, secret string) (string, error) {
	if secret == "" {
		return "no forwarding secret set; leaving config untouched", nil
	}
	if s.Forwarding != software.ForwardingVelocity {
		return "this server takes no forwarding; players join under offline-mode UUIDs", nil
	}
	switch s.Type {
	case software.Paper, software.Purpur:
		if software.AtLeast(s.MinecraftVersion, "1.19") {
			return "wrote Velocity modern-forwarding config to config/paper-global.yml", writePaperGlobal(dataDir, secret)
		}
		return "wrote Velocity modern-forwarding config to paper.yml", writeLegacyPaperYML(dataDir, secret)
	case software.Forge, software.NeoForge:
		return "wrote Velocity modern-forwarding config to config/proxy-compatible-forge.toml", writePCFConfig(dataDir, secret)
	case software.Fabric, software.Quilt:
		return "FabricProxy-Lite reads the forwarding secret from the environment", nil
	}
	return "this server takes no forwarding", nil
}

// writeLegacyPaperYML merges settings.velocity-support into paper.yml, where
// Paper 1.13 to 1.18 keep it.
func writeLegacyPaperYML(dataDir, secret string) error {
	path := filepath.Join(dataDir, "paper.yml")
	root := map[string]any{}
	if existing, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(existing, &root); err != nil {
			return fmt.Errorf("parse existing %s: %w", path, err)
		}
		if root == nil {
			root = map[string]any{}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	vs := childMap(childMap(root, "settings"), "velocity-support")
	vs["enabled"] = true
	vs["online-mode"] = true
	vs["secret"] = secret
	out, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	return writeFileMode(path, out)
}

// writePCFConfig merges the [forwarding] table Proxy Compatible Forge reads.
// Forge's config loader fills in any key the file leaves out, so a fresh file
// carrying only these three is complete.
func writePCFConfig(dataDir, secret string) error {
	dir := filepath.Join(dataDir, "config")
	if err := os.MkdirAll(dir, fwdDirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "proxy-compatible-forge.toml")
	root := map[string]any{}
	if existing, err := os.ReadFile(path); err == nil {
		if _, err := toml.Decode(string(existing), &root); err != nil {
			return fmt.Errorf("parse existing %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	fwd := childMap(root, "forwarding")
	fwd["enabled"] = true
	fwd["mode"] = "MODERN"
	fwd["secret"] = secret
	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(root); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return writeFileMode(path, []byte(buf.String()))
}
