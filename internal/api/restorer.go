package api

import (
	"context"
	"errors"
)

// Restorer starts a world restore from a stored backup (spec §466: former_owner
// 3mo 内重新 claim → restore PVC). Restore only STARTS the work: recreating the
// per-server world PVC and extracting the archive into it is pod-filesystem work
// felis-api cannot do in-process — the world PVC is RWO and its lifecycle is owned
// by the operator's StatefulSet, so the API process has nothing to mount at
// request time. The production implementation therefore hands off to a restore
// Job, exactly the way internal/build hands an image build to a Kaniko Job. The
// call returns once the restore is enqueued, so the handler answers 202
// (restoring), never claiming the world is already back.
//
// It returns ErrNotFound if the server is unknown to the execution backend, and
// an error whose RestoreInProgress method reports true when a restore of another
// backup is still running on the world (409 restore_in_progress; see
// isRestoreInProgress). Any other error is an internal failure (500).
//
// It is an interface so the handler is tested against a fake (api_test.go). The
// production executor is internal/restore's weak-SA restore Job; when cmd/felis
// cannot configure it the API.Restorer is nil and POST
// /servers/{name}/restore-backup reports 503.
type Restorer interface {
	Restore(ctx context.Context, serverName, backupRef string) error
}

// isRestoreInProgress reports whether err says another restore is still running
// on the world. The executor cannot import this package, so its error is matched
// by method rather than by value.
func isRestoreInProgress(err error) bool {
	var busy interface{ RestoreInProgress() bool }
	return errors.As(err, &busy) && busy.RestoreInProgress()
}
