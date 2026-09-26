package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"felis.lolicon.best/internal/maintenance"
)

// Scheduled backups. A world played every day never idles long enough for the
// reaper to archive it, and an owner's own backups are only the ones they
// remembered to take, so felis-api takes a daily restore point of every world
// played since its last one. The backup Job mounts the world volume, which a
// running server holds, so the point is taken once the server is stopped: idle
// auto-stop brings a played world down minutes after its last player leaves,
// and the point lands the same day. A server kept running around the clock gets
// one the next time it stops.
//
// The point is an ordinary world_backups row of reason scheduled: listed and
// restorable by its owner, copied offsite like any other, pruned to [archive]
// scheduled_keep per owner and expired after scheduled_retention, so it never
// takes the place of a backup the owner asked for.

// ScheduledBackuper enqueues the backup Job of a scheduled restore point
// (backupjob.Backuper).
type ScheduledBackuper interface {
	BackupScheduled(ctx context.Context, serverName, formerOwner string) error
}

// ScheduledCandidate is an owned world due a scheduled backup.
type ScheduledCandidate struct {
	Name    string
	OwnerID string
}

// ScheduleStore lists the worlds due a scheduled backup: owned, joined since
// their owner's newest intact scheduled backup, and without one taken after
// before. Worlds never given one come first, then the longest waiting.
type ScheduleStore interface {
	ScheduledBackupCandidates(ctx context.Context, before time.Time) ([]ScheduledCandidate, error)
}

// WorldJobCounter counts the backup and restore Jobs still running.
type WorldJobCounter interface {
	RunningWorldJobs(ctx context.Context) (int, error)
}

// Audit identity of a scheduled backup. The action is its own so the owner's
// manual cooldown (LastBackupRequest, backup.create) never counts it.
const (
	scheduledBackupActor  = "scheduler"
	scheduledBackupAction = "backup.scheduled"
)

// BackupScheduler takes the scheduled backups. Tick launches at most one
// backup Job and only while no backup or restore Job is running, so the points
// queue behind each other and behind the owners' own operations instead of
// loading the node with archives all at once.
type BackupScheduler struct {
	API   *API
	Store ScheduleStore
	Jobs  WorldJobCounter
	// Every is how often a played world gets a point ([archive]
	// scheduled_every). Zero or less turns the scheduler off.
	Every time.Duration

	// tried is when each world last had a Job launched or was found without a
	// volume, so one whose backup keeps failing is retried every Every/4
	// instead of every tick.
	tried map[string]time.Time
	// full remembers that the store was at max_local_bytes, so the pause and
	// the resume are logged once each.
	full bool
}

// Tick launches the backup of the world waiting longest, if any may run now.
func (s *BackupScheduler) Tick(ctx context.Context) error {
	b, ok := s.API.Backuper.(ScheduledBackuper)
	if !ok || s.Every <= 0 {
		return nil
	}
	now := s.API.now()

	running, err := s.Jobs.RunningWorldJobs(ctx)
	if err != nil {
		return fmt.Errorf("count running backup and restore jobs: %w", err)
	}
	if running > 0 {
		return nil
	}

	// A full store is the reaper's to evict; a scheduled point added now would
	// only push out an older backup of somebody else's.
	if limit := s.API.BackupStoreCap; limit > 0 {
		used, err := s.API.Repo.BackupStoreBytes(ctx)
		if err != nil {
			return fmt.Errorf("read the backup store size: %w", err)
		}
		full := used >= limit
		if full != s.full {
			if full {
				log.Printf("api: scheduled backups paused: the backup store holds %d of its %d bytes ([archive] max_local_bytes)", used, limit)
			} else {
				log.Printf("api: scheduled backups resumed: the backup store is below [archive] max_local_bytes")
			}
			s.full = full
		}
		if full {
			return nil
		}
	}

	due, err := s.Store.ScheduledBackupCandidates(ctx, now.Add(-s.Every))
	if err != nil {
		return fmt.Errorf("list the worlds due a scheduled backup: %w", err)
	}
	if s.tried == nil {
		s.tried = map[string]time.Time{}
	}
	for name, at := range s.tried {
		if now.Sub(at) >= s.Every {
			delete(s.tried, name)
		}
	}
	for _, c := range due {
		if at, ok := s.tried[c.Name]; ok && now.Sub(at) < s.Every/4 {
			continue
		}
		// A server never started has no world yet, and one whose volume is gone
		// has nothing left to save; the Job would sit Pending on the claim.
		exists, err := s.API.Cluster.WorldVolumeExists(ctx, c.Name)
		if err != nil {
			return fmt.Errorf("look up the world volume of %s: %w", c.Name, err)
		}
		if !exists {
			s.tried[c.Name] = now
			continue
		}
		var busy *MaintenanceBusyError
		switch err := s.API.Cluster.AcquireMaintenance(ctx, c.Name, maintenance.KindBackup); {
		case errors.Is(err, ErrNotStopped), errors.As(err, &busy), errors.Is(err, ErrMaintenanceInProgress):
			continue // running, or somebody else has the world: next tick
		case errors.Is(err, ErrNotFound):
			s.tried[c.Name] = now
			continue
		case err != nil:
			return fmt.Errorf("lock the world of %s: %w", c.Name, err)
		}
		err = b.BackupScheduled(ctx, c.Name, c.OwnerID)
		// Once the Job exists it holds the world; the annotation only covered
		// the gap.
		if rerr := s.API.Cluster.ReleaseMaintenance(context.WithoutCancel(ctx), c.Name); rerr != nil {
			log.Printf("api: release the maintenance lock on %s: %v (it lapses after %s)", c.Name, rerr, maintenance.Grace)
		}
		s.tried[c.Name] = now
		if err != nil {
			return fmt.Errorf("start the scheduled backup of %s: %w", c.Name, err)
		}
		log.Printf("api: started the scheduled backup of %s", c.Name)
		s.API.writeAudit(ctx, AuditEntry{
			Actor: scheduledBackupActor, Source: scheduledBackupActor,
			Action: scheduledBackupAction, ServerName: c.Name,
		})
		return nil
	}
	return nil
}
