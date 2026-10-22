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

func (a *API) acquireFileWorld(w http.ResponseWriter, r *http.Request, name, kind, notStopped string) (func(), bool) {
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return nil, false
	}
	if info.Phase != "Running" || info.DesiredState != "Running" {
		return a.acquireWorld(w, r, name, kind, notStopped)
	}
	if err := a.Cluster.CheckFileAccess(r.Context(), name); err != nil {
		a.writeLookupError(w, r, maintenanceError(err, notStopped))
		return nil, false
	}
	return func() {}, true
}
