package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"felis.lolicon.best/internal/maintenance"
)

func maintenanceError(err error, notStopped string) error {
	var busy *MaintenanceBusyError
	switch {
	case errors.As(err, &busy):
		return newError(http.StatusConflict, "maintenance_in_progress",
			"%s is running on this server's world; retry once it finishes", maintenanceLabel(busy.Kind))
	case errors.Is(err, ErrMaintenanceInProgress):
		return newError(http.StatusConflict, "maintenance_in_progress",
			"another operation is running on this server's world; retry once it finishes")
	case errors.Is(err, ErrNotStopped):
		return newError(http.StatusConflict, "not_stopped", "%s", notStopped)
	}
	return err
}

func maintenanceLabel(kind string) string {
	switch kind {
	case maintenance.KindRestore:
		return "a restore"
	case maintenance.KindBackup:
		return "a backup"
	case maintenance.KindFileWrite:
		return "a file write"
	case maintenance.KindConfigWrite:
		return "a settings save"
	case maintenance.KindExport:
		return "a world export or file download"
	case maintenance.KindReap:
		return "the idle-world reaper"
	}
	return "another operation"
}

// acquireWorld returns a release callback detached from the request so client disconnects cannot strand the admission lock.
func (a *API) acquireWorld(w http.ResponseWriter, r *http.Request, name, kind, notStopped string) (func(), bool) {
	if err := a.Cluster.AcquireMaintenance(r.Context(), name, kind); err != nil {
		a.writeLookupError(w, r, maintenanceError(err, notStopped))
		return nil, false
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()
		if err := a.Cluster.ReleaseMaintenance(ctx, name); err != nil {
			log.Printf("api: release the maintenance lock on %s: %v (it lapses after %s)", name, err, maintenance.Grace)
		}
	}, true
}

type authServerKey struct{}

type authServerSnapshot struct {
	ownerID   string
	claimedAt *time.Time
}

func withAuthorizedServer(ctx context.Context, rec *ServerRecord) context.Context {
	var claimedAt *time.Time
	if rec.ClaimedAt != nil {
		t := *rec.ClaimedAt
		claimedAt = &t
	}
	return context.WithValue(ctx, authServerKey{}, authServerSnapshot{ownerID: rec.OwnerID, claimedAt: claimedAt})
}

func sameClaimedAt(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func (a *API) recheckAuthorizedServer(w http.ResponseWriter, r *http.Request, name string) (*ServerRecord, bool) {
	p := principalFromContext(r.Context())
	cur, err := a.managedServerRecord(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return nil, false
	}
	if !a.isOwnerOrAdmin(p, cur) {
		writeError(w, r, errForbidden)
		return nil, false
	}
	if snap, ok := r.Context().Value(authServerKey{}).(authServerSnapshot); ok && (p == nil || !p.IsAdmin()) {
		if cur.OwnerID != snap.ownerID || !sameClaimedAt(cur.ClaimedAt, snap.claimedAt) {
			writeError(w, r, errForbidden)
			return nil, false
		}
	}
	return cur, true
}

func (a *API) acquireFileWorld(w http.ResponseWriter, r *http.Request, name, kind, notStopped string) (func(), bool) {
	if _, ok := a.recheckAuthorizedServer(w, r, name); !ok {
		return nil, false
	}
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return nil, false
	}
	if info.Phase != "Running" || info.DesiredState != "Running" {
		release, ok := a.acquireWorld(w, r, name, kind, notStopped)
		if !ok {
			return nil, false
		}
		if _, ok := a.recheckAuthorizedServer(w, r, name); !ok {
			release()
			return nil, false
		}
		return release, true
	}
	if err := a.Cluster.CheckFileAccess(r.Context(), name); err != nil {
		a.writeLookupError(w, r, maintenanceError(err, notStopped))
		return nil, false
	}
	if _, ok := a.recheckAuthorizedServer(w, r, name); !ok {
		return nil, false
	}
	return func() {}, true
}
