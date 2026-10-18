package api

import (
	"net/http"

	"github.com/google/uuid"

	"felis.lolicon.best/internal/naming"
)

// The wake allowlist is Postgres policy, distinct from Minecraft's RCON-managed whitelist.

type allowlistWakeRequest struct {
	CanWake *bool `json:"can_wake"`
}

func (a *API) allowlistServer(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return "", false
	}
	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return "", false
	}
	if !a.isOwnerOrAdmin(principalFromContext(r.Context()), rec) {
		writeError(w, r, errForbidden)
		return "", false
	}
	return name, true
}

func (a *API) handleAllowlistList(w http.ResponseWriter, r *http.Request) {
	name, ok := a.allowlistServer(w, r)
	if !ok {
		return
	}
	entries, err := a.Repo.ServerAllowlist(r.Context(), name)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": name, "entries": entries})
}

// handleAllowlistSetWake retains revoked entries so a later join cannot silently restore wake access.
func (a *API) handleAllowlistSetWake(w http.ResponseWriter, r *http.Request) {
	name, ok := a.allowlistServer(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("uuid"))
	if err != nil {
		writeError(w, r, errBadMCUUID)
		return
	}
	var req allowlistWakeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.CanWake == nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "can_wake is required"))
		return
	}
	if err := a.Repo.SetAllowlistWake(r.Context(), name, id.String(), *req.CanWake); err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	p := principalFromContext(r.Context())
	e := AuditEntry{Actor: auditActor(p), Action: "allowlist.revoke", ServerName: name}
	if *req.CanWake {
		e.Action = "allowlist.restore"
	}
	if p != nil {
		e.ActorUserID = p.UserID
	}
	e.Payload = auditPayload(map[string]any{"mc_uuid": id.String()})
	a.auditEntry(r, e)
	w.WriteHeader(http.StatusNoContent)
}
