package api

import (
	"context"
	"net/http"
	"time"
)

type AsyncJob struct {
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
	Message    string    `json:"message,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	// ThenRestore tracks the safety snapshot's follow-up restore as pending, started or abandoned with a reason.
	ThenRestore       string `json:"then_restore,omitempty"`
	ThenRestoreReason string `json:"then_restore_reason,omitempty"`
	RestoreBackupID   string `json:"restore_backup_id,omitempty"`
	Scheduled         bool   `json:"scheduled,omitempty"`
}

type JobStatusReader interface {
	LatestJobs(ctx context.Context, serverName string) ([]AsyncJob, error)
}

func (a *API) handleServerJobs(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	rec, err := a.managedServerRecord(r.Context(), name)
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
