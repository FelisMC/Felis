// Package restore implements the world-restore executor (spec §7
// POST /servers/{name}/restore-backup, spec §466: a former owner who re-claims a
// released server within the retention window restores their archived world).
//
// felis-api cannot restore a world in-process: the world PVC is RWO and owned by
// the operator's StatefulSet, so the API has nothing to mount at request time
// (see internal/api.Restorer). This package is the production executor it hands
// off to — a one-shot Kubernetes Job in the minecraft namespace that mounts the
// target world PVC and the backup store, then runs `felis restore` (cmd/felis)
// to extract the archive into the world volume.
//
// Trust model, mirroring internal/build's weak-SA isolation (spec §16, §21, §22):
// the restore Pod runs under a deliberately weak service account with its token
// auto-mount disabled, so it cannot reach the K8s API; it is handed ONLY the two
// PVCs and the archive parameters as plain flags, never a database URL or any
// Secret — the four-power red line that a build/restore Pod must not touch the
// felis database or the K8s API. felis-api owns the database and the
// authorization decision (handlers_backups.go); this Pod only moves bytes from
// the backup PVC onto the world PVC. Every isolation guarantee lives in the pure
// jobspec (jobspec.go) and is asserted by jobspec_test.go, because no cluster
// runs in this environment.
//
// The Restorer depends on the Jobs interface, so the orchestration (idempotent
// enqueue, error mapping) is unit-tested against an in-memory fake; the
// controller-runtime implementation (k8sjobs.go) compiles here but is exercised
// only by integration tests against a live cluster.
package restore

import (
	"context"
	"errors"
	"time"

	"felis.lolicon.best/internal/naming"
)

// ErrAlreadyExists is returned by a Jobs implementation when a restore Job for a
// server already exists (a restore is already in flight). The Restorer treats it
// as success — see Restore.
var ErrAlreadyExists = errors.New("restore: job already exists")

// Jobs is the cluster-side restore lifecycle the Restorer depends on. It is an
// interface so the orchestration is tested against a fake; the controller-runtime
// implementation (K8sJobs) is integration-tested only — it requires a live
// cluster. The restore Job is one-shot and self-cleaning (TTL), so unlike the
// build subsystem there is no phase-polling or cancel seam: kicking it off is the
// whole contract, exactly matching the asynchronous 202 the handler answers.
type Jobs interface {
	// CreateRestoreJob renders and applies the restore Job for p. It returns
	// ErrAlreadyExists if a Job of the same (deterministic) name already exists.
	CreateRestoreJob(ctx context.Context, p JobParams) error
}

// Config parameterises the restore executor. Deployment-specific values that
// have no safe default — the felis Image to run and the BackupPVC to mount — are
// supplied by the caller (cmd/felis sources them from the environment); when
// either is empty the caller leaves the API's Restorer nil so the endpoint
// reports 503 rather than enqueuing a Job that cannot run.
type Config struct {
	// Namespace is where the world PVCs live and the restore Job runs (the
	// minecraft namespace). The Job is intentionally co-located with the world it
	// restores; it never runs in the felis control-plane namespace.
	Namespace string
	// ServiceAccount is the weak SA the restore Pod runs as. Like felis-build it
	// MUST NOT be the felis-api SA and has no Role/RoleBinding anywhere.
	ServiceAccount string
	// Image is the felis binary image; the Job runs `felis restore` from it.
	Image string
	// ArchiveStore selects the backup backend. Only "tarLocal" is implemented in
	// this build, mirroring the reaper (cmd/felis buildArchiver).
	ArchiveStore string
	// BackupPVC is the name of the backup PVC the archives live on. It must be
	// RWX so the reaper and concurrent restores can mount it (a helm-slice
	// contract); restore mounts it read-only.
	BackupPVC string
	// BackupRoot is the in-Pod mount path of BackupPVC. It MUST equal the path
	// the reaper wrote archives under (cfg.Archive.LocalPath), because tarLocal
	// archive refs are absolute paths — mounting the PVC anywhere else would make
	// the stored ref unresolvable inside the Pod.
	BackupRoot string
	// WorldsRoot is the in-Pod mount path of the world PVC the archive extracts
	// into.
	WorldsRoot string
	// Deadline caps the restore Pod's wall-clock (activeDeadlineSeconds).
	Deadline time.Duration
	// CPULimit / MemLimit cap the restore container.
	CPULimit string
	MemLimit string
	// RunAsUser / RunAsGroup / FSGroup are the Pod's runtime identity. FSGroup in
	// particular MUST match the operator StatefulSet's runtime group so the files
	// the restore Pod writes are readable by the minecraft server that later
	// mounts the same world PVC. The default matches the conventional minecraft
	// container uid; a deployment that runs minecraft as another id overrides it.
	RunAsUser  int64
	RunAsGroup int64
	FSGroup    int64
	// TTLAfterFinished is how long a finished restore Job lingers before the Job
	// controller garbage-collects it. There is no cancel path, so the TTL is the
	// only cleanup; it also bounds the window in which a re-restore sees a stale
	// completed Job as ErrAlreadyExists.
	TTLAfterFinished time.Duration
}

// defaults applied when a Config field is left zero. Image and BackupPVC have no
// default on purpose — see Config.
const (
	defaultNamespace      = "minecraft"
	defaultServiceAccount = "felis-restore"
	defaultArchiveStore   = "tarLocal"
	defaultBackupRoot     = "/backups"
	defaultWorldsRoot     = "/world"
	defaultDeadline       = 30 * time.Minute
	defaultCPULimit       = "1"
	defaultMemLimit       = "1Gi"
	defaultRunAsID        = int64(1000)
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
	if c.RunAsUser == 0 {
		c.RunAsUser = defaultRunAsID
	}
	if c.RunAsGroup == 0 {
		c.RunAsGroup = defaultRunAsID
	}
	if c.FSGroup == 0 {
		c.FSGroup = defaultRunAsID
	}
	if c.TTLAfterFinished <= 0 {
		c.TTLAfterFinished = defaultTTL
	}
	return c
}

// Restorer is the production internal/api.Restorer (the compile-time proof of
// that is in internal/api's test, which imports this package; this package never
// imports api). It holds no mutable state.
type Restorer struct {
	Jobs   Jobs
	Config Config
}

// Restore enqueues a restore Job that extracts the archive at backupRef into
// serverName's world PVC. It returns once the Job is created — the extraction
// runs in the Pod — so the handler's 202 ("restoring") is honest.
//
// It is idempotent: a duplicate enqueue while a restore Job for this server is
// still running is treated as success rather than surfaced as an error.
//
// The coalescing key is the Job name (RestoreJobName), which depends only on the
// server, NOT on backupRef — so a second request that arrives while one is in
// flight is absorbed regardless of the ref it carries, and if the two refs
// differ the second is silently dropped (the in-flight restore wins). That is
// acceptable here: restore runs only for a Stopped server (handler gate ⑥) and
// the handler always passes the latest backup, which for a stopped server does
// not change, so concurrent requests carry the same ref in practice.
//
// A FINISHED Job — succeeded or failed — does not absorb the next request: its
// deterministic name is replaced so the retry enqueues for real (see
// K8sJobs.CreateRestoreJob). Distinguishing in-flight from finished is what
// keeps the handler's 202 honest in both directions — not a 500 for a genuine
// duplicate, and not a false "restoring" for a retry after a failure.
func (r *Restorer) Restore(ctx context.Context, serverName, backupRef string) error {
	if err := r.Jobs.CreateRestoreJob(ctx, r.jobParams(serverName, backupRef)); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			return nil // already enqueued — idempotent
		}
		return err
	}
	return nil
}

// jobParams projects the server, archive ref, and config onto the inputs
// jobspec.go renders. The world PVC name is derived from the single shared
// naming convention (naming.WorldPVCName), the same one the operator created it
// under and the reaper deletes it by.
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
