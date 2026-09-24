package api

import (
	"context"
	"errors"
	"fmt"
	"log"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
)

// A restore overwrites the world, so by default it starts with a safety snapshot:
// POST /servers/{name}/restore-backup enqueues a backup Job carrying the restore
// (RestoreSnapshotter), and SettleRestoreChains starts that restore once the
// snapshot has succeeded. The snapshot is an ordinary pre_restore row in the
// backup list, which is the way back from a wrong pick.
//
// The chain lives on the backup Job (labels and annotations), so a felis-api
// restart picks it up where it was. While it is pending the Job holds the world
// volume as a restore, which keeps the server down in the gap between the two
// Jobs; if felis-api never settles it, the lock goes with the Job at its TTL and
// the world is left as it was.

// Snapshot Job states a RestoreChain reports.
const (
	ChainSnapshotRunning   = "running"
	ChainSnapshotSucceeded = "succeeded"
	ChainSnapshotFailed    = "failed"
)

// RestoreChain is one safety snapshot whose restore is still pending.
type RestoreChain struct {
	Job       string // the snapshot's backup Job
	Server    string
	BackupID  string // the backup the restore extracts
	BackupRef string
	Snapshot  string // ChainSnapshotRunning / Succeeded / Failed
}

// Why a chain was abandoned: the code recorded on the snapshot Job and reported
// by the jobs route as then_restore_reason, for the panel to word in its own
// language. chainAbandonText is the English the log and the job's message carry.
const (
	ChainAbandonSnapshotFailed = "snapshot_failed"
	ChainAbandonNotConfigured  = "not_configured"
	ChainAbandonServerGone     = "server_gone"
	ChainAbandonServerStarted  = "server_started"
	ChainAbandonRestoreBusy    = "restore_busy"
)

var chainAbandonText = map[string]string{
	ChainAbandonSnapshotFailed: "the safety backup failed, so the world was left as it was",
	ChainAbandonNotConfigured:  "restore is not configured",
	ChainAbandonServerGone:     "the server no longer exists",
	ChainAbandonServerStarted:  "the server was started before the restore could run",
	ChainAbandonRestoreBusy:    "another restore is running on this world",
}

// chainAbandonMessage words a reason code; an unknown code (a newer felis-api
// wrote it) is shown as is.
func chainAbandonMessage(reason string) string {
	if s, ok := chainAbandonText[reason]; ok {
		return s
	}
	return reason
}

// RestoreChains reads the pending chains and records how each was settled
// (maintenance.ThenRestoreStarted or ThenRestoreAbandoned, with the reason code
// of a chain given up).
type RestoreChains interface {
	PendingRestoreChains(ctx context.Context) ([]RestoreChain, error)
	SettleRestoreChain(ctx context.Context, job, state, reason string) error
}

// snapshotFirst reports whether a restore can start with a safety snapshot:
// the Backuper can carry a restore and something settles the chain.
func (a *API) snapshotFirst() (RestoreSnapshotter, bool) {
	if a.RestoreChains == nil || a.Restorer == nil {
		return nil, false
	}
	s, ok := a.Backuper.(RestoreSnapshotter)
	return s, ok
}

// SettleRestoreChains advances every pending chain once: it starts the restore
// behind a snapshot that succeeded and gives up the one behind a snapshot that
// failed. A chain whose restore could not be created this time stays pending and
// is retried on the next call. cmd/felis runs it on a short interval.
func (a *API) SettleRestoreChains(ctx context.Context) error {
	if a.RestoreChains == nil {
		return nil
	}
	chains, err := a.RestoreChains.PendingRestoreChains(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range chains {
		state, reason, err := a.settleRestoreChain(ctx, c)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Job, err))
			continue
		}
		if state == "" {
			continue
		}
		if err := a.RestoreChains.SettleRestoreChain(ctx, c.Job, state, reason); err != nil {
			errs = append(errs, fmt.Errorf("%s: settle as %s: %w", c.Job, state, err))
			continue
		}
		if state == maintenance.ThenRestoreStarted {
			log.Printf("api: safety snapshot %s done; restoring %s from backup %s", c.Job, c.Server, c.BackupID)
		} else {
			log.Printf("api: restore of %s from backup %s abandoned: %s", c.Server, c.BackupID, chainAbandonMessage(reason))
		}
	}
	return errors.Join(errs...)
}

// settleRestoreChain decides one chain: the state to record ("" to leave it
// pending) and, for an abandoned chain, the reason code.
func (a *API) settleRestoreChain(ctx context.Context, c RestoreChain) (string, string, error) {
	switch c.Snapshot {
	case ChainSnapshotFailed:
		return maintenance.ThenRestoreAbandoned, ChainAbandonSnapshotFailed, nil
	case ChainSnapshotSucceeded:
	default:
		return "", "", nil
	}
	if a.Restorer == nil {
		return maintenance.ThenRestoreAbandoned, ChainAbandonNotConfigured, nil
	}
	// The pending chain holds the world volume, so nothing should have started
	// the server; a start that got past it anyway must not have its world
	// replaced underneath.
	info, err := a.Cluster.GetServer(ctx, c.Server)
	if errors.Is(err, ErrNotFound) {
		return maintenance.ThenRestoreAbandoned, ChainAbandonServerGone, nil
	}
	if err != nil {
		return "", "", err
	}
	if info.Ready || info.DesiredState != string(v1alpha1.DesiredStopped) {
		return maintenance.ThenRestoreAbandoned, ChainAbandonServerStarted, nil
	}
	if err := a.Restorer.Restore(ctx, c.Server, c.BackupRef); err != nil {
		if isRestoreInProgress(err) {
			return maintenance.ThenRestoreAbandoned, ChainAbandonRestoreBusy, nil
		}
		if errors.Is(err, ErrNotFound) {
			return maintenance.ThenRestoreAbandoned, ChainAbandonServerGone, nil
		}
		return "", "", err
	}
	return maintenance.ThenRestoreStarted, "", nil
}
