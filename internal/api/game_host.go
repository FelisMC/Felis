package api

import (
	"context"
	"net/http"
	"time"
)

// minGameHostBytes is the memory a single host needs to run game servers beside the platform; a "4 GB" host reports about 3.5 GiB.
const minGameHostBytes = 3584 << 20

// checkGameHost refuses a new server on a single-node install whose host is too small to run one beside the platform.
func (a *API) checkGameHost(ctx context.Context) error {
	if a.Distribution != nil || a.FreeMemory == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	nodes, err := a.FreeMemory(ctx)
	if err != nil || len(nodes) == 0 {
		return nil
	}
	for _, n := range nodes {
		if n.Capacity == 0 || n.Capacity >= minGameHostBytes {
			return nil
		}
	}
	return newError(http.StatusConflict, "host_too_small",
		"this host has under 4 GB of memory and runs only the platform; game servers need a 4 GB host or a worker node")
}
