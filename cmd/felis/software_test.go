package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"felis.lolicon.best/internal/software"
	"github.com/BurntSushi/toml"
	"sigs.k8s.io/yaml"
)

// java takes its options before -jar or the main class and the program's
// arguments after: the heap and the owner's flags first, then the platform's
// properties, then the target, then nogui and the spec's args.
func TestLaunchArgv(t *testing.T) {
	jar := launchArgv("/opt/java/openjdk/bin/java", software.Launch{
		JVMArgs: []string{"-Dlog4j2.formatMsgNoLookups=true"}, Jar: ".felis/server/paper.jar", GameArgs: []string{"nogui"},
	}, "1536M", "-XX:+UseG1GC  -XX:MaxGCPauseMillis=200", []string{"--port", "25565"})
	want := []string{"/opt/java/openjdk/bin/java", "-Xmx1536M", "-XX:+UseG1GC", "-XX:MaxGCPauseMillis=200",
		"-Dlog4j2.formatMsgNoLookups=true", "-jar", ".felis/server/paper.jar", "nogui", "--port", "25565"}
	if !reflect.DeepEqual(jar, want) {
		t.Errorf("jar launch = %q\nwant %q", jar, want)
	}

	knot := launchArgv("java", software.Launch{
		JVMArgs:   []string{"-Dfabric.gameJarPath=.felis/server/minecraft_server.1.21.4.jar"},
		Classpath: []string{"libraries/a.jar", "libraries/b.jar"},
		MainClass: "net.fabricmc.loader.impl.launch.knot.KnotServer", GameArgs: []string{"nogui"},
	}, "", "", nil)
	want = []string{"java", "-Dfabric.gameJarPath=.felis/server/minecraft_server.1.21.4.jar",
		"-cp", "libraries/a.jar:libraries/b.jar", "net.fabricmc.loader.impl.launch.knot.KnotServer", "nogui"}
	if !reflect.DeepEqual(knot, want) {
		t.Errorf("knot launch = %q\nwant %q", knot, want)
	}

	forge := launchArgv("java", software.Launch{
		Args: []string{"@user_jvm_args.txt", "@libraries/net/minecraftforge/forge/1.20.1-47.4.10/unix_args.txt"}, GameArgs: []string{"nogui"},
	}, "2G", "", nil)
	want = []string{"java", "-Xmx2G", "@user_jvm_args.txt", "@libraries/net/minecraftforge/forge/1.20.1-47.4.10/unix_args.txt", "nogui"}
	if !reflect.DeepEqual(forge, want) {
		t.Errorf("forge launch = %q\nwant %q", forge, want)
	}
}

func readProps(t *testing.T, dir string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			out[k] = v
		}
	}
	return out
}

// Every start: the EULA the owner accepted, the game port the proxy dials,
// offline mode behind the proxy, and RCON for the operator. A new server starts
// with the game's own whitelist off (vanilla 26.3 turns it on by default and
// then refuses every player); an owner who turns it on keeps it on.
func TestWriteServerSettings(t *testing.T) {
	dir := t.TempDir()
	if err := writeServerSettings(dir, "25575", "s3cret"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "eula.txt")); string(b) != "eula=true\n" {
		t.Errorf("eula.txt = %q", b)
	}
	got := readProps(t, dir)
	for k, v := range map[string]string{"server-port": "25565", "online-mode": "false", "white-list": "false",
		"enable-rcon": "true", "rcon.port": "25575", "rcon.password": "s3cret"} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	props := filepath.Join(dir, "server.properties")
	b, _ := os.ReadFile(props)
	edited := strings.NewReplacer("white-list=false", "white-list=true", "online-mode=false", "online-mode=true",
		"server-port=25565", "server-port=30000").Replace(string(b)) + "motd=Owner's MOTD\n"
	_ = os.WriteFile(props, []byte(edited), 0o644)
	if err := writeServerSettings(dir, "", ""); err != nil {
		t.Fatal(err)
	}
	got = readProps(t, dir)
	for k, v := range map[string]string{"white-list": "true", "motd": "Owner's MOTD", "online-mode": "false",
		"server-port": "25565", "enable-rcon": "false"} {
		if got[k] != v {
			t.Errorf("after the owner's edits: %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestLaunchEnvHandsFabricProxyLiteTheSecret(t *testing.T) {
	t.Setenv(forwardingSecretEnv, "fwd")
	base := []string{"PATH=/usr/bin"}
	fabric := launchEnv(base, software.Spec{Type: software.Fabric, Forwarding: software.ForwardingVelocity})
	if !slices.Contains(fabric, "FABRIC_PROXY_SECRET=fwd") {
		t.Errorf("fabric env = %v, want FABRIC_PROXY_SECRET", fabric)
	}
	for _, s := range []software.Spec{
		{Type: software.Paper, Forwarding: software.ForwardingVelocity},
		{Type: software.Fabric, Forwarding: software.ForwardingNone},
	} {
		if env := launchEnv(base, s); !reflect.DeepEqual(env, base) {
			t.Errorf("%s/%s env = %v, want it unchanged", s.Type, s.Forwarding, env)
		}
	}
}

// The forwarding config each software reads, by the keys its own docs name:
// Paper's proxies.velocity (paper-global.yml, 1.19 on) and
// settings.velocity-support (paper.yml, before), and Proxy Compatible Forge's
// [forwarding] table (enabled, mode, secret).
func TestWriteSoftwareForwarding(t *testing.T) {
	read := func(t *testing.T, path string) map[string]any {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]any{}
		if strings.HasSuffix(path, ".toml") {
			if _, err := toml.Decode(string(b), &m); err != nil {
				t.Fatal(err)
			}
		} else if err := yaml.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	dig := func(m map[string]any, keys ...string) any {
		var cur any = m
		for _, k := range keys {
			mm, _ := cur.(map[string]any)
			cur = mm[k]
		}
		return cur
	}
	velocity := func(s software.Spec) software.Spec { s.Forwarding = software.ForwardingVelocity; return s }

	dir := t.TempDir()
	if _, err := writeSoftwareForwarding(dir, velocity(software.Spec{Type: software.Paper, MinecraftVersion: "1.16.5"}), "fwd"); err != nil {
		t.Fatal(err)
	}
	if m := read(t, filepath.Join(dir, "paper.yml")); dig(m, "settings", "velocity-support", "enabled") != true ||
		dig(m, "settings", "velocity-support", "online-mode") != true || dig(m, "settings", "velocity-support", "secret") != "fwd" {
		t.Errorf("paper.yml = %v", m)
	}

	dir = t.TempDir()
	if _, err := writeSoftwareForwarding(dir, velocity(software.Spec{Type: software.Purpur, MinecraftVersion: "1.21.4"}), "fwd"); err != nil {
		t.Fatal(err)
	}
	if m := read(t, filepath.Join(dir, "config", "paper-global.yml")); dig(m, "proxies", "velocity", "enabled") != true ||
		dig(m, "proxies", "velocity", "secret") != "fwd" {
		t.Errorf("paper-global.yml = %v", m)
	}

	dir = t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "config"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "config", "proxy-compatible-forge.toml"),
		[]byte("[forwarding]\nenabled = false\nsecret = \"old\"\napprovedProxyHosts = []\n\n[crossStitch]\nenabled = false\n"), 0o644)
	if _, err := writeSoftwareForwarding(dir, velocity(software.Spec{Type: software.NeoForge, MinecraftVersion: "1.21.4"}), "fwd"); err != nil {
		t.Fatal(err)
	}
	m := read(t, filepath.Join(dir, "config", "proxy-compatible-forge.toml"))
	if dig(m, "forwarding", "enabled") != true || dig(m, "forwarding", "mode") != "MODERN" || dig(m, "forwarding", "secret") != "fwd" {
		t.Errorf("proxy-compatible-forge.toml forwarding = %v", m["forwarding"])
	}
	if dig(m, "crossStitch", "enabled") != false {
		t.Errorf("the owner's crossStitch setting was lost: %v", m)
	}

	for _, s := range []software.Spec{
		{Type: software.Vanilla, MinecraftVersion: "1.21.4", Forwarding: software.ForwardingNone},
		velocity(software.Spec{Type: software.Fabric, MinecraftVersion: "1.21.4"}),
	} {
		dir := t.TempDir()
		if _, err := writeSoftwareForwarding(dir, s, "fwd"); err != nil {
			t.Fatal(err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s wrote %v; it takes its forwarding from nowhere on disk", s.Type, entries)
		}
	}
	dir = t.TempDir()
	if _, err := writeSoftwareForwarding(dir, velocity(software.Spec{Type: software.Paper, MinecraftVersion: "1.21.4"}), ""); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("no secret, yet %v was written", entries)
	}
}
