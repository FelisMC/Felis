package api

import (
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
	writeJSON(w, http.StatusOK, view)
}
