package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/maintenance"
)

// Scheduled backups use separate retention and keep limits so they cannot replace the owner's manual backups.

type ScheduledBackuper interface {
	BackupScheduled(ctx context.Context, serverName, formerOwner string) error
}

type ScheduledCandidate struct {
	Name    string
	OwnerID string
}

// ScheduleStore prioritizes worlds with no scheduled backup, then the longest waiting, and only includes worlds played since the last intact backup.
type ScheduleStore interface {
	ScheduledBackupCandidates(ctx context.Context, before time.Time) ([]ScheduledCandidate, error)
}

type WorldJobCounter interface {
	RunningWorldJobs(ctx context.Context) (int, error)
}

// Use a separate audit action so scheduled backups do not consume the owner's manual cooldown.
const (
	scheduledBackupActor  = "scheduler"
	scheduledBackupAction = "backup.scheduled"
)

// BackupScheduler starts at most one backup per tick and waits for all running backup or restore Jobs.
type BackupScheduler struct {
	API   *API
	Store ScheduleStore
	Jobs  WorldJobCounter
	// Every<=0 disables scheduled backups.
	Every time.Duration

	// tried delays failed or missing-volume worlds by Every/4 instead of retrying each tick.
	tried map[string]time.Time
	full  bool
}

func (s *BackupScheduler) Tick(ctx context.Context) error {
	b, ok := s.API.Backuper.(ScheduledBackuper)
	every := s.Every
	if s.API.ArchivePolicy != nil {
		every = s.API.ArchivePolicy.Reaper(ctx).ScheduledEvery
	}
	if !ok || every <= 0 {
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

	// Let the reaper evict from a full store instead of displacing another owner's backup with a scheduled one.
	if limit := s.API.backupStoreCap(ctx); limit > 0 {
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

	due, err := s.Store.ScheduledBackupCandidates(ctx, now.Add(-every))
	if err != nil {
		return fmt.Errorf("list the worlds due a scheduled backup: %w", err)
	}
	if s.tried == nil {
		s.tried = map[string]time.Time{}
	}
	for name, at := range s.tried {
		if now.Sub(at) >= every {
			delete(s.tried, name)
		}
	}
	for _, c := range due {
		if at, ok := s.tried[c.Name]; ok && now.Sub(at) < every/4 {
			continue
		}
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
		case errors.Is(err, ErrNotStopped):
			if started, err := s.startLive(ctx, c, now); started || err != nil {
				return err
			}
			continue
		case errors.As(err, &busy), errors.Is(err, ErrMaintenanceInProgress):
			continue
		case errors.Is(err, ErrNotFound):
			s.tried[c.Name] = now
			continue
		case err != nil:
			return fmt.Errorf("lock the world of %s: %w", c.Name, err)
		}
		err = b.BackupScheduled(ctx, c.Name, c.OwnerID)
		// The Job holds the volume after creation; the annotation protects only the admission gap.
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

// startLive retries console failures after a quarter interval; transitional, unsupported or busy servers wait for later ticks.
func (s *BackupScheduler) startLive(ctx context.Context, c ScheduledCandidate, now time.Time) (bool, error) {
	var busy *MaintenanceBusyError
	switch err := s.API.startLiveBackup(ctx, c.Name, c.OwnerID, backupjob.ReasonScheduled); {
	case err == nil:
	case errors.Is(err, ErrNotRunning), errors.As(err, &busy), errors.Is(err, ErrMaintenanceInProgress), errors.Is(err, errLiveBackupUnavailable):
		return false, nil
	case errors.Is(err, errLiveBackupConsole), errors.Is(err, ErrNotFound):
		s.tried[c.Name] = now
		return false, nil
	default:
		s.tried[c.Name] = now
		return false, fmt.Errorf("start the live scheduled backup of %s: %w", c.Name, err)
	}
	s.tried[c.Name] = now
	log.Printf("api: started the scheduled backup of %s while it runs", c.Name)
	s.API.writeAudit(ctx, AuditEntry{
		Actor: scheduledBackupActor, Source: scheduledBackupActor,
		Action: scheduledBackupAction, ServerName: c.Name, Payload: []byte(`{"live":true}`),
	})
	return true, nil
}
