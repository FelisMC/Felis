package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// forwardingSecretEnv is the env var the operator injects the Velocity
// modern-forwarding secret under (mirrors internal/operator.envForwardingSecret).
const forwardingSecretEnv = "FELIS_FORWARDING_SECRET"

// defaultForwardingDataDir is the world PVC mount inside a server pod (mirrors
// internal/operator.dataMountPath). It is Paper's working directory, so its
// config/ and server.properties live under it.
const defaultForwardingDataDir = "/data"

// fwd*Mode make the written config readable AND rewritable by the main server
// container, whose UID we do not control (an arbitrary user image). The
// initContainer runs as root (see buildStatefulSet) so it can write into a data
// volume of unknown ownership; 0666/0777 then let a non-root Paper rewrite the
// same files on boot.
//
// ponytail: relies on the initContainer running as root to write into a volume of
// unknown ownership; that is how the operator schedules it. If that ever changes,
// give the server pod an fsGroup so the shared volume is group-writable instead.
const (
	fwdFileMode os.FileMode = 0o666
	fwdDirMode  os.FileMode = 0o777
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
	// MkdirAll honours the process umask (root's is typically 022 → 0755); chmod
	// does not, and a non-root main container must be able to place/replace the
	// file in this directory on boot.
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
// umask (root's is typically 022 → 0644) but a non-root main container must be
// able to rewrite these files on boot.
func writeFileMode(path string, data []byte) error {
	if err := os.WriteFile(path, data, fwdFileMode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(path, fwdFileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}
