package api

import (
	"context"
	"errors"
	"fmt"
	"log"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
)

// Restore chains persist on snapshot Jobs, keeping the world stopped between snapshot and restore and surviving API restarts.

const (
	ChainSnapshotRunning   = "running"
	ChainSnapshotSucceeded = "succeeded"
	ChainSnapshotFailed    = "failed"
)

type RestoreChain struct {
	Job       string // snapshot backup Job name
	Server    string
	BackupID  string // backup to restore
	BackupRef string
	Snapshot  string
}

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

// chainAbandonMessage preserves unknown reason codes written by newer API versions.
func chainAbandonMessage(reason string) string {
	if s, ok := chainAbandonText[reason]; ok {
		return s
	}
	return reason
}

type RestoreChains interface {
	PendingRestoreChains(ctx context.Context) ([]RestoreChain, error)
	SettleRestoreChain(ctx context.Context, job, state, reason string) error
}

func (a *API) snapshotFirst() (RestoreSnapshotter, bool) {
	if a.RestoreChains == nil || a.Restorer == nil {
		return nil, false
	}
	s, ok := a.Backuper.(RestoreSnapshotter)
	return s, ok
}

// SettleRestoreChains retries restore creation failures and abandons chains only after snapshot failure or invalid state.
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

// settleRestoreChain returns an empty state to keep the chain pending.
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
	// Do not replace the world if a server started despite the pending chain's volume hold.
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
