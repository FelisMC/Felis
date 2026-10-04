package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"felis.lolicon.best/internal/maintenance"
)

// maintenanceError maps the world-volume lock's refusals onto their 409s:
// maintenance_in_progress while a restore, backup, file write or world export
// holds the volume, not_stopped (with the caller's wording) while the server is
// not fully down. Anything else passes through unchanged.
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

// acquireWorld admits one world-volume operation of the given kind
// (internal/maintenance) and returns the release to run once its Job exists, or
// false after writing the refusal. notStopped is the not_stopped wording the
// calling face uses.
//
// The release runs on a context detached from the request: a client that hangs
// up the moment its 202 is written must not leave the lock behind to refuse the
// owner's next wake for maintenance.Grace.
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
