package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// Live restore points: a backup of a server that keeps running, after the snapshot mode
// of Proxmox VE, with the game's own save system standing in for a filesystem snapshot.
//
//  1. felis-api admits the backup (LiveBackupCluster.AcquireLiveBackup) and records the
//     freeze on the server, so a restart of felis-api still finishes it.
//  2. Over RCON it turns saving off (save-off) and writes everything in memory to disk
//     (save-all flush): the world on disk is complete and stays still.
//  3. The backup Job's init container copies the world into a scratch volume on the
//     same node (felis stage-world; a reflink where the filesystem has them).
//  4. The moment that copy step exits, settleLiveBackups turns saving back on
//     (save-on). The archive, its checksum and its read-back then run from the copy
//     while the game plays on.
//
// So players see saving paused only for the copy, seconds for a few GB. What the freeze
// costs: a crash during it loses what changed since the flush, as any crash between two
// autosaves does, and chunks stay loaded meanwhile. A game that restarted mid-copy has
// saved on its way down, so its copy is dropped. Plugin databases written outside the
// save system (SQLite, H2) are copied as they are, not as of the flush.

// liveFreezeCap is the longest a game's saving stays off: a copy still running then is
// given up, and saving comes back on.
const liveFreezeCap = 5 * time.Minute

// liveFlushTimeout bounds save-all flush on a large world.
const liveFlushTimeout = 2 * time.Minute

// liveUnrecordedGrace is how long a freeze may go without its Job recorded before the
// settle loop treats felis-api as having died in between.
const liveUnrecordedGrace = time.Minute

// LiveBackuper enqueues the backup Job of a running server and returns its name
// (backupjob.Backuper.BackupLive). reason is the world_backups reason: empty for an
// owner's own backup, backupjob.ReasonScheduled or backupjob.ReasonReplica.
type LiveBackuper interface {
	BackupLive(ctx context.Context, serverName, formerOwner, reason string) (string, error)
}

// errLiveBackupUnavailable answers when this install cannot back up a running server:
// no console, no live Backuper, or a cluster without the support.
var errLiveBackupUnavailable = newError(http.StatusConflict, "not_stopped",
	"stop the server before backing up its world")

// errLiveBackupConsole answers when the game did not take the freeze over RCON.
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

// startLiveBackup freezes a running server's world and enqueues the Job copying it. On
// any failure saving is back on and nothing holds the world.
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
		// Without the record nothing would turn saving back on after the copy; thaw now
		// and drop the Job, whose copy could then catch the game writing.
		if derr := lc.DeleteLiveBackupJob(context.Background(), name, job); derr != nil {
			log.Printf("api: live backup of %s: could not drop Job %s: %v", name, job, derr)
		}
		thaw(wasOff)
		return err
	}
	return nil
}

// savingWasOff reads save-off's reply: "Saving is already turned off" names a world an
// owner had already stopped saving, which thawing must leave as it was.
func savingWasOff(reply string) bool {
	return strings.Contains(strings.ToLower(reply), "already")
}

// SettleLiveBackups turns saving back on for each frozen world whose copy is done, whose
// game restarted, or whose freeze has lasted liveFreezeCap; a copy that may be torn is
// dropped. cmd/felis runs it every couple of seconds.
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
		// felis-api died between creating the Job and recording it, or the record is
		// unreadable: whatever Job exists copies an unfrozen world after the thaw.
		if age < liveUnrecordedGrace {
			return nil
		}
		if err := lc.DeleteLiveBackupJob(ctx, f.Server, ""); err != nil {
			return err
		}
	case !samePod:
		// The game restarted: it saved on its way down, so a copy still going is torn.
		if running && !p.StagingDone {
			if err := lc.DeleteLiveBackupJob(ctx, f.Server, f.Job); err != nil {
				return err
			}
		}
	case p.StagingDone || !running:
		// The copy is done, or the Job ended without finishing it: the world may move.
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
			// Keep the freeze: the next pass tries again.
			return err
		}
	}
	return lc.ClearLiveBackup(ctx, f.Server)
}
