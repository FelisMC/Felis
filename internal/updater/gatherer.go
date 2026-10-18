package updater

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"felis.lolicon.best/internal/updates"
)

type commandRunner interface {
	output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// versionFromCLI returns the first parseable whitespace-delimited version or an error if none is present.
func versionFromCLI(raw string) (updates.Version, error) {
	for _, tok := range strings.Fields(raw) {
		if v, err := updates.Parse(tok); err == nil {
			return v, nil
		}
	}
	return updates.Version{}, fmt.Errorf("updater: no version token in CLI output %q", truncate(raw, 80))
}

// dockerBuildSuffix restores k3s/rke2 build metadata from tag-safe hyphens to SemVer plus signs.
var dockerBuildSuffix = regexp.MustCompile(`-(k3s\d+|rke2r\d+)$`)

// versionFromImageRef ignores digests and registry ports, restores encoded build suffixes, and rejects non-version tags.
func versionFromImageRef(ref string) (updates.Version, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return updates.Version{}, fmt.Errorf("updater: empty image reference")
	}
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	lastSeg := ref[strings.LastIndexByte(ref, '/')+1:]
	colon := strings.LastIndexByte(lastSeg, ':')
	if colon < 0 {
		return updates.Version{}, fmt.Errorf("updater: image reference %q has no tag", ref)
	}
	tag := lastSeg[colon+1:]
	if tag == "" {
		return updates.Version{}, fmt.Errorf("updater: image reference %q has an empty tag", ref)
	}
	tag = dockerBuildSuffix.ReplaceAllString(tag, "+$1")
	v, err := updates.Parse(tag)
	if err != nil {
		return updates.Version{}, fmt.Errorf("updater: image tag: %w", err)
	}
	return v, nil
}

var jarVersion = regexp.MustCompile(`\d+\.\d+\.\d+|\d+\.\d+`)

// versionFromJarName drops snapshot qualifiers for advisory reporting; it must not drive unattended application.
func versionFromJarName(name string) (updates.Version, error) {
	m := jarVersion.FindString(name)
	if m == "" {
		return updates.Version{}, fmt.Errorf("updater: no version in jar name %q", name)
	}
	v, err := updates.Parse(m)
	if err != nil {
		return updates.Version{}, fmt.Errorf("updater: jar name %q: %w", name, err)
	}
	return v, nil
}

// sysGatherer reports missing injected readers as errors instead of supplying zero versions.
type sysGatherer struct {
	run          commandRunner
	imageForSpec func(ctx context.Context, spec Spec) (string, error)
	jarForSpec   func(ctx context.Context, spec Spec) (string, error)
}

func (g sysGatherer) Current(ctx context.Context, spec Spec) (updates.Version, error) {
	switch spec.Name {
	case "k3s":
		return g.cliVersion(ctx, "k3s")
	case "cloudflared":
		return g.cliVersion(ctx, "cloudflared")
	case "felis-api":
		if g.imageForSpec == nil {
			return updates.Version{}, fmt.Errorf("updater: image gather seam for %q not wired", spec.Name)
		}
		ref, err := g.imageForSpec(ctx, spec)
		if err != nil {
			return updates.Version{}, fmt.Errorf("updater: read image for %q: %w", spec.Name, err)
		}
		return versionFromImageRef(ref)
	case "velocity":
		if g.jarForSpec == nil {
			return updates.Version{}, fmt.Errorf("updater: jar gather seam for %q not wired", spec.Name)
		}
		name, err := g.jarForSpec(ctx, spec)
		if err != nil {
			return updates.Version{}, fmt.Errorf("updater: read jar for %q: %w", spec.Name, err)
		}
		return versionFromJarName(name)
	default:
		return updates.Version{}, fmt.Errorf("updater: no gather method for component %q", spec.Name)
	}
}

func (g sysGatherer) cliVersion(ctx context.Context, bin string) (updates.Version, error) {
	if g.run == nil {
		return updates.Version{}, fmt.Errorf("updater: command runner not wired for %q", bin)
	}
	out, err := g.run.output(ctx, bin, "--version")
	if err != nil {
		return updates.Version{}, fmt.Errorf("updater: run %s --version: %w", bin, err)
	}
	v, err := versionFromCLI(string(out))
	if err != nil {
		return updates.Version{}, fmt.Errorf("updater: %s: %w", bin, err)
	}
	return v, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
