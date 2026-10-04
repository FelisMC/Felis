package felis

import (
	"encoding/xml"
	"io/fs"
	"os"
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
	// The plugin builds invoke this shared init script after COPYing the directory;
	// a directory entry alone would pass the COPY check even if the script was omitted.
	requireEmbedded(t, "plugins/shared/build-progress.gradle")
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

// The lock file is the install's only source of upstream builds, and bootstrap.sh reads it
// with a strict KEY=value parser that dies on anything unexpected, so a malformed lock is a
// failed install on every host. Check the shipped copy the same way here.
func TestGameStackLockIsComplete(t *testing.T) {
	lock := gameStackLock(t)
	m := regexp.MustCompile(`GAME_STACK_LOCK_KEYS="([^"]*)"`).FindStringSubmatch(BootstrapScript())
	if m == nil {
		t.Fatal("bootstrap.sh no longer declares GAME_STACK_LOCK_KEYS")
	}
	keys := strings.Fields(m[1])
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, k := range keys {
		v, ok := lock[k]
		if !ok || v == "" {
			t.Errorf("game-stack.lock does not set %s", k)
			continue
		}
		if strings.HasSuffix(k, "_SHA256") && !sha.MatchString(v) {
			t.Errorf("%s=%q is not a lowercase sha256", k, v)
		}
		// A moving URL pins nothing: the digest check would start failing the day
		// upstream publishes the next build.
		if strings.HasSuffix(k, "_URL") && strings.Contains(v, "lastSuccessfulBuild") {
			t.Errorf("%s names a moving build: %s", k, v)
		}
	}
	for k := range lock {
		if !strings.Contains(" "+m[1]+" ", " "+k+" ") {
			t.Errorf("game-stack.lock sets %s, which bootstrap.sh refuses as an unknown key", k)
		}
	}
	// Fill's URLs are content-addressed; a lock whose digest disagrees with its own URL
	// was edited by hand and half-way.
	for _, name := range []string{"PAPER", "VELOCITY"} {
		if !strings.Contains(lock[name+"_JAR_URL"], "/objects/"+lock[name+"_JAR_SHA256"]+"/") {
			t.Errorf("%s_JAR_SHA256 is not the digest in %s_JAR_URL", name, name)
		}
	}
	if !strings.Contains(lock["LIMBO_JAR_URL"], "-"+lock["MC_VERSION"]+".jar") {
		t.Errorf("LIMBO_JAR_URL %s is not a Minecraft %s build", lock["LIMBO_JAR_URL"], lock["MC_VERSION"])
	}
	if !strings.Contains(lock["PAPER_JAR_URL"], "/paper-"+lock["MC_VERSION"]+"-") {
		t.Errorf("PAPER_JAR_URL %s is not a Minecraft %s build; the lobby would not speak the login gate's protocol", lock["PAPER_JAR_URL"], lock["MC_VERSION"])
	}
}

// Each downloaded jar's digest is a build-arg bootstrap.sh passes and the Dockerfile must
// both require and spend on the file it downloaded; docker only warns about an unknown
// --build-arg, so a renamed arg would ship an unchecked jar.
func TestGameStackDigestsReachTheImageBuilds(t *testing.T) {
	script := BootstrapScript()
	for _, c := range []struct{ dockerfile, arg, path string }{
		{"deploy/limbo/Dockerfile", "LIMBO_JAR_SHA256", "/limbo/Limbo.jar"},
		{"deploy/limbo/Dockerfile", "LIMBO_SCHEM_SHA256", "/limbo/spawn.schem"},
		{"deploy/lobby/Dockerfile", "LUCKPERMS_JAR_SHA256", "/paper/plugins/LuckPerms.jar"},
	} {
		if !strings.Contains(script, "--build-arg "+c.arg+"=\"$"+c.arg+"\"") {
			t.Errorf("bootstrap.sh never passes --build-arg %s", c.arg)
		}
		dockerfile := readGameStackFile(t, c.dockerfile)
		if !strings.Contains(dockerfile, "ARG "+c.arg) {
			t.Errorf("%s declares no ARG %s", c.dockerfile, c.arg)
		}
		if !strings.Contains(dockerfile, `echo "$`+c.arg+`  `+c.path+`" | sha256sum -c`) {
			t.Errorf("%s never verifies %s against %s", c.dockerfile, c.path, c.arg)
		}
	}
}

// A base image named by tag alone is whatever the tag points at on build day.
func TestDockerfileBaseImagesArePinnedByDigest(t *testing.T) {
	root, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"Dockerfile": string(root)}
	for _, name := range []string{"deploy/limbo/Dockerfile", "deploy/lobby/Dockerfile", "deploy/paper/Dockerfile"} {
		files[name] = readGameStackFile(t, name)
	}
	pinned := regexp.MustCompile(`^FROM (--platform=\S+ )?\S+:\S+@sha256:[0-9a-f]{64}( AS \S+)?$`)
	for name, body := range files {
		n := 0
		for line := range strings.SplitSeq(body, "\n") {
			if !strings.HasPrefix(line, "FROM ") {
				continue
			}
			n++
			if !pinned.MatchString(line) {
				t.Errorf("%s: %q is not pinned by digest", name, line)
			}
		}
		if n == 0 {
			t.Errorf("%s has no FROM line", name)
		}
	}
}

// The plugin jars are built in four places the installer controls — the lobby and limbo
// image builds, bootstrap's Velocity build and the release build of the same jar — and
// through each module's wrapper by a developer or CI. A tag alone is whatever it points
// at on build day, and two Gradle versions are two chances for a build to pass in one
// place and break in the other, so all of them run one image, pinned by digest, whose
// Gradle is the wrappers' Gradle.
func TestPluginBuildsRunOnePinnedGradle(t *testing.T) {
	sources := map[string]string{
		"deploy/lobby/Dockerfile": readGameStackFile(t, "deploy/lobby/Dockerfile"),
		"deploy/limbo/Dockerfile": readGameStackFile(t, "deploy/limbo/Dockerfile"),
		"deploy/bootstrap.sh":     BootstrapScript(),
		// The release build compiles felis-velocity.jar for hosts that install prebuilt.
		"deploy/build-release-artifacts.sh": readRepoFile(t, "deploy/build-release-artifacts.sh"),
	}
	anyRef := regexp.MustCompile(`gradle:[\w.-]+(@sha256:\w+)?`)
	pinned := regexp.MustCompile(`^gradle:(\d+\.\d+(?:\.\d+)?)-jdk\d+@sha256:[0-9a-f]{64}$`)
	images := map[string]bool{}
	gradle := ""
	for name, body := range sources {
		refs := anyRef.FindAllString(body, -1)
		if len(refs) == 0 {
			t.Errorf("%s names no gradle image", name)
		}
		for _, ref := range refs {
			m := pinned.FindStringSubmatch(ref)
			if m == nil {
				t.Errorf("%s: %s is not a gradle image pinned by digest", name, ref)
				continue
			}
			images[ref] = true
			gradle = m[1]
		}
	}
	if len(images) != 1 {
		t.Fatalf("the plugin builds use %d different gradle images, want one: %v", len(images), images)
	}
	// bootstrap names the image once and has to spend it where it builds the jar.
	if !strings.Contains(BootstrapScript(), `"$PLUGIN_BUILD_IMAGE" gradle --no-daemon clean build`) {
		t.Error("build_velocity_plugin does not build in $PLUGIN_BUILD_IMAGE")
	}

	for _, module := range []string{"velocity", "paper", "limbo"} {
		props := wrapperProperties(t, module)
		if want := "gradle-" + gradle + "-bin.zip"; !strings.HasSuffix(props["distributionUrl"], "/"+want) {
			t.Errorf("plugins/%s wrapper runs %s; the image builds run Gradle %s", module, props["distributionUrl"], gradle)
		}
	}
	// The mods are no part of the install, but a wrapper without a checksum runs whatever
	// the download handed it.
	for _, module := range []string{"velocity", "paper", "limbo", "fabric", "forge", "neoforge"} {
		if sum := wrapperProperties(t, module)["distributionSha256Sum"]; !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sum) {
			t.Errorf("plugins/%s wrapper pins no distribution sha256 (got %q)", module, sum)
		}
	}
}

// Each plugin compiles against the API of the exact build the install runs, and Gradle
// checks those bytes against the module's verification file. Nothing but this test ties
// the three to deploy/game-stack.lock: a lock refresh that leaves them behind builds the
// lobby against yesterday's API, or fails every image build on a checksum the file does
// not have.
func TestPluginApisAreTheLockedBuilds(t *testing.T) {
	lock := gameStackLock(t)

	// Paper: paper-<mc>-<build>.jar runs; paper-api <mc>.build.<build>-<channel> compiles.
	jar := regexp.MustCompile(`/paper-([^/]+)-(\d+)\.jar$`).FindStringSubmatch(lock["PAPER_JAR_URL"])
	if jar == nil {
		t.Fatalf("PAPER_JAR_URL %s does not name paper-<mc>-<build>.jar", lock["PAPER_JAR_URL"])
	}
	dep := regexp.MustCompile(`compileOnly 'io\.papermc\.paper:paper-api:([^']+)'`).
		FindStringSubmatch(readGameStackFile(t, "plugins/paper/build.gradle"))
	if dep == nil {
		t.Fatal("plugins/paper/build.gradle declares no paper-api dependency")
	}
	if !regexp.MustCompile(`^` + regexp.QuoteMeta(jar[1]+".build."+jar[2]) + `(-[a-z]+)?$`).MatchString(dep[1]) {
		t.Errorf("paper-api %s is not the API of the locked server paper-%s-%s.jar", dep[1], jar[1], jar[2])
	}
	requireVerified(t, "paper", "io.papermc.paper", "paper-api", dep[1])

	// Limbo: the lock's release, passed to the image build, which refuses to guess one.
	limbo := lock["LIMBO_VERSION"]
	if !strings.Contains(BootstrapScript(), `--build-arg LIMBO_VERSION="$LIMBO_VERSION"`) {
		t.Error("bootstrap.sh does not pass the locked LIMBO_VERSION to the limbo image build")
	}
	dockerfile := readGameStackFile(t, "deploy/limbo/Dockerfile")
	if !regexp.MustCompile(`(?m)^ARG LIMBO_VERSION$`).MatchString(dockerfile) ||
		!strings.Contains(dockerfile, `if [ -z "${LIMBO_VERSION:-}" ]`) {
		t.Error("deploy/limbo/Dockerfile does not require LIMBO_VERSION; a build without it would " +
			"compile against a version nobody chose")
	}
	// LOOHP publishes the jar the login gate runs as the Limbo API artifact itself, so the
	// checksum Gradle holds for it is the lock's: compiled-against and running are one file.
	if got := requireVerified(t, "limbo", "com.loohp", "Limbo", limbo)["Limbo-"+limbo+".jar"]; got != lock["LIMBO_JAR_SHA256"] {
		t.Errorf("verification-metadata.xml holds %q for Limbo-%s.jar; the login gate runs %s", got, limbo, lock["LIMBO_JAR_SHA256"])
	}

	// Velocity: the API default is the proxy the install runs.
	api := regexp.MustCompile(`findProperty\('velocityApi'\) \?: '([^']+)'`).
		FindStringSubmatch(readGameStackFile(t, "plugins/velocity/build.gradle"))
	if api == nil {
		t.Fatal("plugins/velocity/build.gradle has no velocityApi default")
	}
	if api[1] != lock["VELOCITY_VERSION"] {
		t.Errorf("velocity-api defaults to %s; the install runs Velocity %s", api[1], lock["VELOCITY_VERSION"])
	}
	requireVerified(t, "velocity", "com.velocitypowered", "velocity-api", api[1])
}

// requireVerified asserts the module's shipped verification file checks metadata and
// pins group:name:version, and returns that component's artifact sha256s by file name.
func requireVerified(t *testing.T, module, group, name, version string) map[string]string {
	t.Helper()
	path := "plugins/" + module + "/gradle/verification-metadata.xml"
	var doc struct {
		VerifyMetadata bool `xml:"configuration>verify-metadata"`
		Components     []struct {
			Group     string `xml:"group,attr"`
			Name      string `xml:"name,attr"`
			Version   string `xml:"version,attr"`
			Artifacts []struct {
				Name   string `xml:"name,attr"`
				SHA256 []struct {
					Value string `xml:"value,attr"`
				} `xml:"sha256"`
			} `xml:"artifact"`
		} `xml:"components>component"`
	}
	if err := xml.Unmarshal([]byte(readGameStackFile(t, path)), &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if !doc.VerifyMetadata {
		t.Errorf("%s does not verify metadata; a swapped pom could redirect the graph", path)
	}
	for _, c := range doc.Components {
		if c.Group != group || c.Name != name || c.Version != version {
			continue
		}
		sums := map[string]string{}
		for _, a := range c.Artifacts {
			if len(a.SHA256) > 0 {
				sums[a.Name] = a.SHA256[0].Value
			}
		}
		if sums[name+"-"+version+".jar"] == "" {
			t.Errorf("%s pins %s:%s:%s but no sha256 for its jar", path, group, name, version)
		}
		return sums
	}
	t.Errorf("%s has no checksum for %s:%s:%s; the build would refuse it", path, group, name, version)
	return nil
}

// wrapperProperties reads a module's gradle-wrapper.properties off disk: the wrappers are
// for developers and CI, and nothing embeds them.
func wrapperProperties(t *testing.T, module string) map[string]string {
	t.Helper()
	b, err := os.ReadFile("plugins/" + module + "/gradle/wrapper/gradle-wrapper.properties")
	if err != nil {
		t.Fatal(err)
	}
	props := map[string]string{}
	for line := range strings.SplitSeq(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(k, "#") {
			props[k] = strings.ReplaceAll(v, `\:`, ":")
		}
	}
	return props
}

// gameStackLock parses the shipped deploy/game-stack.lock the way bootstrap.sh does.
func gameStackLock(t *testing.T) map[string]string {
	t.Helper()
	lock := map[string]string{}
	for line := range strings.SplitSeq(readGameStackFile(t, "deploy/game-stack.lock"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("not a KEY=value line: %q", line)
		}
		lock[k] = v
	}
	return lock
}

// readRepoFile reads a file of the checkout that the binary does not embed.
func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readGameStackFile(t *testing.T, name string) string {
	t.Helper()
	b, err := gameStackAssets.ReadFile(name)
	if err != nil {
		t.Fatalf("read embedded %s: %v", name, err)
	}
	return string(b)
}
