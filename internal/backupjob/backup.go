// Package backupjob enqueues world-backup Jobs that archive data and record their database rows.
package backupjob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/placement"
)

// ErrAlreadyExists is treated as a successful enqueue on a Job-name collision.
var ErrAlreadyExists = errors.New("backup: job already exists")

type Jobs interface {
	// CreateBackupJob returns ErrAlreadyExists on a name collision.
	CreateBackupJob(ctx context.Context, p JobParams) error
}

// Config requires Image and BackupPVC; callers leave the executor unavailable when either is missing.
type Config struct {
	ResolveWorld placement.Resolver
	Namespace    string
	// ServiceAccount needs no Kubernetes API grants; database access comes from the mounted config Secret.
	ServiceAccount string
	Image          string
	BackupPVC      string
	// ConfigSecret includes the database URL required to record the backup row.
	ConfigSecret string
	ConfigMount  string
	// BackupRoot must equal archive.local_path because stored archive references are absolute.
	BackupRoot string
	WorldsRoot string
	Deadline   time.Duration
	CPULimit   string
	MemLimit   string
	// Root with DAC_OVERRIDE reads private game-owned files; omit zero FSGroup to avoid changing volume ownership.
	RunAsUser  int64
	RunAsGroup int64
	FSGroup    int64
	// StageLimit empty leaves live-backup scratch capacity uncapped; the copy still checks available space.
	StageLimit       string
	TTLAfterFinished time.Duration
}

const (
	defaultNamespace      = "minecraft"
	defaultServiceAccount = "felis-restore"
	defaultConfigSecret   = "felis-config"
	defaultConfigMount    = "/etc/felis"
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
	if c.ConfigSecret == "" {
		c.ConfigSecret = defaultConfigSecret
	}
	if c.ConfigMount == "" {
		c.ConfigMount = defaultConfigMount
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

type Backuper struct {
	Jobs   Jobs
	Config Config
}

// Backup returns after enqueueing with a fresh Job name so completed Jobs awaiting TTL cleanup cannot absorb a new request.
func (b *Backuper) Backup(ctx context.Context, serverName, formerOwner string) error {
	p := b.jobParams(serverName, formerOwner)
	if b.Config.ResolveWorld != nil {
		w, err := b.Config.ResolveWorld(ctx, serverName)
		if err != nil {
			return err
		}
		p.WorldPVC = w.Claim
	}
	if err := b.Jobs.CreateBackupJob(ctx, p); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return nil
		}
		return err
	}
	return nil
}

// BackupThenRestore holds the world through a safety snapshot; the API starts restoration only after that snapshot succeeds.
func (b *Backuper) BackupThenRestore(ctx context.Context, serverName, formerOwner, backupID, backupRef string) error {
	p := b.jobParams(serverName, formerOwner)
	if b.Config.ResolveWorld != nil {
		w, err := b.Config.ResolveWorld(ctx, serverName)
		if err != nil {
			return err
		}
		p.WorldPVC = w.Claim
	}
	p.RestoreRef, p.RestoreBackupID = backupRef, backupID
	if err := b.Jobs.CreateBackupJob(ctx, p); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return nil
		}
		return err
	}
	return nil
}

// BackupScheduled uses separate keep limits so scheduled points do not displace manual backups.
func (b *Backuper) BackupScheduled(ctx context.Context, serverName, formerOwner string) error {
	p := b.jobParams(serverName, formerOwner)
	if b.Config.ResolveWorld != nil {
		w, err := b.Config.ResolveWorld(ctx, serverName)
		if err != nil {
			return err
		}
		p.WorldPVC = w.Claim
	}
	p.Scheduled = true
	if err := b.Jobs.CreateBackupJob(ctx, p); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return nil
		}
		return err
	}
	return nil
}

// BackupLive copies the frozen world first and returns the Job name; the API restores saving after the copy exits.
func (b *Backuper) BackupLive(ctx context.Context, serverName, formerOwner, reason string) (string, error) {
	p := b.jobParams(serverName, formerOwner)
	if b.Config.ResolveWorld != nil {
		w, err := b.Config.ResolveWorld(ctx, serverName)
		if err != nil {
			return "", err
		}
		p.WorldPVC = w.Claim
	}
	p.Live, p.StageLimit = true, b.Config.StageLimit
	p.Scheduled, p.Replica = reason == ReasonScheduled, reason == ReasonReplica
	if err := b.Jobs.CreateBackupJob(ctx, p); err != nil {
		return "", err
	}
	return p.JobName, nil
}

func jobNameSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("backupjob: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func (b *Backuper) jobParams(serverName, formerOwner string) JobParams {
	cfg := b.Config.withDefaults()
	return JobParams{
		Server:           serverName,
		JobName:          BackupJobName(serverName) + "-" + jobNameSuffix(),
		FormerOwner:      formerOwner,
		WorldPVC:         naming.WorldPVCName(serverName),
		BackupPVC:        cfg.BackupPVC,
		Namespace:        cfg.Namespace,
		ServiceAccount:   cfg.ServiceAccount,
		Image:            cfg.Image,
		ConfigSecret:     cfg.ConfigSecret,
		ConfigMount:      cfg.ConfigMount,
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
