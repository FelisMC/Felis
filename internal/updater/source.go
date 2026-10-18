package updater

import (
	"context"
	"fmt"

	"felis.lolicon.best/internal/updates"
)

// RoutingSource dispatches release lookups according to component topology.
type RoutingSource struct {
	routes map[string]Spec
	paper  paperMC
	gh     github
	pg     postgresFeed
	// notes holds per-component release details; updates.Run calls Latest sequentially.
	notes map[string]string
}

func NewRoutingSource(specs []Spec) *RoutingSource {
	routes := make(map[string]Spec, len(specs))
	for _, s := range specs {
		routes[s.Name] = s
	}
	return &RoutingSource{routes: routes, paper: newPaperMC(), gh: newGitHub(), pg: newPostgresFeed(), notes: map[string]string{}}
}

func (r *RoutingSource) Notes() map[string]string { return r.notes }

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
	case sourceTemurin:
		// Keep Temurin discovery within the installed feature release; changing feature versions requires an installer pin update.
		return r.gh.latestTemurin(ctx, comp.Current.Major)
	case sourcePostgres:
		rel, err := r.pg.latest(ctx, comp.Current)
		if err != nil {
			return updates.Version{}, err
		}
		if rel.note != "" {
			r.notes[comp.Name] = rel.note
		}
		return rel.latest, nil
	case sourceNone:
		return updates.Version{}, fmt.Errorf("updater: %q is pinned and has no release source", comp.Name)
	default:
		return updates.Version{}, fmt.Errorf("updater: component %q has an unknown source kind", comp.Name)
	}
}
