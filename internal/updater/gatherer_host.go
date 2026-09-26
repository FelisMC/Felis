package updater

import (
	"archive/zip"
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/updates"
)

// This file wires the two gather seams NewSysGatherer deliberately leaves nil, for
// the one caller that can satisfy them without a cluster client: the on-host `felis
// update` CLI. Both reads are answered from the node the command runs on, which is
// exactly where bootstrap.sh installed the things being read.
//
// felis-api is answered from the RUNNING BINARY's own build stamp rather than from
// the control-plane Deployment's image tag. That is not a shortcut around the k8s
// read — it is the identity the user named ("当前版本号通过 felis version 查询"), and
// it is the same artifact: deploy/bootstrap.sh builds the felis image from the same
// checkout it installs /usr/local/bin/felis from, stamping both with one `git
// describe`. Reading the Deployment would answer a slightly different question (what
// is rolled out) and is still the right seam for the in-cluster CronJob path; it is
// left to NewSysGatherer, which keeps returning "not wired" there.
//
// The panel has no version of its own on purpose: it is compiled into the felis
// binary with //go:embed, so "the panel version" IS the felis version. The same is
// true of the plugin jars, which are built from this repo in the same bootstrap run.
// That is why the CLI's --panel and --plugins selectors both resolve here.

// DefaultVelocityJarPath is where deploy/bootstrap.sh installs the proxy. The jar is
// installed under a FIXED name with no version in it (install_velocity does
// `atomic_install_file "$tmp" "${VELOCITY_DIR}/velocity.jar"`), which is why the
// filename extractor alone cannot answer for Velocity and the manifest read below
// exists.
const DefaultVelocityJarPath = "/opt/felis/velocity/velocity.jar"

// DefaultJREReleasePath is the release file of the runtime deploy/bootstrap.sh
// installs for Velocity (install_jre unpacks Temurin into /opt/felis/jre).
const DefaultJREReleasePath = "/opt/felis/jre/release"

// NewHostGatherer builds the VersionGatherer for `felis update` running on the node.
// felisVersion is the CLI's own resolved build stamp; velocityJar is the installed
// proxy jar (empty means DefaultVelocityJarPath). k3s and cloudflared keep the
// existing exec seam — they are real binaries on this host and already answer.
func NewHostGatherer(felisVersion, velocityJar string) VersionGatherer {
	if velocityJar == "" {
		velocityJar = DefaultVelocityJarPath
	}
	return hostGatherer{
		sys:          sysGatherer{run: execRunner{}},
		felisVersion: felisVersion,
		velocityJar:  velocityJar,
		jreRelease:   DefaultJREReleasePath,
	}
}

// hostGatherer answers the two seams it can satisfy from the local filesystem and
// delegates every other component to the standard system gatherer, so the CLI and
// the (future) in-cluster runner share one dispatch table and cannot drift.
type hostGatherer struct {
	sys          sysGatherer
	felisVersion string
	velocityJar  string
	jreRelease   string
}

// Current implements VersionGatherer.
func (g hostGatherer) Current(ctx context.Context, spec Spec) (updates.Version, error) {
	switch spec.Name {
	case "felis-api":
		// An unstamped local `go build` reports "dev", which is not a version. Fail
		// closed with an actionable message rather than inventing a 0.0.0 that would
		// make every release upstream look like an upgrade.
		v, err := updates.Parse(g.felisVersion)
		if err != nil {
			return updates.Version{}, fmt.Errorf("updater: felis build stamp %q is not a version (an unstamped build cannot be compared): %w", g.felisVersion, err)
		}
		return v, nil
	case "velocity":
		return velocityJarVersion(g.velocityJar)
	case "jre":
		return jreReleaseVersion(g.jreRelease)
	case "postgresql":
		return g.postgresVersion(ctx)
	default:
		return g.sys.Current(ctx, spec)
	}
}

// postgresVersionArgv asks the server binary in the felis-postgres Deployment
// (internal/platform/postgres.go) for its version. The database runs from the image
// the release pins, so the answer comes from the container: a distribution package
// the move into k3s left installed on the host answers with a version nothing runs.
var postgresVersionArgv = []string{
	"kubectl", "exec", "-n", platform.DefaultControlNamespace, "deploy/" + platform.PostgresName,
	"-c", platform.PostgresContainer, "--", "postgres", "--version",
}

func (g hostGatherer) postgresVersion(ctx context.Context) (updates.Version, error) {
	if g.sys.run == nil {
		return updates.Version{}, fmt.Errorf("updater: command runner not wired for %s", platform.PostgresName)
	}
	out, err := g.sys.run.output(ctx, "k3s", postgresVersionArgv...)
	if err != nil {
		if msg := truncate(string(out), 200); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return updates.Version{}, fmt.Errorf("updater: ask %s for its version: %w", platform.PostgresName, err)
	}
	v, err := versionFromCLI(string(out))
	if err != nil {
		return updates.Version{}, fmt.Errorf("updater: %s: %w", platform.PostgresName, err)
	}
	return v, nil
}

// jreReleaseVersion reads the runtime's version from its release file. Temurin writes
// SEMANTIC_VERSION="25.0.4.1+1"; JAVA_VERSION="25.0.4.1" is the fallback every JDK
// build writes. JAVA_RUNTIME_VERSION is avoided: its "-LTS" tail reads as a prerelease.
func jreReleaseVersion(path string) (updates.Version, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return updates.Version{}, fmt.Errorf("updater: read JRE release file: %w", err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			fields[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	for _, key := range []string{"SEMANTIC_VERSION", "JAVA_VERSION"} {
		if fields[key] == "" {
			continue
		}
		if v, err := updates.Parse(fields[key]); err == nil {
			return v, nil
		}
	}
	return updates.Version{}, fmt.Errorf("updater: no SEMANTIC_VERSION or JAVA_VERSION in %s", path)
}

// velocityJarVersion reads the installed proxy's version out of the jar itself.
//
// It reads META-INF/MANIFEST.MF's Implementation-Version, which is authoritative
// rather than incidental: Velocity reports its own version at runtime from that
// attribute (getImplementationVersion), so the value here is the same string the
// proxy prints about itself. The filename is only a fallback, and on a Felis node it
// is expected to fail — bootstrap installs the jar as a fixed "velocity.jar" — so it
// exists for the hand-placed "velocity-3.5.1.jar" case, not the normal one.
func velocityJarVersion(path string) (updates.Version, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return updates.Version{}, fmt.Errorf("updater: open velocity jar %s: %w", path, err)
	}
	defer zr.Close()

	raw, err := manifestAttr(&zr.Reader, "Implementation-Version")
	if err == nil {
		v, perr := updates.Parse(raw)
		if perr == nil {
			return v, nil
		}
		err = perr
	}
	// Fall back to the filename before surfacing the manifest failure, so a jar whose
	// manifest is missing or unparseable still answers when the name carries a version.
	if v, ferr := versionFromJarName(filepath.Base(path)); ferr == nil {
		return v, nil
	}
	return updates.Version{}, fmt.Errorf("updater: no version in %s manifest or filename: %w", path, err)
}

// manifestAttr returns one attribute value from a jar's META-INF/MANIFEST.MF.
//
// This does not implement the JAR spec's 72-byte line folding (a wrapped
// value continues on the next line after a single leading space). Version values are
// far short of the wrap point, so folding cannot bite here; if this ever reads a long
// attribute, join continuation lines before splitting on ':'.
func manifestAttr(zr *zip.Reader, key string) (string, error) {
	f, err := zr.Open("META-INF/MANIFEST.MF")
	if err != nil {
		return "", fmt.Errorf("updater: jar has no manifest: %w", err)
	}
	defer f.Close()

	// Bound the read: a manifest is a few KiB, and this parses a file that arrived
	// over the network in an earlier bootstrap run.
	sc := bufio.NewScanner(io.LimitReader(f, 256<<10))
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), key) {
			continue
		}
		if v := strings.TrimSpace(value); v != "" {
			return v, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("updater: read manifest: %w", err)
	}
	return "", fmt.Errorf("updater: manifest has no %s", key)
}
