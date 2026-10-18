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

	// The server may become unavailable after readiness was checked; the command still maps connection failures to 503.
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !info.Ready {
		writeError(w, r, newError(http.StatusConflict, "not_running",
			"server is not running; wake it before sending console commands"))
		return
	}

	if a.Console == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "console_unavailable",
			"console subsystem is not configured"))
		return
	}

	output, err := a.Console.RunCommand(r.Context(), name, command)
	switch {
	case errors.Is(err, ErrConsoleUnavailable):
		writeError(w, r, newError(http.StatusServiceUnavailable, "console_unavailable",
			"server console is currently unreachable; wake the server and retry"))
		return
	case err != nil:
		a.writeLookupError(w, r, err)
		return
	}

	a.audit(r, "console.command", name)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "output": output})
}
