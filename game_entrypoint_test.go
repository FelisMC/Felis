package felis

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Login rewrites its player cap every boot; lobby seeds it only when absent.

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
	java := "#!/bin/sh\nprintf '%s\\n' \"$$\" > \"$FELIS_TEST_JAVA_PID\"\ncat > \"$FELIS_TEST_JAVA_INPUT\"\n"
	if err := os.WriteFile(filepath.Join(bin, "java"), []byte(java), 0o755); err != nil {
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", path)
	const input = "say console input 世界\nlist\n"
	cmd.Stdin = strings.NewReader(input)
	pidPath, inputPath := filepath.Join(root, "java.pid"), filepath.Join(root, "java.stdin")
	cmd.Env = append([]string{"PATH=" + bin + ":" + os.Getenv("PATH"), "FELIS_FORWARDING_SECRET=fwd-test",
		"FELIS_TEST_JAVA_PID=" + pidPath, "FELIS_TEST_JAVA_INPUT=" + inputPath}, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s failed: %v\n%s", name, err, out)
	}
	pid, err := os.ReadFile(pidPath)
	if err != nil || strings.TrimSpace(string(pid)) != strconv.Itoa(cmd.Process.Pid) {
		t.Fatalf("%s did not exec Java in the entrypoint process: pid=%q err=%v", name, pid, err)
	}
	stdin, err := os.ReadFile(inputPath)
	if err != nil || string(stdin) != input {
		t.Fatalf("%s Java stdin = %q, err=%v", name, stdin, err)
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

func TestLobbyEntrypointWritesTheRconPasswordVerbatim(t *testing.T) {
	const password = `a|b\c&d/e`
	props := runEntrypoint(t, "deploy/lobby/entrypoint.sh", `RUNTIME_DIR="/paper"`, lobbyImage,
		"enable-rcon=false\nrcon.password=stale\n", "RCON_PASSWORD="+password)
	assertProp(t, props, "enable-rcon", "true")
	assertProp(t, props, "rcon.password", password)
}

var limboImage = []string{"Limbo.jar", "plugins/felis-limbo.jar"}

func TestGameEntrypointsPassStdinToJava(t *testing.T) {
	for _, tc := range []struct {
		name, runtime string
		files         []string
	}{
		{"paper", `RUNTIME_DIR="/paper"`, []string{"paper.jar"}},
		{"lobby", `RUNTIME_DIR="/paper"`, lobbyImage},
		{"limbo", `RUNTIME_DIR="/limbo"`, limboImage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runEntrypoint(t, "deploy/"+tc.name+"/entrypoint.sh", tc.runtime, tc.files, "")
		})
	}
}

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
