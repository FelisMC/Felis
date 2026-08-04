package felis

import (
	"io/fs"
	"regexp"
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

// The Paper jar digest rides the same cross-file contract as LuckPerms above: bootstrap.sh
// resolves "url sha256" out of Fill's content-addressed download URL and passes the digest
// as a build-arg the Dockerfile must require and verify. docker only WARNS about an unknown
// --build-arg, so a renamed arg would surface as a required-arg failure on a real host
// mid-install — this test is the only compile step the pairing gets.
//
// Both images pull the same jar from the same URL, so both have to check it: a gate on one
// of them leaves the other booting on whatever bytes happened to arrive.
func TestPaperJarDigestWiringIsConsistent(t *testing.T) {
	const arg = "PAPER_JAR_SHA256"
	if n := strings.Count(BootstrapScript(), "--build-arg "+arg+"="); n < 2 {
		t.Errorf("bootstrap.sh passes --build-arg %s %d time(s); the lobby and the "+
			"plain-Paper build each need it", arg, n)
	}
	for _, name := range []string{"deploy/lobby/Dockerfile", "deploy/paper/Dockerfile"} {
		dockerfile := readGameStackFile(t, name)
		if !strings.Contains(dockerfile, "ARG "+arg) {
			t.Errorf("%s declares no ARG %s", name, arg)
		}
		if !strings.Contains(dockerfile, `if [ -z "${PAPER_JAR_SHA256:-}" ]`) {
			t.Errorf("%s does not fail the build when %s is unset", name, arg)
		}
		// Requiring the arg is not the same as spending it, and which file gets hashed
		// matters as much as the command: a `sha256sum -c` over some other download
		// would satisfy a bare substring check while paper.jar still arrives unchecked.
		if !strings.Contains(dockerfile, `echo "$PAPER_JAR_SHA256  /paper/paper.jar" | sha256sum -c`) {
			t.Errorf("%s never verifies /paper/paper.jar against %s", name, arg)
		}
	}
}

// A 1.8 client joining a protocol-47 backend dies on the first chunk unless ViaVersion's
// serverside block-connection tracking is off: under modern forwarding the Velocity injector
// reports 1.13 as the lowest supported protocol, ConnectionData.init() returns early on that,
// and the 1.12.2->1.13 chunk rewrite then dereferences the provider init() never built.
//
// ViaVersion ships the option ON, so this is a correction bootstrap has to make rather than a
// default it can inherit — and nothing else in the install would notice it missing. The failure
// surfaces only when a legacy player joins, on a host that installed cleanly.
func TestBootstrapPinsViaBlockConnectionsOff(t *testing.T) {
	// go:embed takes the working tree verbatim, so the eol attribute is what keeps a Windows
	// checkout from compiling CRs into the installer. Assert it rather than normalizing them
	// away: the only assertion that would otherwise notice is the one spanning a line break
	// below, and it would report a missing pin instead of the line endings.
	script := BootstrapScript()
	if strings.Contains(script, "\r\n") {
		t.Fatal("embedded bootstrap.sh has CRLF line endings; .gitattributes pins *.sh to LF " +
			"and this script is piped into `bash -s` on a Linux host")
	}

	const key = "serverside-blockconnections"
	if !strings.Contains(script, key+": false") {
		t.Errorf("bootstrap.sh never writes %s: false; a fresh install inherits ViaVersion's "+
			"default of true and NPEs the first 1.8 player to receive a chunk", key)
	}

	// Writing the value is only half of it: the file has to be the one ViaVersion reads.
	// Via names its data directory after the plugin in lowercase.
	if !strings.Contains(script, "plugins/viaversion") {
		t.Error("bootstrap.sh does not target plugins/viaversion, so whatever it writes is " +
			"not the config ViaVersion loads")
	}

	// Via staging and this correction have to stay welded together. If the call is dropped,
	// every branch above still exists and still looks right in review.
	if !strings.Contains(script, "pin_via_block_connections\n  ok \"Via staged") {
		t.Error("install_via_plugins no longer calls pin_via_block_connections; the jars would " +
			"be staged with the option left at its default")
	}
}

// The embed list and the images bootstrap.sh builds are two lists nobody reconciles.
// deploy/paper shipped an image build without ever being added to gameStackAssets, and
// nothing said so: a checkout on disk satisfies the build either way, and the tar is
// only the build context on the path that has no checkout — `curl | bash`, where the
// third `docker build -f` then names a file that was never unpacked. So derive the
// inputs from the script and from each Dockerfile's own COPY lines instead of restating
// them here; a fourth image inherits the check for free.
func TestGameStackTarCarriesEveryBuildInput(t *testing.T) {
	// Matches the path only when GAME_STACK_DIR is followed by one, which skips the
	// build-context arguments (`"$GAME_STACK_DIR"`, `"${GAME_STACK_DIR}:/src:z"`) and
	// the glob for gradle's output, none of which are inputs this tar has to carry.
	found := regexp.MustCompile(`\$\{GAME_STACK_DIR\}/(\S+?)"`).FindAllStringSubmatch(BootstrapScript(), -1)
	var paths []string
	seen := map[string]bool{}
	for _, m := range found {
		if !seen[m[1]] {
			seen[m[1]] = true
			paths = append(paths, m[1])
		}
	}
	// Guards the regex itself: a rewrite of how bootstrap.sh spells the build context
	// would otherwise turn this test into an unconditional pass. It has to come before
	// the loop — a missing file in there is fatal, and a floor placed after it would
	// never be reached to say that the regex, not the tar, is what went wrong.
	if len(paths) < 3 {
		t.Fatalf("only %d game-stack path(s) resolved out of bootstrap.sh; the limbo, "+
			"lobby and paper Dockerfiles are all built from ${GAME_STACK_DIR}", len(paths))
	}
	for _, path := range paths {
		requireEmbedded(t, path)
		// A Dockerfile that arrives without the files it COPYs fails just as late and
		// just as far from here; the deploy/paper gap was missing its entrypoint too.
		for _, src := range copySources(t, path) {
			requireEmbedded(t, src)
		}
	}
}

// copySources lists the build-context paths a Dockerfile COPYs in, skipping the
// --from=<stage> copies, whose sources are produced by an earlier stage rather than
// unpacked from the tar.
func copySources(t *testing.T, dockerfile string) []string {
	t.Helper()
	var out []string
	// Continuations are joined first: a COPY split across lines would otherwise be two
	// fragments, neither of them starting with COPY followed by a source, and its
	// source would slip past unchecked.
	body := strings.ReplaceAll(readGameStackFile(t, dockerfile), "\\\n", " ")
	for line := range strings.SplitSeq(body, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "COPY" || strings.HasPrefix(f[1], "--") {
			continue
		}
		out = append(out, strings.TrimSuffix(f[1], "/"))
	}
	return out
}

// fs.Stat rather than ReadFile: half of these are directories (`COPY plugins/shared/`),
// and embed.FS answers for those too.
func requireEmbedded(t *testing.T, path string) {
	t.Helper()
	if _, err := fs.Stat(gameStackAssets, path); err != nil {
		t.Errorf("%s is a game-stack build input but is not in gameStackAssets; an "+
			"install with no source checkout dies on it: %v", path, err)
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
