package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// Live backup pauses game saving only for the world copy; plugin databases outside the save system are not transactionally snapshotted.

const liveFreezeCap = 5 * time.Minute

const liveFlushTimeout = 2 * time.Minute

const liveUnrecordedGrace = time.Minute

// LiveBackuper uses an empty reason for manual backups and scheduled or replica for background copies.
type LiveBackuper interface {
	BackupLive(ctx context.Context, serverName, formerOwner, reason string) (string, error)
}

var errLiveBackupUnavailable = newError(http.StatusConflict, "not_stopped",
	"stop the server before backing up its world")

var errLiveBackupConsole = newError(http.StatusServiceUnavailable, "live_backup_console",
	"the server did not respond over its console, so it could not be backed up while running; try again or stop it first")

func (a *API) liveBackupParts() (LiveBackupCluster, LiveBackuper, bool) {
	lc, ok := a.Cluster.(LiveBackupCluster)
	if !ok || a.Console == nil {
		return nil, nil, false
	}
	lb, ok := a.Backuper.(LiveBackuper)
	return lc, lb, ok
}

// startLiveBackup restores saving and releases the world on any startup failure.
func (a *API) startLiveBackup(ctx context.Context, name, formerOwner, reason string) error {
	lc, lb, ok := a.liveBackupParts()
	if !ok {
		return errLiveBackupUnavailable
	}
	f, err := lc.AcquireLiveBackup(ctx, name)
	if err != nil {
		return err
	}
	thaw := func(wasOff bool) {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if !wasOff {
			if _, err := a.Console.RunCommand(cleanup, name, "save-on"); err != nil {
				log.Printf("api: live backup of %s: could not turn saving back on: %v", name, err)
			}
		}
		if err := lc.ClearLiveBackup(cleanup, name); err != nil {
			log.Printf("api: live backup of %s: could not clear the freeze: %v", name, err)
		}
	}
	off, cancel := context.WithTimeout(ctx, 15*time.Second)
	reply, err := a.Console.RunCommand(off, name, "save-off")
	cancel()
	if err != nil {
		thaw(true)
		return errLiveBackupConsole
	}
	wasOff := savingWasOff(reply)
	flush, cancel := context.WithTimeout(ctx, liveFlushTimeout)
	_, err = a.Console.RunCommand(flush, name, "save-all flush")
	cancel()
	if err != nil {
		thaw(wasOff)
		return errLiveBackupConsole
	}
	job, err := lb.BackupLive(ctx, name, formerOwner, reason)
	if err != nil {
		thaw(wasOff)
		return err
	}
	f.Job, f.WasOff = job, wasOff
	if err := lc.SaveLiveBackup(ctx, f); err != nil {
		// Without a persisted Job record recovery cannot thaw safely; thaw immediately and discard the potentially torn copy.
		if derr := lc.DeleteLiveBackupJob(context.Background(), name, job); derr != nil {
			log.Printf("api: live backup of %s: could not drop Job %s: %v", name, job, derr)
		}
		thaw(wasOff)
		return err
	}
	return nil
}

// savingWasOff preserves an owner's existing save-off state when thawing.
func savingWasOff(reply string) bool {
	return strings.Contains(strings.ToLower(reply), "already")
}

// SettleLiveBackups thaws on copy completion, game restart or freeze expiry and discards potentially torn copies.
func (a *API) SettleLiveBackups(ctx context.Context) error {
	lc, ok := a.Cluster.(LiveBackupCluster)
	if !ok || a.Console == nil {
		return nil
	}
	freezes, err := lc.ListLiveBackups(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range freezes {
		if err := a.settleLiveBackup(ctx, lc, f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (a *API) settleLiveBackup(ctx context.Context, lc LiveBackupCluster, f LiveFreeze) error {
	p, err := lc.LiveBackupProgress(ctx, f)
	if err != nil {
		return err
	}
	age := a.now().Sub(f.Started)
	samePod := p.PodUID != "" && p.PodUID == f.PodUID && p.Restarts == f.Restarts
	running := p.JobExists && !p.JobFinished
	switch {
	case f.Job == "":
		// An unrecorded Job may copy after thaw; discard it because the source can change.
		if age < liveUnrecordedGrace {
			return nil
		}
		if err := lc.DeleteLiveBackupJob(ctx, f.Server, ""); err != nil {
			return err
		}
	case !samePod:
		// A restart saves during shutdown, invalidating any copy still in progress.
		if running && !p.StagingDone {
			if err := lc.DeleteLiveBackupJob(ctx, f.Server, f.Job); err != nil {
				return err
			}
		}
	case p.StagingDone || !running:
	case age >= liveFreezeCap:
		log.Printf("api: live backup of %s: the copy took longer than %s; dropping it so the world saves again", f.Server, liveFreezeCap)
		if err := lc.DeleteLiveBackupJob(ctx, f.Server, f.Job); err != nil {
			return err
		}
	default:
		return nil
	}
	if samePod && !f.WasOff {
		on, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := a.Console.RunCommand(on, f.Server, "save-on")
		cancel()
		if err != nil {
			return err
		}
	}
	return lc.ClearLiveBackup(ctx, f.Server)
}
