package updater

import (
	"context"
	"os/exec"
)

// This file is INTEGRATION-ONLY. execRunner shells out to the real k3s / cloudflared
// binaries; it cannot run on a box without them and is never exercised by the unit
// tests — the extraction logic it feeds (gatherer.go) is proven with a fake
// commandRunner. This mirrors internal/cfsetup, which keeps its ExecRunner apart from
// its unit-verified core.

// execRunner is the production commandRunner: it runs the tool and returns combined
// output (banners sometimes print to stderr), bounded by the caller's context.
type execRunner struct{}

func (execRunner) output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// NewSysGatherer builds the production VersionGatherer. The CLI seam (k3s / cloudflared
// `--version`) is wired and works on a real node. The felis-api pod-image read and the
// off-cluster Velocity jar inspection are the remaining integration seams; they are
// deliberately left nil, so those two components surface an explicit "gather seam not
// wired" error (which the Runner records and skips) rather than being planned against a
// wrong or zero version. Wiring them — a k8s client read of the control-plane
// Deployment's image, and however the operator exposes the proxy host — is the next
// integration step, tracked in doc.go. When the jar seam lands, revisit
// versionFromJarName's "-SNAPSHOT" handling: it reports a snapshot build as its stable
// core, which is harmless while this seam is nil and Velocity is Notify-only, but would
// suppress a legitimate "a stable is now out" notice once a real current flows.
func NewSysGatherer() VersionGatherer {
	return sysGatherer{run: execRunner{}}
}
