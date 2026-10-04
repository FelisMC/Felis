package api

import (
	"context"
	"net/http"

	"felis.lolicon.best/internal/naming"
)

// System services have no player owner or business-layer row. Staff manage their
// existing cluster objects; creation and claiming keep the reserved-name gate.
func validateManagedServerName(r *http.Request, name string) error {
	if p := principalFromContext(r.Context()); naming.IsSystemServer(name) && p != nil && p.IsAdmin() {
		return naming.ValidateSystemServerName(name)
	}
	return naming.ValidateServerName(name)
}

func (a *API) managedServerRecord(ctx context.Context, name string) (*ServerRecord, error) {
	if !naming.IsSystemServer(name) {
		return a.Repo.ServerByName(ctx, name)
	}
	info, err := a.Cluster.GetServer(ctx, name)
	if err != nil {
		return nil, err
	}
	return &ServerRecord{Name: name, Subdomain: info.Subdomain}, nil
}
