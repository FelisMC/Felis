package api

import (
	"context"
	"errors"
)

// Restorer returns after enqueueing; ErrNotFound means the server is absent and RestoreInProgress marks an existing restore.
type Restorer interface {
	Restore(ctx context.Context, serverName, backupRef string) error
}

// isRestoreInProgress matches by method so executors do not need to import api.
func isRestoreInProgress(err error) bool {
	var busy interface{ RestoreInProgress() bool }
	return errors.As(err, &busy) && busy.RestoreInProgress()
}
