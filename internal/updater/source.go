package updater

import (
	"context"
	"errors"
	"fmt"

	"felis.lolicon.best/internal/updates"
)

// errGitHubNotWired is returned by RoutingSource for GitHub-backed components until
// the GitHub Releases source is implemented. updates.Run tolerates a per-component
// source error (records it in SourceErrors and plans that component to ActionNone),
// so an un-wired GitHub source degrades those components to "latest unknown" in the
// report — honest, never a fabricated version. It is a distinct sentinel so the gap
// is greppable and testable, not silently swallowed.
var errGitHubNotWired = errors.New("updater: github release source not yet wired")

// RoutingSource is the production updates.ReleaseSource. updates.Run calls a single
// source for every non-pinned component, so this one dispatches each component to its
// configured upstream by the topology. Today it fully implements the PaperMC route
// (Velocity) and returns errGitHubNotWired for the GitHub-backed components
// (felis-api, k3s, cloudflared), which are enumerated remaining integration.
type RoutingSource struct {
	routes map[string]Spec
	paper  paperMC
}

// NewRoutingSource builds the router from a topology. Pinned specs are indexed too
// but never reached (Run skips pinned components before calling Latest).
func NewRoutingSource(specs []Spec) *RoutingSource {
	routes := make(map[string]Spec, len(specs))
	for _, s := range specs {
		routes[s.Name] = s
	}
	return &RoutingSource{routes: routes, paper: newPaperMC()}
}

// Latest implements updates.ReleaseSource. An unknown component name is an error, not
// a silent zero, so a topology/route mismatch is loud.
func (r *RoutingSource) Latest(ctx context.Context, comp updates.Component) (updates.Version, error) {
	spec, ok := r.routes[comp.Name]
	if !ok {
		return updates.Version{}, fmt.Errorf("updater: no source route for %q", comp.Name)
	}
	switch spec.Source {
	case sourcePaperMC:
		return r.paper.latestStable(ctx, spec.Coord)
	case sourceGitHub:
		return updates.Version{}, errGitHubNotWired
	case sourceNone:
		// A pinned component (Run never reaches this, but be explicit and loud).
		return updates.Version{}, fmt.Errorf("updater: %q is pinned and has no release source", comp.Name)
	default:
		return updates.Version{}, fmt.Errorf("updater: component %q has an unknown source kind", comp.Name)
	}
}
