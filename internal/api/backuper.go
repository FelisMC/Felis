package api

import "context"

// Backuper starts an on-demand world backup (spec §18/§19 WorldArchiver, run on
// demand rather than on the reaper's daily schedule) — the "back up before I touch
// it" lever behind POST /api/v1/servers/{name}/backup. Like Restorer it only STARTS
// the work: tarring the world PVC into the archive store is pod-filesystem work
// felis-api cannot do in-process — the world PVC is RWO and owned by the operator's
// StatefulSet, so the API has nothing to mount at request time. The production
// implementation therefore hands off to a backup Job (internal/backupjob), which
// self-records the world_backups row like the reaper. The call returns once the
// backup is enqueued, so the handler answers 202 (backing_up).
//
// formerOwner is the current owner recorded on the backup row so it can later be
// restored (empty when an admin backs up an unowned server). It returns ErrNotFound
// if the server is unknown to the execution backend; any other error maps to 500.
//
// It is an interface so the handler is tested against a fake; the production executor
// (internal/backupjob.Backuper) is integration-only, and until it is wired the
// API.Backuper is nil so POST /servers/{name}/backup reports 503 — the backup
// authorization boundary is exercised without shipping a stub that cannot run.
type Backuper interface {
	Backup(ctx context.Context, serverName, formerOwner string) error
}

// RestoreSnapshotter is the Backuper's safety-snapshot lever: it enqueues a backup
// of the world as it is now, labelled with the restore to run once that backup
// has succeeded (backupID, backupRef). RestoreChains settles it: it starts the
// restore after a successful snapshot and gives the restore up after a failed
// one, so a restore never overwrites a world that has no copy. Until it is
// settled the snapshot holds the world volume as a restore (internal/maintenance).
type RestoreSnapshotter interface {
	BackupThenRestore(ctx context.Context, serverName, formerOwner, backupID, backupRef string) error
}
