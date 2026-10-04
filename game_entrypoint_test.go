package felis

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The lobby and login gate take their player cap from server.properties, which the
// login gate rewrites on every boot; the lobby seeds it only when absent. These run
// the shipped entrypoints the way a pod does (image and volume paths pointed into temp
// dirs, java replaced by a stub that exits) and read the file the server would start on.

// runEntrypoint runs the embedded entrypoint with its runtime dir holding the given
// files and server.properties seeded with props ("" for a first boot), and returns
// server.properties afterwards.
func runEntrypoint(t *testing.T, name, runtimeVar string, files []string, props string, env ...string) string {
	t.Helper()
	root := t.TempDir()
	runtime, data, bin := filepath.Join(root, "image"), filepath.Join(root, "data"), filepath.Join(root, "bin")
	for _, f := range files {
		p := filepath.Join(runtime, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("jar"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{data, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if props != "" {
		if err := os.WriteFile(filepath.Join(data, "server.properties"), []byte(props), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "java"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	script := readGameStackFile(t, name)
	for old, repl := range map[string]string{
		runtimeVar:         `RUNTIME_DIR="` + runtime + `"`,
		`DATA_DIR="/data"`: `DATA_DIR="` + data + `"`,
	} {
		if !strings.Contains(script, old) {
			t.Fatalf("%s no longer sets %s", name, old)
		}
		script = strings.Replace(script, old, repl, 1)
	}
	path := filepath.Join(root, "entrypoint.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", path)
	cmd.Env = append([]string{"PATH=" + bin + ":" + os.Getenv("PATH"), "FELIS_FORWARDING_SECRET=fwd-test"}, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s failed: %v\n%s", name, err, out)
	}
	got, err := os.ReadFile(filepath.Join(data, "server.properties"))
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// propLines lists the key's lines, so a key written twice shows up as two.
func propLines(props, key string) []string {
	var out []string
	for _, line := range strings.Split(props, "\n") {
		if strings.HasPrefix(line, key+"=") {
			out = append(out, line)
		}
	}
	return out
}

func assertProp(t *testing.T, props, key, want string) {
	t.Helper()
	got := propLines(props, key)
	if len(got) != 1 || got[0] != key+"="+want {
		t.Errorf("%s: got %q, want exactly [%s=%s]\nserver.properties:\n%s", key, got, key, want, props)
	}
}

var lobbyImage = []string{"paper.jar", "plugins/felis-paper.jar", "plugins/LuckPerms.jar"}

func TestLobbyEntrypointSeedsAndPreservesThePlayerCap(t *testing.T) {
	t.Run("preserves an administrator capacity", func(t *testing.T) {
		props := runEntrypoint(t, "deploy/lobby/entrypoint.sh", `RUNTIME_DIR="/paper"`, lobbyImage,
			"#Minecraft server properties\nmax-players=20\nmotd=Kept as it was\n")
		assertProp(t, props, "max-players", "20")
		assertProp(t, props, "motd", "Kept as it was")
	})
	t.Run("on a first boot", func(t *testing.T) {
		props := runEntrypoint(t, "deploy/lobby/entrypoint.sh", `RUNTIME_DIR="/paper"`, lobbyImage, "")
		assertProp(t, props, "max-players", "200")
		assertProp(t, props, "online-mode", "false")
	})
}

// The RCON password is arbitrary bytes from a Secret; each of sed's special characters
// has to land in the file as itself when an earlier boot's line is replaced.
func TestLobbyEntrypointWritesTheRconPasswordVerbatim(t *testing.T) {
	const password = `a|b\c&d/e`
	props := runEntrypoint(t, "deploy/lobby/entrypoint.sh", `RUNTIME_DIR="/paper"`, lobbyImage,
		"enable-rcon=false\nrcon.password=stale\n", "RCON_PASSWORD="+password)
	assertProp(t, props, "enable-rcon", "true")
	assertProp(t, props, "rcon.password", password)
}

var limboImage = []string{"Limbo.jar", "plugins/felis-limbo.jar"}

func TestLimboEntrypointNeverCapsTheGate(t *testing.T) {
	t.Run("over a cap left on the volume", func(t *testing.T) {
		props := runEntrypoint(t, "deploy/limbo/entrypoint.sh", `RUNTIME_DIR="/limbo"`, limboImage,
			"max-players=10\nlevel-name=world;spawn.schem\n")
		assertProp(t, props, "max-players", "-1")
		assertProp(t, props, "level-name", "world;spawn.schem")
		assertProp(t, props, "velocity-modern", "true")
	})
	t.Run("on a first boot", func(t *testing.T) {
		props := runEntrypoint(t, "deploy/limbo/entrypoint.sh", `RUNTIME_DIR="/limbo"`, limboImage, "")
		assertProp(t, props, "max-players", "-1")
		assertProp(t, props, "forwarding-secrets", "fwd-test")
	})
}
