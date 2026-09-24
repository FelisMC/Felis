// Package backupjob implements the on-demand world-backup executor (spec §18/§19
// WorldArchiver, run on demand rather than on the reaper's daily schedule). It is
// the "back up before I touch it" lever behind POST /api/v1/servers/{name}/backup:
// an owner or admin stops a server, then snapshots its world into the archive store
// as a first-class world_backups row — restorable by internal/restore and expired
// by the reaper's retention pass, so it never leaks as an orphan archive.
//
// felis-api cannot archive a world in-process: the world PVC is RWO and owned by
// the operator's StatefulSet, so the API has nothing to mount at request time (the
// same constraint that makes internal/restore a Job). This package is the executor
// it hands off to — a one-shot Kubernetes Job in the minecraft namespace that
// mounts the target world PVC (read-only) and the backup PVC (read-write), then
// runs `felis backup` (cmd/felis) to tar the world into the archive store AND
// record the world_backups row.
//
// Trust model. The backup Pod mirrors internal/restore's weak-SA isolation (a weak
// SA with its token un-mounted, so it cannot reach the K8s API) with ONE deliberate
// departure, reviewed in jobspec.go: it DOES mount the felis config Secret so it can
// self-record its backup row atomically with the archive, exactly like the reaper —
// the only other component holding both a world mount and the database. A restore
// Pod must stay DB-blind because it processes a poisoned archive; a backup Pod only
// reads a world the operator already owns and tars it, so that threat does not apply.
//
// The Backuper depends on the Jobs interface, so the orchestration (idempotent
// enqueue, error mapping) is unit-tested against an in-memory fake; the
// controller-runtime implementation (k8sjobs.go) compiles here but is exercised only
// by integration tests against a live cluster.
package backupjob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"felis.lolicon.best/internal/naming"
)

// ErrAlreadyExists is returned by a Jobs implementation when a backup Job for a
// server already exists (a backup is already in flight). The Backuper treats it as
// success — see Backup.
var ErrAlreadyExists = errors.New("backup: job already exists")

// Jobs is the cluster-side backup lifecycle the Backuper depends on. It is an
// interface so the orchestration is tested against a fake; the controller-runtime
// implementation (K8sJobs) is integration-tested only — it requires a live cluster.
type Jobs interface {
	// CreateBackupJob renders and applies the backup Job for p. It returns
	// ErrAlreadyExists if a Job of the same (deterministic) name already exists.
	CreateBackupJob(ctx context.Context, p JobParams) error
}

// Config parameterises the backup executor. Deployment-specific values that have no
// safe default — the felis Image to run and the BackupPVC to mount — are supplied
// by the caller (cmd/felis sources them from the environment); when either is empty
// the caller leaves the API's Backuper nil so the endpoint reports 503 rather than
// enqueuing a Job that cannot run.
type Config struct {
	// Namespace is where the world PVCs live and the backup Job runs (the minecraft
	// namespace), co-located with the world it snapshots.
	Namespace string
	// ServiceAccount is the weak SA the backup Pod runs as. It reuses felis-restore
	// (bare, no Role/RoleBinding): a backup Pod needs no K8s API access, only
	// filesystem access to the two PVCs and — via the mounted config Secret, not the
	// SA — the database.
	ServiceAccount string
	// Image is the felis binary image; the Job runs `felis backup` from it.
	Image string
	// BackupPVC is the name of the backup PVC the archive is written into (the same
	// PVC the reaper writes to and restore reads from).
	BackupPVC string
	// ConfigSecret is the felis config Secret (felis.toml, carrying the DB URL) the
	// backup Pod mounts to self-record its world_backups row. Defaults to the name
	// the control-plane manifests use.
	ConfigSecret string
	// ConfigMount is the in-Pod mount path of ConfigSecret (holds felis.toml).
	ConfigMount string
	// BackupRoot is the in-Pod mount path of BackupPVC. It MUST equal cfg.Archive.
	// LocalPath — the path the reaper wrote archives under and restore mounts to
	// resolve them — because tarLocal archive refs are absolute.
	BackupRoot string
	// WorldsRoot is the in-Pod mount path of the world PVC being archived.
	WorldsRoot string
	// Deadline caps the backup Pod's wall-clock (activeDeadlineSeconds).
	Deadline time.Duration
	// CPULimit / MemLimit cap the backup container.
	CPULimit string
	MemLimit string
	// RunAsUser / RunAsGroup / FSGroup are the Pod's runtime identity. They default
	// to ROOT (0:0): the world volume is written by the game uid (naming.GameUID),
	// or by root in a world an older release wrote, and Paper saves files no other
	// non-root uid can read — level.dat is written mode 0600 (tar walk: permission
	// denied, verified live). DAC_OVERRIDE on the container reads them whichever
	// uid owns them. Set 0/0/0 explicitly for root; FSGroup is omitted when zero.
	RunAsUser  int64
	RunAsGroup int64
	FSGroup    int64
	// TTLAfterFinished is how long a finished backup Job lingers before the Job
	// controller garbage-collects it; it also bounds the window in which a re-backup
	// sees a stale completed Job as ErrAlreadyExists.
	TTLAfterFinished time.Duration
}

// defaults applied when a Config field is left zero. Image and BackupPVC have no
// default on purpose — see Config.
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

// withDefaults returns a copy of c with zero fields filled, so a partially
// configured Config (or the zero value, in tests) is always usable.
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

// Backuper is the production internal/api.Backuper (the compile-time proof of that
// is in internal/api's wire test, which imports this package; this package never
// imports api). It holds no mutable state.
type Backuper struct {
	Jobs   Jobs
	Config Config
}

// Backup enqueues a backup Job that tars serverName's world into the archive store
// and records the world_backups row. formerOwner is stamped on that row so the owner
// can later restore it (empty for an admin backing up an unowned server). It returns
// once the Job is created — the archive runs in the Pod — so the handler's 202
// ("backing_up") is honest.
//
// Each call gets a fresh, unique Job name (BackupJobName + random suffix), so an
// on-demand backup requested again after a previous one — the console "立即备份"
// repeat-tap case — always produces a new archive rather than colliding with a
// just-finished Job still inside its TTL window. ErrAlreadyExists is kept only as a
// defensive no-op against the astronomically unlikely suffix collision.
//
// Unique names mean two truly simultaneous taps can schedule two backup
// Pods; both mount the world PVC read-only so neither corrupts anything, and if they
// land on different nodes the RWO attach fails one cleanly. Add single-flight-on-
// running only if a real double-tap storm ever shows up.
func (b *Backuper) Backup(ctx context.Context, serverName, formerOwner string) error {
	if err := b.Jobs.CreateBackupJob(ctx, b.jobParams(serverName, formerOwner)); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return nil // suffix collision — treat as enqueued
		}
		return err
	}
	return nil
}

// BackupThenRestore enqueues the safety snapshot in front of a restore: a backup
// Job like Backup's, recorded as a pre_restore backup and labelled with the
// restore to run once it succeeds (backupID, backupRef). felis-api creates that
// restore Job when the snapshot finishes and gives it up if the snapshot fails,
// so the world is never overwritten without a way back; the snapshot Job holds
// the world volume as a restore until then (internal/maintenance).
func (b *Backuper) BackupThenRestore(ctx context.Context, serverName, formerOwner, backupID, backupRef string) error {
	p := b.jobParams(serverName, formerOwner)
	p.RestoreRef, p.RestoreBackupID = backupRef, backupID
	if err := b.Jobs.CreateBackupJob(ctx, p); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return nil // suffix collision — treat as enqueued
		}
		return err
	}
	return nil
}

// jobNameSuffix is a short random hex tag that makes each backup Job name unique.
// 32 bits is ample: collisions only matter within a single Job's TTL window across
// a handful of manual backups.
func jobNameSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand only fails if the OS RNG is gone — unrecoverable.
		panic("backupjob: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// jobParams projects the server, former owner, and config onto the inputs jobspec.go
// renders. The world PVC name is derived from the single shared naming convention
// (naming.WorldPVCName), the same one the operator created it under.
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
