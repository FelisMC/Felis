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

// DefaultVelocityJarPath has no version in its filename, so normal host gathering reads the manifest.
const DefaultVelocityJarPath = "/opt/felis/velocity/velocity.jar"

const DefaultJREReleasePath = "/opt/felis/jre/release"

// NewHostGatherer uses the CLI build stamp and local proxy jar; an empty jar path selects DefaultVelocityJarPath.
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

type hostGatherer struct {
	sys          sysGatherer
	felisVersion string
	velocityJar  string
	jreRelease   string
}

func (g hostGatherer) Current(ctx context.Context, spec Spec) (updates.Version, error) {
	switch spec.Name {
	case "felis-api":
		// Reject unstamped dev builds instead of inventing a version that would make every release appear newer.
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

// postgresVersionArgv reads the running database container; host package versions may describe an unused installation.
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

// jreReleaseVersion prefers SEMANTIC_VERSION then JAVA_VERSION, excluding JAVA_RUNTIME_VERSION's misleading LTS qualifier.
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

// velocityJarVersion prefers Implementation-Version and falls back to a versioned filename for manually installed jars.
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
	if v, ferr := versionFromJarName(filepath.Base(path)); ferr == nil {
		return v, nil
	}
	return updates.Version{}, fmt.Errorf("updater: no version in %s manifest or filename: %w", path, err)
}

// manifestAttr does not unfold 72-byte continuation lines; callers must limit it to short attributes such as versions.
func manifestAttr(zr *zip.Reader, key string) (string, error) {
	f, err := zr.Open("META-INF/MANIFEST.MF")
	if err != nil {
		return "", fmt.Errorf("updater: jar has no manifest: %w", err)
	}
	defer f.Close()

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
