package felis

import (
	"strings"
	"testing"
)

// The lobby's LuckPerms wiring is a three-file contract with no compiler behind it:
// bootstrap.sh resolves a URL and passes it as a build-arg, the Dockerfile requires
// that exact arg name and writes the jar to a fixed path, and entrypoint.sh copies
// from that same path on every boot. A typo in any one of them is invisible until a
// lobby actually starts — and then `set -e` turns the failed cp into a crashloop on
// the always-on hub every authenticated player is transferred to.
//
// All three files ship inside the binary (gameStackAssets, bootstrapScript) for the
// TUI install path that has no source checkout, so reading them back here checks
// what is actually shipped rather than what is merely on disk.
func TestLobbyLuckPermsWiringIsConsistent(t *testing.T) {
	dockerfile := readGameStackFile(t, "deploy/lobby/Dockerfile")
	entrypoint := readGameStackFile(t, "deploy/lobby/entrypoint.sh")
	script := BootstrapScript()

	// The path is the contract. RUNTIME_DIR is /paper in the entrypoint, so the
	// Dockerfile's download target and the boot-time copy source must be the same
	// string; anything else fails at `cp`, not at build.
	const jarPath = "/paper/plugins/LuckPerms.jar"
	if !strings.Contains(dockerfile, "-o "+jarPath) {
		t.Errorf("Dockerfile does not download LuckPerms to %s", jarPath)
	}
	if !strings.Contains(entrypoint, `cp -f "$RUNTIME_DIR/plugins/LuckPerms.jar"`) {
		t.Error("entrypoint.sh does not refresh LuckPerms.jar from the image seed; " +
			"a lobby would keep whatever stale jar the PVC happens to hold")
	}
	if !strings.Contains(entrypoint, `RUNTIME_DIR="/paper"`) {
		t.Error(`RUNTIME_DIR is no longer "/paper", so the copy above no longer ` +
			"resolves to the path the Dockerfile writes")
	}

	// The build-arg name has to agree across the two files that never see each other.
	const arg = "LUCKPERMS_JAR_URL"
	if !strings.Contains(dockerfile, "ARG "+arg) {
		t.Errorf("Dockerfile declares no ARG %s", arg)
	}
	if !strings.Contains(script, "--build-arg "+arg+"=") {
		t.Errorf("bootstrap.sh never passes --build-arg %s", arg)
	}
	if !strings.Contains(script, "LUCKPERMS_JAR_URL=\"$(luckperms_latest_jar)\"") {
		t.Error("bootstrap.sh does not resolve the LuckPerms URL before building")
	}

	// bukkit-legacy targets Minecraft 1.8-1.12 and the other platforms are not
	// loadable by Paper at all, so the resolver's grep must pin the /bukkit/ path
	// segment — the metadata endpoint returns every platform's URL in one payload.
	if !strings.Contains(script, "/bukkit/loader/") {
		t.Error("luckperms_latest_jar does not pin the bukkit/loader path; it could " +
			"return the fabric, forge or velocity jar, none of which Paper can load")
	}

	// A missing jar must stop the build. The alternative — shipping a lobby that
	// starts fine and answers every `lp` command from the panel's permission screen
	// with "Unknown command" — is discovered in production.
	if !strings.Contains(dockerfile, `if [ -z "${LUCKPERMS_JAR_URL:-}" ]`) {
		t.Error("Dockerfile does not fail the build when LUCKPERMS_JAR_URL is unset")
	}
}

func readGameStackFile(t *testing.T, name string) string {
	t.Helper()
	b, err := gameStackAssets.ReadFile(name)
	if err != nil {
		t.Fatalf("read embedded %s: %v", name, err)
	}
	return string(b)
}
