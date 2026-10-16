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

// defaultForwardingDataDir is the world PVC mount inside a server pod (mirrors
// internal/operator.dataMountPath). It is Paper's working directory, so its
// config/ and server.properties live under it.
const defaultForwardingDataDir = "/data"

// fwd*Mode are the modes the written config lands with. The initContainer runs as
// the same uid as the server container (naming.GameUID, pinned by the operator in
// the pod securityContext) after the prepare-data initContainer has handed the
// whole volume to that uid, so owner read/write is all the server needs to rewrite
// these files on boot and nothing else on the node gets write access to them.
const (
	fwdFileMode os.FileMode = 0o644
	fwdDirMode  os.FileMode = 0o755
)

// cmdInitForwarding is the felis-image initContainer entrypoint that makes an
// ARBITRARY Paper image joinable behind a modern-forwarding Velocity proxy,
// WITHOUT modifying that image: it writes the Velocity block into
// <data>/config/paper-global.yml and forces online-mode=false in
// <data>/server.properties before the server container starts. This is the same
// config deploy/lobby/entrypoint.sh writes for the Felis-built lobby, lifted into
// Go so it can be applied to an image Felis did not build.
//
// It is idempotent and MERGE-based: Paper expands paper-global.yml to its full
// default tree on first boot, and the panel file editor may change either file
// between boots, so it only ever sets proxies.velocity.* and the single
// online-mode key and preserves every other setting.
//
// "Preserves" means values, not formatting, and only for the YAML half:
// sigs.k8s.io/yaml round-trips through JSON, so paper-global.yml comes back with
// its keys sorted and its comments dropped. Every setting survives and Paper reads
// it back identically, but a user who annotated that file loses the annotations.
// Accepted rather than fixed: comment-faithful editing means a yaml.v3 Node walk,
// which is a lot of machinery for two keys. server.properties is edited line-wise
// and does keep its comments and ordering.
//
// An empty/unset secret is a deliberate no-op (exit 0): a cluster whose proxy is
// not in modern mode provisions no Secret, and wedging every server's init on a
// missing optional value would be worse than the pre-forwarding status quo.
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

// setVelocity sets proxies.velocity.{enabled,online-mode,secret}, creating the
// intermediate maps when missing. proxies.velocity.online-mode is Paper trusting
// that the proxy verified the player as premium — distinct from server.properties
// online-mode, which must be false so the backend does not re-authenticate.
func setVelocity(root map[string]any, secret string) {
	velocity := childMap(childMap(root, "proxies"), "velocity")
	velocity["enabled"] = true
	velocity["online-mode"] = true
	velocity["secret"] = secret
}

// childMap returns parent[key] as a map, replacing a missing or non-map value with
// a fresh one. sigs.k8s.io/yaml decodes nested objects to map[string]any (JSON
// semantics), so the assertion holds for any well-formed paper-global.yml.
func childMap(parent map[string]any, key string) map[string]any {
	if m, ok := parent[key].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	parent[key] = m
	return m
}

// forceServerPropertyOffline sets online-mode=false in server.properties. A backend
// behind a modern-forwarding proxy must be offline-mode (the proxy did the Mojang
// auth); an arbitrary image defaulting to online-mode=true rejects every proxied
// login.
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

// upsertProperty sets key=value in a java .properties body, replacing an existing
// uncommented assignment or appending one, and leaving every other line —
// comments included — untouched. Keys sit at column 0 the way Paper writes them,
// so a "#key=" comment does not match.
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

// writeFileMode writes data then forces the mode, since WriteFile honours the
// umask and leaves an existing file's mode alone: a file an older release wrote
// world-writable (0666) is tightened back to fwdFileMode on the next boot.
func writeFileMode(path string, data []byte) error {
	if err := os.WriteFile(path, data, fwdFileMode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(path, fwdFileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// writeSoftwareForwarding writes the forwarding config of a runtime-image
// server, by what it runs (software.ForwardingFor decided whether it can):
//
//   - Paper and Purpur: the Velocity block of config/paper-global.yml from 1.19,
//     of paper.yml's settings.velocity-support before;
//   - Forge and NeoForge: the [forwarding] table of Proxy Compatible Forge's
//     config/proxy-compatible-forge.toml;
//   - Fabric and Quilt: nothing on disk, since FabricProxy-Lite takes the secret
//     from the environment `felis launch` sets;
//   - a server without forwarding: nothing.
//
// The launcher forces online-mode=false for every one of them.
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
