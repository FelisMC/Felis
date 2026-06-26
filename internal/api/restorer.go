package api

import "context"

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
// It returns ErrNotFound if the server is unknown to the execution backend; any
// other error is an internal failure (the handler maps it to 500).
//
// It is an interface so the handler is tested against a fake (api_test.go). The
// production executor — a restore Job mirroring internal/build's jobspec + weak-SA
// isolation — is integration-only and is a deliberate follow-up: until it is wired
// the API.Restorer is nil and POST /servers/{name}/restore-backup reports 503, so
// the restore authorization boundary is exercised without shipping a stub that
// cannot run in the real cluster topology.
type Restorer interface {
	Restore(ctx context.Context, serverName, backupRef string) error
}
