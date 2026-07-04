package updater

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"felis.lolicon.best/internal/updates"
)

// This file is the VERIFIABLE core of the current-version gatherer: the pure
// extractors that turn a raw system string (a `--version` line, a container image
// reference, a proxy jar filename) into an updates.Version, plus a sysGatherer that
// dispatches each component to the right extractor over an injected seam. The seams'
// actual I/O — shelling out, reading a running pod's image, listing an off-cluster
// jar — is integration and lives in gatherer_integration.go; here every path is
// exercised with a fake, exactly as the release sources are exercised with httptest.
//
// Why the extraction is load-bearing enough to unit-test: Current() feeds
// updates.Run's comparison. A mis-read current version is not a cosmetic bug — it
// silently makes every downstream decision wrong (a spurious apply, or a missed
// upgrade). The subtlety that proves the point is k3s: its Git tag "v1.36.2+k3s1"
// parses as a STABLE release (build metadata after '+' is ignored), but a container
// registry cannot store '+' in a tag, so the SAME build ships as the image tag
// "v1.36.2-k3s1" — which, taken literally, parses as a PRERELEASE ("-k3s1" tail) and
// would wrongly bar a stable image from comparison. versionFromImageRef normalizes
// that convention back; versionFromCLI never sees it because the k3s binary prints the
// '+' form.

// commandRunner is the exec seam: it runs a binary and returns its combined output.
// The production implementation (execRunner, gatherer_integration.go) shells out to
// the real k3s/cloudflared binary; tests inject a fake that returns canned output, so
// the extraction logic is proven without either tool installed.
type commandRunner interface {
	output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// versionFromCLI extracts a version from a `<tool> --version` banner. It scans the
// whitespace-separated tokens and returns the FIRST that parses as a version, which
// matches the universal "<name> version <V> (<build>)" convention while tolerating the
// differing preambles and trailing build/date fields:
//
//	k3s:         "k3s version v1.36.2+k3s1 (a1b2c3)\ngo version go1.24.0"  -> v1.36.2+k3s1
//	cloudflared: "cloudflared version 2026.6.1 (built 2026-06-20-1057 UTC)" -> 2026.6.1
//
// It fails closed: output with no parseable token is an error, never a zero version.
func versionFromCLI(raw string) (updates.Version, error) {
	for _, tok := range strings.Fields(raw) {
		if v, err := updates.Parse(tok); err == nil {
			return v, nil
		}
	}
	return updates.Version{}, fmt.Errorf("updater: no version token in CLI output %q", truncate(raw, 80))
}

// dockerBuildSuffix matches the k3s / rke2 build metadata that a container tag encodes
// with '-' because a Docker tag may not contain the SemVer '+'. Restoring the '+'
// makes the image tag parse to the same STABLE version the CLI reports.
var dockerBuildSuffix = regexp.MustCompile(`-(k3s\d+|rke2r\d+)$`)

// versionFromImageRef extracts a version from a container image reference's tag:
//
//	"rancher/k3s:v1.36.2-k3s1"                    -> v1.36.2  (stable; "-k3s1" restored to "+k3s1")
//	"ghcr.io/acme/felis-api:1.4.0"                -> 1.4.0
//	"localhost:5000/felis-api:1.4.0@sha256:deadbeef" -> 1.4.0  (registry port kept, digest stripped)
//
// It strips any "@sha256:" digest, takes the tag after the last ':' of the final path
// segment (so a "host:port/repo" registry port is not mistaken for the tag), restores
// the Docker-encoded k3s/rke2 build suffix, and parses. A reference with no tag or a
// non-version tag ("latest") fails closed.
func versionFromImageRef(ref string) (updates.Version, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return updates.Version{}, fmt.Errorf("updater: empty image reference")
	}
	if i := strings.IndexByte(ref, '@'); i >= 0 { // strip a "...@sha256:..." digest
		ref = ref[:i]
	}
	// The tag, if any, is after the last ':' within the final path segment; a ':' in an
	// earlier segment is a registry host port, not a tag separator.
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

// jarVersion matches the first dotted-numeric run in a filename (three parts preferred
// over two so "3.4.0" wins whole).
var jarVersion = regexp.MustCompile(`\d+\.\d+\.\d+|\d+\.\d+`)

// versionFromJarName extracts a version from a proxy jar filename:
//
//	"velocity-3.4.0-SNAPSHOT-461.jar" -> 3.4.0
//	"velocity-3.4.0.jar"              -> 3.4.0
//
// It is best-effort by nature (an admin may rename the jar): it returns the numeric
// core of the first version-looking token and fails closed if the name carries none.
// A "-SNAPSHOT" qualifier is intentionally dropped — Velocity is Notify-only and never
// auto-applied, so a slightly optimistic current only affects an advisory message.
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

// sysGatherer is the production VersionGatherer. It reads each component's CURRENT
// version by the method that component exposes — a CLI banner for the node binaries
// (k3s, cloudflared), the running pod's image tag for the control plane (felis-api),
// the installed jar's name for the off-cluster proxy (velocity) — and turns it into a
// Version with the pure extractors above. The three seams are injected: `run` (exec)
// is wired in production; `imageForSpec` and `jarForSpec` are the two reads that still
// need real infra (a k8s client, off-cluster host access) and are nil until built, so
// those components surface a clear gather error rather than a wrong version.
//
// Dispatch is keyed by the component identities in Topology(); an unrecognized name is
// a loud error, not a silent skip.
type sysGatherer struct {
	run          commandRunner
	imageForSpec func(ctx context.Context, spec Spec) (string, error)
	jarForSpec   func(ctx context.Context, spec Spec) (string, error)
}

// Current implements VersionGatherer.
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

// cliVersion runs `<bin> --version` through the exec seam and extracts the version.
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

// truncate bounds an error's echo of untrusted output.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
