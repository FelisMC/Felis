package api

import (
	"context"
	"errors"
	"net/http"

	"felis.lolicon.best/internal/nodecontrol"
)

type NodeControl interface {
	List(context.Context) ([]nodecontrol.Task, error)
	Get(context.Context, string) (nodecontrol.Task, error)
	Start(context.Context, nodecontrol.Request, string) (nodecontrol.Task, error)
}

func (a *API) handleNodeTasks(w http.ResponseWriter, r *http.Request) {
	if a.NodeControl == nil {
		writeJSON(w, 200, map[string]any{"available": false, "tasks": []nodecontrol.Task{}})
		return
	}
	tasks, err := a.NodeControl.List(r.Context())
	if err != nil {
		a.writeNodeControlError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"available": true, "tasks": tasks})
}
func (a *API) handleNodeTask(w http.ResponseWriter, r *http.Request) {
	if !a.nodeControlReady(w, r) {
		return
	}
	task, err := a.NodeControl.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeNodeControlError(w, r, err)
		return
	}
	writeJSON(w, 200, task)
}
func (a *API) handleStartNodeTask(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) || !a.nodeControlReady(w, r) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req nodecontrol.Request
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	a.startNodeTask(w, r, req)
}
func (a *API) handleRetryNodeTask(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) || !a.nodeControlReady(w, r) {
		return
	}
	task, err := a.NodeControl.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeNodeControlError(w, r, err)
		return
	}
	if task.State != "failed" {
		writeError(w, r, newError(409, "conflict", "only failed node tasks can be retried"))
		return
	}
	a.startNodeTask(w, r, task.Request)
}
func (a *API) startNodeTask(w http.ResponseWriter, r *http.Request, req nodecontrol.Request) {
	if err := req.Validate(); err != nil {
		writeError(w, r, newError(400, "bad_request", "%v", err))
		return
	}
	task, err := a.NodeControl.Start(r.Context(), req, principalFromContext(r.Context()).UserID)
	if err != nil {
		a.writeNodeControlError(w, r, err)
		return
	}
	a.audit(r, "platform.node."+req.Action, task.ID)
	writeJSON(w, http.StatusAccepted, task)
}
func (a *API) nodeControlReady(w http.ResponseWriter, r *http.Request) bool {
	if a.NodeControl == nil {
		a.writeNodeControlError(w, r, errors.New("node-control not configured"))
		return false
	}
	return true
}
func (a *API) writeNodeControlError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, nodecontrol.ErrBusy):
		writeError(w, r, newError(409, "node_operation_busy", "a node operation is already running"))
	case errors.Is(err, nodecontrol.ErrNotFound):
		writeError(w, r, newError(404, "not_found", "node task not found"))
	default:
		writeError(w, r, newError(503, "node_control_unavailable", "host node service is unavailable; inspect felis-node-control.service"))
	}
}

// Fail closed for starts when the configured host service cannot report its maintenance state.
func NodeMaintenanceGuard(control NodeControl) func(context.Context) error {
	return func(ctx context.Context) error {
		tasks, err := control.List(ctx)
		if err != nil {
			return newError(503, "node_control_unavailable", "host node service is unavailable")
		}
		for _, task := range tasks {
			if task.State == "running" {
				return newError(409, "node_operation_busy", "node maintenance is in progress")
			}
		}
		return nil
	}
}
