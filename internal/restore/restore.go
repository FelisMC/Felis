// Package restore enqueues credential-blind Jobs that extract stored archives into world volumes.
package restore

import (
	"context"
	"errors"
	"time"

	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/placement"
)

// ErrAlreadyExists coalesces an unfinished restore of the same archive.
var ErrAlreadyExists = errors.New("restore: job already exists")

// ErrOtherRestoreRunning means a different archive is already restoring; this request did not take effect.
var ErrOtherRestoreRunning error = otherRestoreRunning{}

type otherRestoreRunning struct{}

func (otherRestoreRunning) Error() string {
	return "restore: a restore of another backup is still running"
}

// RestoreInProgress exposes the conflict without requiring api to import the executor package.
func (otherRestoreRunning) RestoreInProgress() bool { return true }

type Jobs interface {
	// CreateRestoreJob coalesces the same unfinished archive, rejects a different one and replaces finished Jobs.
	CreateRestoreJob(ctx context.Context, p JobParams) error
}

// Config requires Image and BackupPVC; callers leave the executor unavailable when either is missing.
type Config struct {
	ResolveWorld placement.Resolver
	Namespace    string
	// ServiceAccount must be the unbound weak restore identity, never the API account.
	ServiceAccount string
	Image          string
	ArchiveStore   string
	// BackupPVC is mounted read-only and must support the deployment's concurrent archive readers and writers.
	BackupPVC string
	// BackupRoot must equal archive.local_path so stored absolute archive references resolve inside the Job.
	BackupRoot string
	WorldsRoot string
	Deadline   time.Duration
	CPULimit   string
	MemLimit   string
	// Root with DAC_OVERRIDE replaces private game files; prepare-data restores game ownership before the next start.
	RunAsUser        int64
	RunAsGroup       int64
	FSGroup          int64
	TTLAfterFinished time.Duration
}

const (
	defaultNamespace      = "minecraft"
	defaultServiceAccount = "felis-restore"
	defaultArchiveStore   = "tarLocal"
	defaultBackupRoot     = "/backups"
	defaultWorldsRoot     = "/world"
	defaultDeadline       = 30 * time.Minute
	defaultCPULimit       = "1"
	defaultMemLimit       = "1Gi"
	defaultTTL            = 10 * time.Minute
)

func (c Config) withDefaults() Config {
	if c.Namespace == "" {
		c.Namespace = defaultNamespace
	}
	if c.ServiceAccount == "" {
		c.ServiceAccount = defaultServiceAccount
	}
	if c.ArchiveStore == "" {
		c.ArchiveStore = defaultArchiveStore
	}
	if c.BackupRoot == "" {
		c.BackupRoot = defaultBackupRoot
	}
	if c.WorldsRoot == "" {
		c.WorldsRoot = defaultWorldsRoot
	}
	if c.Deadline <= 0 {
		c.Deadline = defaultDeadline
	}
	if c.CPULimit == "" {
		c.CPULimit = defaultCPULimit
	}
	if c.MemLimit == "" {
		c.MemLimit = defaultMemLimit
	}
	if c.TTLAfterFinished <= 0 {
		c.TTLAfterFinished = defaultTTL
	}
	return c
}

type Restorer struct {
	Jobs   Jobs
	Config Config
}

// Restore coalesces the same running archive and rejects a different one; finished Jobs are replaced so retries enqueue actual work.
func (r *Restorer) Restore(ctx context.Context, serverName, backupRef string) error {
	p := r.jobParams(serverName, backupRef)
	if r.Config.ResolveWorld != nil {
		w, err := r.Config.ResolveWorld(ctx, serverName)
		if err != nil {
			return err
		}
		p.WorldPVC = w.Claim
	}
	if err := r.Jobs.CreateRestoreJob(ctx, p); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return nil
		}
		return err
	}
	return nil
}

func (r *Restorer) jobParams(serverName, backupRef string) JobParams {
	cfg := r.Config.withDefaults()
	return JobParams{
		Server:           serverName,
		WorldPVC:         naming.WorldPVCName(serverName),
		BackupPVC:        cfg.BackupPVC,
		BackupRef:        backupRef,
		ArchiveStore:     cfg.ArchiveStore,
		Namespace:        cfg.Namespace,
		ServiceAccount:   cfg.ServiceAccount,
		Image:            cfg.Image,
		BackupRoot:       cfg.BackupRoot,
		WorldsRoot:       cfg.WorldsRoot,
		Deadline:         cfg.Deadline,
		CPULimit:         cfg.CPULimit,
		MemLimit:         cfg.MemLimit,
		RunAsUser:        cfg.RunAsUser,
		RunAsGroup:       cfg.RunAsGroup,
		FSGroup:          cfg.FSGroup,
		TTLAfterFinished: cfg.TTLAfterFinished,
	}
}
