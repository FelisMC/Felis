package api

import (
	"errors"
	"net/http"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
)

// Retirement records intent and stops the server; the reaper later archives and releases or deletes it.

// retireRequest requires the server name as typed confirmation.
type retireRequest struct {
	Confirm string `json:"confirm"`
	Delete  bool   `json:"delete"`
}

var errServerRetiring = newError(http.StatusConflict, "server_retiring",
	"this server is being given up or deleted; cancel that first")

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

// handleRetire refuses reaper-exempt system servers because the reaper cannot complete their retirement.
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
		// A manually deleted CR can leave its PVC behind; archiving that unguarded volume requires operator intervention.
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
	// Stop before recording retirement so failed stops leave no pending request; the reaper repeats the stop before archival.
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

// handleCancelRetire leaves the server stopped; only staff can cancel a deletion request.
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
