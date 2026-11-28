package api

import (
	"errors"
	"net/http"

	"felis.lolicon.best/internal/naming"
)

type setProtectionRequest struct {
	Protected bool `json:"protected"`
}

type setCollaboratorRequest struct {
	Role string `json:"role"`
}

type collaboratorsView struct {
	Server        string                  `json:"server"`
	Protected     bool                    `json:"protected"`
	Collaborators []CollaboratorRecord    `json:"collaborators"`
	Candidates    []CollaboratorCandidate `json:"candidates"`
}

func (a *API) collaboratorTarget(w http.ResponseWriter, r *http.Request) (*ServerRecord, bool) {
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
	p := principalFromContext(r.Context())
	if !canManageCollaborators(principalPlatformRole(p), rec.Protected) {
		writeError(w, r, errForbidden)
		return nil, false
	}
	return rec, true
}

func (a *API) handleSetServerProtection(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !a.requireReauth(w, r, p) {
		return
	}
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body setProtectionRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.Repo.SetServerProtected(r.Context(), name, body.Protected); err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	a.auditDetail(r, "server.protection", name, map[string]any{"protected": body.Protected})
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "protected": body.Protected})
}

func (a *API) handleListCollaborators(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.collaboratorTarget(w, r)
	if !ok {
		return
	}
	list, err := a.Repo.ServerCollaborators(r.Context(), rec.Name)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if list == nil {
		list = []CollaboratorRecord{}
	}
	candidates, err := a.Repo.CollaboratorCandidates(r.Context(), 200)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if candidates == nil {
		candidates = []CollaboratorCandidate{}
	}
	writeJSON(w, http.StatusOK, collaboratorsView{
		Server:        rec.Name,
		Protected:     rec.Protected,
		Collaborators: list,
		Candidates:    candidates,
	})
}

func (a *API) handleSetCollaborator(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.collaboratorTarget(w, r)
	if !ok {
		return
	}
	userID := r.PathValue("user_id")
	if userID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "user_id is required"))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body setCollaboratorRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !validCollaboratorRole(body.Role) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "role must be operator or manager"))
		return
	}
	target, err := a.Repo.UserDetail(r.Context(), userID)
	if errors.Is(err, ErrNotFound) || (err == nil && (target.Disabled || target.DeletedAt != nil)) {
		writeError(w, r, newError(http.StatusBadRequest, "unknown_user", "collaborator must be an active account"))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	if target.Role == "owner" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "platform owner already has full access"))
		return
	}
	p := principalFromContext(r.Context())
	saved, err := a.Repo.UpsertServerCollaborator(r.Context(), rec.Name, userID, body.Role, p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	a.auditDetail(r, "server.collaborator.set", rec.Name, map[string]any{"user_id": userID, "role": body.Role})
	writeJSON(w, http.StatusOK, saved)
}

func (a *API) handleRemoveCollaborator(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.collaboratorTarget(w, r)
	if !ok {
		return
	}
	userID := r.PathValue("user_id")
	if userID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "user_id is required"))
		return
	}
	if err := a.Repo.DeleteServerCollaborator(r.Context(), rec.Name, userID); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "collaborator not found"))
			return
		}
		writeError(w, r, err)
		return
	}
	a.auditDetail(r, "server.collaborator.remove", rec.Name, map[string]any{"user_id": userID})
	w.WriteHeader(http.StatusNoContent)
}
