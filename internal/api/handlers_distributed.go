package api

import (
	"context"
	"errors"
	"net/http"

	"felis.lolicon.best/internal/distributed"
	"felis.lolicon.best/internal/naming"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type Distribution interface {
	Nodes(context.Context) ([]distributed.Node, error)
	ValidateNode(context.Context, string) error
	BeginMigration(context.Context, string, string, string) (distributed.Operation, error)
	Migration(context.Context, string, string) (distributed.Operation, error)
	RetryMigration(context.Context, string, string) (distributed.Operation, error)
}

func (a *API) distributedReady(w http.ResponseWriter, r *http.Request) bool {
	if a.Distribution == nil {
		writeError(w, r, newError(503, "distributed_unavailable", "distributed deployment is not configured"))
		return false
	}
	return true
}

func (a *API) handleNodes(w http.ResponseWriter, r *http.Request) {
	if !a.distributedReady(w, r) {
		return
	}
	nodes, err := a.Distribution.Nodes(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (a *API) handleMigration(w http.ResponseWriter, r *http.Request) {
	if !a.distributedReady(w, r) {
		return
	}
	server := r.PathValue("name")
	if err := naming.ValidateServerName(server); err != nil {
		writeError(w, r, newError(400, "bad_name", "%v", err))
		return
	}
	rec, err := a.Repo.ServerByName(r.Context(), server)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if rec.Retire != nil {
		writeError(w, r, errServerRetiring)
		return
	}
	var body struct {
		TargetNode string `json:"targetNode"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.Distribution.ValidateNode(r.Context(), body.TargetNode); err != nil {
		writeError(w, r, newError(400, "bad_node", "%v", err))
		return
	}
	op, err := a.Distribution.BeginMigration(r.Context(), server, body.TargetNode, rec.OwnerID)
	if err != nil {
		a.writeMigrationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op)
}

func (a *API) handleMigrationStatus(w http.ResponseWriter, r *http.Request) {
	if !a.distributedReady(w, r) {
		return
	}
	op, err := a.Distribution.Migration(r.Context(), r.PathValue("name"), r.PathValue("id"))
	if err != nil {
		a.writeMigrationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (a *API) handleMigrationRetry(w http.ResponseWriter, r *http.Request) {
	if !a.distributedReady(w, r) {
		return
	}
	rec, err := a.Repo.ServerByName(r.Context(), r.PathValue("name"))
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if rec.Retire != nil {
		writeError(w, r, errServerRetiring)
		return
	}
	op, err := a.Distribution.RetryMigration(r.Context(), r.PathValue("name"), r.PathValue("id"))
	if err != nil {
		a.writeMigrationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op)
}

func (a *API) writeMigrationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, distributed.ErrBusy):
		writeError(w, r, newError(409, "migration_busy", "%v", err))
	case errors.Is(err, distributed.ErrNotFound), apierrors.IsNotFound(err):
		writeError(w, r, newError(404, "not_found", "%v", err))
	case apierrors.IsConflict(err):
		writeError(w, r, newError(409, "conflict", "retry after refreshing migration status"))
	default:
		writeError(w, r, err)
	}
}
