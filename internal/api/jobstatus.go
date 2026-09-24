package api

import (
	"context"
	"net/http"
	"time"

	"felis.lolicon.best/internal/naming"
)

// AsyncJob is the observable outcome of one asynchronous world operation. The API
// only ENQUEUES backup/restore Jobs — the work, and any failure, happens in the
// cluster — so without this projection a failed job left its only trace in a Job
// object an operator with kubectl could read. The route is the API-side outlet.
type AsyncJob struct {
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`  // "backup" | "restore"
	State      string    `json:"state"` // "running" | "succeeded" | "failed"
	Message    string    `json:"message,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// JobStatusReader reads the newest backup/restore Jobs for a server, newest
// first. Optional like Restorer/Backuper: when nil the route answers 503.
type JobStatusReader interface {
	LatestJobs(ctx context.Context, serverName string) ([]AsyncJob, error)
}

// handleServerJobs serves GET /api/v1/servers/{name}/jobs — the latest async world
// operations for one server, so a 202 that later failed is visible without kubectl.
// Authorization mirrors the backup/restore gates' front half (owner-or-admin); the
// message is free-form Job text and can name paths the owner already sees through
// the file editor.
func (a *API) handleServerJobs(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return
	}
	if a.JobStatus == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "jobs_unavailable",
			"job status is not configured"))
		return
	}
	jobs, err := a.JobStatus.LatestJobs(r.Context(), name)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if jobs == nil {
		jobs = []AsyncJob{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": name, "jobs": jobs})
}
