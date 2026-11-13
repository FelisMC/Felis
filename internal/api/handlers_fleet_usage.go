package api

import (
	"context"
	"net/http"
	"time"
)

func (a *API) handleFleetUsage(w http.ResponseWriter, r *http.Request) {
	now := a.now()
	view := PlatformUsage{History: []PlatformUsagePoint{}, Nodes: []NodeUsage{}}
	if a.Usage != nil {
		view = a.Usage.PlatformSnapshot(now)
	}
	failures, err := a.Repo.CountAuthFailures(r.Context(), now.Add(-24*time.Hour))
	if err != nil {
		writeError(w, r, err)
		return
	}
	view.LoginFailures24h = failures
	if a.FreeMemory != nil && r.URL.Query().Get("include") == "free_memory" {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		if free, err := a.FreeMemory(ctx); err == nil {
			view.FreeMemory = free
		}
		cancel()
	}
	writeJSON(w, http.StatusOK, view)
}
