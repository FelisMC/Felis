package api

import (
	"errors"
	"net/http"
	"strings"

	"felis.lolicon.best/internal/naming"
)

var (
	ErrUnknownAssignee = errors.New("the new owner is not a live account")
	ErrAlreadyOwner    = errors.New("the account already owns the server")
	ErrAssignRetiring  = errors.New("the server is being given up or deleted")
)

// QuotaExcess is one cap an assignment passes; storage is counted in MB.
type QuotaExcess struct {
	Resource string `json:"resource"`
	Limit    int64  `json:"limit"`
	After    int64  `json:"after"`
}

type assignmentView struct {
	Name    string        `json:"name"`
	OwnerID string        `json:"owner_id"`
	Over    []QuotaExcess `json:"over"`
}

// assignTarget refuses system services through the reserved-name check.
func assignTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return "", false
	}
	return name, true
}

func (a *API) writeAssignError(w http.ResponseWriter, r *http.Request, err error, over []QuotaExcess) {
	switch {
	case errors.Is(err, ErrUnknownAssignee):
		writeError(w, r, newError(http.StatusBadRequest, "unknown_user", "the new owner must be an active account"))
	case errors.Is(err, ErrAlreadyOwner):
		writeError(w, r, newError(http.StatusConflict, "already_owner", "the account already owns this server"))
	case errors.Is(err, ErrAssignRetiring):
		writeError(w, r, errServerRetiring)
	case errors.Is(err, ErrQuotaExceeded):
		parts := make([]string, len(over))
		for i, o := range over {
			parts[i] = o.Resource
		}
		writeError(w, r, newError(http.StatusConflict, "quota_exceeded",
			"the server exceeds the new owner's quota (%s); confirm to assign it anyway", strings.Join(parts, ", ")))
	default:
		a.writeLookupError(w, r, err)
	}
}

func (a *API) handleAssignPreview(w http.ResponseWriter, r *http.Request) {
	name, ok := assignTarget(w, r)
	if !ok {
		return
	}
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "user_id is required"))
		return
	}
	res, err := a.serverResources(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	owner, over, err := a.Repo.AssignPreview(r.Context(), name, userID, res)
	if err != nil {
		a.writeAssignError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, assignmentView{Name: name, OwnerID: owner, Over: nonNilExcess(over)})
}

func (a *API) handleAssignServer(w http.ResponseWriter, r *http.Request) {
	name, ok := assignTarget(w, r)
	if !ok {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body struct {
		UserID         string `json:"user_id"`
		AllowOverQuota bool   `json:"allow_over_quota"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.UserID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "user_id is required"))
		return
	}
	res, err := a.claimResources(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	previous, over, err := a.Repo.AssignServer(r.Context(), name, body.UserID, res, body.AllowOverQuota)
	if err != nil {
		a.writeAssignError(w, r, err, over)
		return
	}
	p := principalFromContext(r.Context())
	var from any
	if previous != "" {
		from = previous
	}
	a.auditEntry(r, AuditEntry{Actor: auditActor(p), ActorUserID: p.UserID, Action: "server.assign", ServerName: name,
		Payload: auditPayload(map[string]any{"owner_from": from, "owner_to": body.UserID, "over_quota": nonNilExcess(over)})})
	writeJSON(w, http.StatusOK, assignmentView{Name: name, OwnerID: body.UserID, Over: nonNilExcess(over)})
}

func nonNilExcess(over []QuotaExcess) []QuotaExcess {
	if over == nil {
		return []QuotaExcess{}
	}
	return over
}
