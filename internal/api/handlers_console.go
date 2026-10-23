package api

import (
	"errors"
	"net/http"
)

// maxConsoleCommandLen stays below the RCON packet limit including framing.
const maxConsoleCommandLen = 1024

type commandRequest struct {
	Command string `json:"command"`
}

func (a *API) handleCommand(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	var body commandRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	command, err := normalizeConsoleCommand(body.Command)
	if err != nil {
		writeError(w, r, err)
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

	// The transport checks again after its per-server lock; readiness is not required during startup.
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if info.DesiredState != "Running" || info.Phase != "Running" && info.Phase != "Starting" {
		writeError(w, r, newError(http.StatusConflict, "not_running",
			"start the server before sending console commands"))
		return
	}
	if err := a.Cluster.CheckFileAccess(r.Context(), name); err != nil {
		a.writeLookupError(w, r, maintenanceError(err, ""))
		return
	}

	if a.Console == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "console_unavailable",
			"console subsystem is not configured"))
		return
	}

	result, err := a.Console.SubmitCommand(r.Context(), name, command)
	switch {
	case errors.Is(err, ErrConsoleOutcomeUnknown):
		a.audit(r, "console.command.uncertain", name)
		writeError(w, r, newError(http.StatusBadGateway, "command_outcome_unknown",
			"the command may already have executed; check the console log before sending it again"))
		return
	case errors.Is(err, ErrConsoleNotRunning):
		writeError(w, r, newError(http.StatusConflict, "not_running", "the server stopped accepting console input"))
		return
	case errors.Is(err, ErrConsoleUnavailable):
		writeError(w, r, newError(http.StatusServiceUnavailable, "console_unavailable",
			"no writable game console is available; wait for startup or restart the server after updating"))
		return
	case err != nil:
		a.writeLookupError(w, r, err)
		return
	}

	a.audit(r, "console.command", name)
	writeJSON(w, http.StatusOK, result)
}
