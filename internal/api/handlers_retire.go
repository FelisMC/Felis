package api

import (
	"errors"
	"net/http"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
)

// A server leaves its owner, or the platform, through a retirement: the owner
// gives it up, or an admin asks for it to be deleted. felis-api only records the
// request and stops the server; the reaper, the one component that deletes a
// world, carries it out on its next daily run. It archives the world (a
// "released" backup kept for the reaper's retention, recorded against the owner),
// deletes the world volume and releases the server for someone else to claim, and
// for a deletion also removes the MinecraftServer and marks the servers row
// deleted, which frees the name and subdomain. Until the reaper gets to it the
// owner or an admin can cancel it; the server cannot be woken or claimed in the
// meantime, so the world the reaper archives is the one the owner left.

// retireRequest is the PUT /servers/{name}/retirement body. Confirm must repeat
// the server's name, the same typed confirmation the panel asks for, so a stray
// or replayed call cannot give a world away.
type retireRequest struct {
	Confirm string `json:"confirm"`
	Delete  bool   `json:"delete"`
}

// errServerRetiring refuses a wake or claim of a server with a pending
// retirement.
var errServerRetiring = newError(http.StatusConflict, "server_retiring",
	"this server is being given up or deleted; cancel that first")

// retireServer resolves {name} for the retirement routes and applies their gate:
// 400 for a malformed name, 404 for a server that does not exist, 403 for a caller
// who neither owns it nor is an admin.
func (a *API) retireServer(w http.ResponseWriter, r *http.Request) (*ServerRecord, bool) {
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return nil, false
	}
	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return nil, false
	}
	if !a.isOwnerOrAdmin(principalFromContext(r.Context()), rec) {
		writeError(w, r, errForbidden)
		return nil, false
	}
	return rec, true
}

// handleRetire records a retirement and stops the server. The owner may give the
// server up; deleting it is an admin's call. A system server (reaperExempt, the
// lobby) is never retired: the reaper would not touch it and the request would
// sit forever. A deletion of a server whose MinecraftServer is already gone (one
// removed with kubectl) is accepted, and the reaper then only marks its row.
func (a *API) handleRetire(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.retireServer(w, r)
	if !ok {
		return
	}
	p := principalFromContext(r.Context())
	var req retireRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.Delete && !p.IsAdmin() {
		writeError(w, r, newError(http.StatusForbidden, "forbidden", "only an administrator can delete a server"))
		return
	}
	if req.Confirm != rec.Name {
		writeError(w, r, newError(http.StatusBadRequest, "confirm_mismatch",
			"type the server's name to confirm"))
		return
	}

	info, err := a.Cluster.GetServer(r.Context(), rec.Name)
	switch {
	case errors.Is(err, ErrNotFound) && req.Delete:
		// The StatefulSet retains its claim, so a MinecraftServer removed by hand
		// can leave the world volume behind, and the reaper deletes no world it
		// cannot hold still to archive. That one is an operator's to deal with.
		switch exists, err := a.Cluster.WorldVolumeExists(r.Context(), rec.Name); {
		case err != nil:
			writeError(w, r, err)
			return
		case exists:
			writeError(w, r, newError(http.StatusConflict, "world_volume_orphaned",
				"this server's MinecraftServer is gone but its world volume remains; archive and remove the volume by hand first"))
			return
		}
		info = nil
	case err != nil:
		a.writeLookupError(w, r, err)
		return
	case info.ReaperExempt:
		writeError(w, r, newError(http.StatusConflict, "system_server",
			"a system server cannot be given up or deleted"))
		return
	}
	// Stop first: a failed stop leaves nothing recorded, and a wake that lands
	// after the request is refused. The reaper stops the server again before it
	// touches the world, so one that slips in between costs only time.
	if info != nil && info.DesiredState != string(v1alpha1.DesiredStopped) {
		if err := a.Cluster.SetDesiredState(r.Context(), rec.Name, v1alpha1.DesiredStopped); err != nil {
			a.writeLookupError(w, r, err)
			return
		}
	}
	st, err := a.Repo.RequestRetire(r.Context(), rec.Name, req.Delete)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}

	e := AuditEntry{Actor: auditActor(p), Action: "server.release", ServerName: rec.Name}
	if st.Delete {
		e.Action = "server.delete"
	}
	if p != nil {
		e.ActorUserID = p.UserID
	}
	if rec.OwnerID != "" {
		e.Payload = auditPayload(map[string]any{"owner_id": rec.OwnerID})
	}
	a.auditEntry(r, e)
	writeJSON(w, http.StatusAccepted, map[string]any{"name": rec.Name, "retiring": st})
}

// handleCancelRetire drops a pending retirement. The owner may take back giving
// the server up, but a deletion an admin asked for is the admin's to cancel. The
// server stays stopped; its owner starts it again when they want it.
func (a *API) handleCancelRetire(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.retireServer(w, r)
	if !ok {
		return
	}
	p := principalFromContext(r.Context())
	pending := rec.Retire != nil
	if err := a.Repo.CancelRetire(r.Context(), rec.Name, p.IsAdmin()); err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusForbidden, "forbidden",
				"only an administrator can cancel the deletion of a server"))
			return
		}
		a.writeLookupError(w, r, err)
		return
	}
	if pending {
		a.audit(r, "server.retire_cancel", rec.Name)
	}
	w.WriteHeader(http.StatusNoContent)
}
