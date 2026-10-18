package updater

import (
	"context"
	"os/exec"
)

type execRunner struct{}

func (execRunner) output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// NewSysGatherer wires CLI reads only; callers need injected image and jar readers or NewHostGatherer for those components.
func NewSysGatherer() VersionGatherer {
	return sysGatherer{run: execRunner{}}
}
