package api

import (
	"net/http"

	"github.com/google/uuid"

	"felis.lolicon.best/internal/naming"
)

// The wake allowlist (server_allowlist) decides who may wake a sleeping server
// whose autostartPolicy is allowlist. A player lands on it by joining the server
// once (RecordJoin); these two routes let the owner, or an admin, see who is on it
// and take a player's wake right away or give it back. It is Felis's own record in
// Postgres, so it answers whether the server is running or not, unlike the
// Minecraft whitelist under /access, which is the game server's and needs RCON.

// allowlistWakeRequest is the PUT /servers/{name}/allowlist/{uuid} body.
type allowlistWakeRequest struct {
	CanWake *bool `json:"can_wake"`
}

// allowlistServer resolves {name} for the allowlist routes and applies their
// gate: 400 for a malformed name, 404 for a server that does not exist, 403 for a
// caller who neither owns it nor is an admin.
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

// handleAllowlistList returns the server's wake allowlist, newest first,
// including the players whose wake right was taken away.
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

// handleAllowlistSetWake takes a player's wake right away (can_wake false) or
// gives it back (true). The entry stays on the list either way, so a revoked
// player's next join does not quietly undo the owner's choice.
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
