package updater

import (
	"context"
	"fmt"

	"felis.lolicon.best/internal/updates"
)

// RoutingSource is the production updates.ReleaseSource. updates.Run calls a single
// source for every non-pinned component, so this one dispatches each component to its
// configured upstream by the topology: the PaperMC Fill API for Velocity, and the
// GitHub Releases API for felis-api, k3s and cloudflared.
type RoutingSource struct {
	routes map[string]Spec
	paper  paperMC
	gh     github
}

// NewRoutingSource builds the router from a topology. Pinned specs are indexed too
// but never reached (Run skips pinned components before calling Latest).
func NewRoutingSource(specs []Spec) *RoutingSource {
	routes := make(map[string]Spec, len(specs))
	for _, s := range specs {
		routes[s.Name] = s
	}
	return &RoutingSource{routes: routes, paper: newPaperMC(), gh: newGitHub()}
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
		return r.gh.latestStable(ctx, spec.Coord)
	case sourceNone:
		// A pinned component (Run never reaches this, but be explicit and loud).
		return updates.Version{}, fmt.Errorf("updater: %q is pinned and has no release source", comp.Name)
	default:
		return updates.Version{}, fmt.Errorf("updater: component %q has an unknown source kind", comp.Name)
	}
}
