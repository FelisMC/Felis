// Package reaper implements the world reaper / three-clock retention batch
// (spec §18). It is the only component that deletes a player's world data, so
// every step is ordered and gated to honor §18's six red lines:
//
//	① system-server exemption — spec.reaperExempt servers are never touched
//	   (losing the lobby would be total ingress loss).
//	② delete PVC ≠ delete the servers row — the row stays so the subdomain
//	   remains reserved and a re-claim yields the same-named empty world.
//	③ world_backups is NOT FK'd to servers — a backup must outlive the world
//	   it came from (3-month retention from deletion).
//	④ back up BEFORE deleting — the PVC is only deleted after the archive is
//	   both written and recorded; an archive failure preserves the world.
//	⑤ warnings are best-effort — a delivery failure never blocks a reap, and a
//	   server with no linked owner is still reaped on time.
//	⑥ a real join resets the clock — RecordJoin (spec §7) refreshes
//	   last_active_at and clears warned_*, so renewal restarts the countdown.
//
// The logic here is pure and hermetically testable: all I/O is behind the
// Store, Cluster, and Warner interfaces plus the backup.WorldArchiver, with an
// injectable clock and id generator. The Postgres and Kubernetes bindings live
// in pgstore.go / k8scluster.go and are integration-tested, not unit-tested.
package reaper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/metrics"
)

// Day is a calendar day; the retention windows in §18 are expressed in days.
const Day = 24 * time.Hour

// ReasonInactive is the canonical world_backups.reason for an idle-reaped world
// (spec §18). It is a stable label, not a literal restatement of the deadline.
const ReasonInactive = "inactive_15d"

// Audit actions emitted by the reaper. The actor/source are a system identity
// ("reaper") because no human Access email is in play here (spec §14).
const (
	ActionReapWorld   = "reap_world"
	ActionEvictBackup = "evict_backup_early"
)

// ErrNotFound is returned by Cluster.Inspect when the MinecraftServer CRD for a
// servers row no longer exists. The reaper treats it as "skip" — it will not
// delete world data it cannot first inspect for the exemption flag.
var ErrNotFound = errors.New("reaper: server not found")

// ErrNotQuiet is returned by Cluster.HoldWorld while the server is not fully
// down yet, or another operation (a restore, backup or file write) holds its
// world. The world is left alone and the server is tried again on the next run.
var ErrNotQuiet = errors.New("reaper: world not quiet")

// errStoreFull is an internal sentinel: the backup store is at capacity and
// could not be freed, so the world is preserved rather than deleted without a
// backup (red line ④). It is never returned to callers.
var errStoreFull = errors.New("reaper: backup store full, world preserved")

// Tier identifies which warning column a notice corresponds to. The tiers are
// positional: Tier3d is the first configured WarnBefore offset (the earlier,
// larger one) and maps to servers.warned_3d_at; Tier1d is the second and maps
// to warned_1d_at. The names follow the schema columns, not fixed durations —
// the actual thresholds are derived from Config.IdleBeforeReap.
type Tier int

const (
	Tier3d Tier = iota
	Tier1d
)

// Config holds the retention windows. The warning thresholds are derived from
// the deadline (threshold = IdleBeforeReap - offset) rather than hardcoded, so
// changing IdleBeforeReap moves the warnings with it and the toml's
// warn_before=["3d","1d"] maps 1:1 onto WarnBefore.
type Config struct {
	IdleBeforeReap time.Duration   // §18: reap after this much per-server idle (default 15d)
	WarnBefore     []time.Duration // §24: warn these long before the deadline (default 3d, 1d)
	Retention      time.Duration   // §18: keep a backup this long after deletion (default 3mo≈90d)
	MaxLocalBytes  int64           // §26: backup store soft cap; 0 = unlimited
	// ManualRetention is how long an owner's on-demand backup is kept; it is
	// a restore point for a world that still exists, so it goes sooner than a
	// reaped world's only archive (default 30d).
	ManualRetention time.Duration
	// ManualKeep caps the on-demand backups kept per server; the backup Job
	// removes the oldest beyond it (default 5).
	ManualKeep int
	// ManualCooldown is the shortest gap between two owner-requested backups
	// of one server (default 10m); operators are not held to it.
	ManualCooldown time.Duration
	// ScheduledEvery is the spacing of the restore points felis-api takes of
	// a world that has been played since its last one (default 1d; 0 turns
	// them off). ScheduledKeep caps how many a server keeps (default 7), and
	// ScheduledRetention is how long each is kept (default 90d, the reaper's
	// retention): the newest ones are all a quiet world has.
	ScheduledEvery     time.Duration
	ScheduledKeep      int
	ScheduledRetention time.Duration
	// RequireOffsite holds each deletion until the world's archive has its
	// off-site copy ([offsite] configured; internal/offsite records the copy).
	// The archive is written on the run that finds the world idle, and the
	// world is deleted on the first run after the copy lands, normally the
	// next day.
	RequireOffsite bool
	// VerifyEvery is how often each stored archive is read back in full, and
	// VerifyPerRun caps how many one run reads (the ones checked longest ago
	// first), so bit rot in a backup is found before a restore needs it.
	VerifyEvery  time.Duration
	VerifyPerRun int
	// PartialAfter is how old an unfinished archive file must be before it is
	// swept: longer than any archive takes to write.
	PartialAfter time.Duration
}

// DefaultConfig is the spec's §24 default window set.
func DefaultConfig() Config {
	return Config{
		IdleBeforeReap: 15 * Day,
		WarnBefore:     []time.Duration{3 * Day, 1 * Day},
		Retention:      90 * Day,
		MaxLocalBytes:  0,

		ManualRetention: 30 * Day,
		ManualKeep:      5,
		ManualCooldown:  10 * time.Minute,

		ScheduledEvery:     Day,
		ScheduledKeep:      7,
		ScheduledRetention: 90 * Day,

		VerifyEvery:  7 * Day,
		VerifyPerRun: 10,
		PartialAfter: 6 * time.Hour,
	}
}

// Candidate is a servers row in the reaper's view (only deleted_at IS NULL rows
// are listed). Activity is per-server: LastActiveAt = max(last human join,
// created_at); stop/idle/wake do NOT move it (spec §18).
type Candidate struct {
	Name         string
	OwnerID      string // "" when unowned — still reaped, but never warned (red line ⑤)
	LastActiveAt time.Time
	Warned3dAt   time.Time // zero = not yet sent
	Warned1dAt   time.Time // zero = not yet sent
}

func (c Candidate) warnedAt(t Tier) time.Time {
	if t == Tier1d {
		return c.Warned1dAt
	}
	return c.Warned3dAt
}

// ServerCRD is the slice of the MinecraftServer CRD the reaper needs: the
// exemption flag (red line ①) and the world PVC to archive then delete.
type ServerCRD struct {
	Exempt bool
	PVC    string
}

// BackupRecord is a world_backups insert. FormerOwner is captured so the
// backup, which outlives the server row, still records who it belonged to
// (red line ③). It is NOT a foreign key.
type BackupRecord struct {
	ID          string
	ServerName  string
	FormerOwner string
	BackupRef   string
	SizeBytes   int64
	Reason      string
	ExpiresAt   time.Time
	// SHA256 is the archive's digest as written ("" when the backend keeps none)
	// and SkippedEntries the world entries it could not hold.
	SHA256         string
	SkippedEntries int
}

// Fresh is the backup FreshBackup found.
type Fresh struct {
	ID     string
	Ref    string
	SHA256 string
	// Offsite reports that the archive has its off-site copy (offsite_at).
	Offsite bool
}

// StoredBackup is an existing world_backups row, used by both the expiry pass
// and the capacity-eviction path.
type StoredBackup struct {
	ID         string
	ServerName string
	BackupRef  string
	SizeBytes  int64
	Reason     string
	SHA256     string // "" when none was recorded
}

// AuditRecord is a reaper-sourced audit_logs entry. The PG binding fills
// actor="reaper", source="reaper", and puts FormerOwner into the payload jsonb
// (audit_logs has no former_owner column).
type AuditRecord struct {
	Action      string
	ServerName  string
	FormerOwner string
}

// Store is the business-layer (Postgres) face the reaper needs. It deliberately
// exposes only the narrow operations §18 performs, never a generic UPDATE.
type Store interface {
	// ListActiveServers returns every servers row with deleted_at IS NULL.
	ListActiveServers(ctx context.Context) ([]Candidate, error)

	// FreshBackup reports an existing present reaper archive (reason
	// inactive_15d) for server whose world is still current — created at or
	// after since (the world's last_active_at) and after the current claim,
	// preferring one already copied off-site, and never one found corrupt. It
	// makes a reap idempotent across a DeletePVC failure, and across the wait for
	// the off-site copy: the retry reuses the archive instead of writing a
	// duplicate.
	FreshBackup(ctx context.Context, server string, since time.Time) (b Fresh, ok bool, err error)

	// InsertBackup records a world_backups row (status=present).
	InsertBackup(ctx context.Context, rec BackupRecord) error

	// ReleaseWorld is the post-delete business mutation: owner_id→NULL,
	// last_active_at→at (clock reset), warned_*→NULL. It does NOT delete the
	// row (red line ②).
	ReleaseWorld(ctx context.Context, name string, at time.Time) error

	// RestartClock sets last_active_at→at and clears warned_* on a server with
	// no world to reclaim, so it is not found idle again every run.
	RestartClock(ctx context.Context, name string, at time.Time) error

	// MarkWarned stamps the warned_3d_at / warned_1d_at column for tier.
	MarkWarned(ctx context.Context, name string, tier Tier, at time.Time) error

	// PresentBackupBytes is the total size of status=present backups (§26 cap).
	PresentBackupBytes(ctx context.Context) (int64, error)

	// EvictableBackups lists the status=present backups that may go before
	// their expiry when the store is full, in eviction order: on-demand
	// backups first, then reaper archives that have an off-site copy, oldest
	// first within each. A reaper archive without an off-site copy is the only
	// copy of a deleted world and is never listed.
	EvictableBackups(ctx context.Context) ([]StoredBackup, error)

	// ListExpiredBackups lists status=present backups whose expires_at < now.
	ListExpiredBackups(ctx context.Context, now time.Time) ([]StoredBackup, error)

	// MarkBackupDeleted flips a backup to status=deleted, deleted_at=at.
	MarkBackupDeleted(ctx context.Context, id string, at time.Time) error

	// BackupsToVerify lists up to limit present backups not found corrupt and
	// not read back since checkedBefore, the ones never read back first, then
	// the ones read back longest ago.
	BackupsToVerify(ctx context.Context, checkedBefore time.Time, limit int) ([]StoredBackup, error)
	// MarkBackupVerified records a read-back that matched at `at`, and the
	// archive's digest when none was recorded yet.
	MarkBackupVerified(ctx context.Context, id, sha256 string, at time.Time) error
	// MarkBackupCorrupt records a read-back that failed: the backup is no
	// longer reused for a reap or offered for a restore.
	MarkBackupCorrupt(ctx context.Context, id string, at time.Time) error
	// LiveBackupRefs lists the backup_ref of every backup not deleted.
	LiveBackupRefs(ctx context.Context) ([]string, error)

	// Audit appends a reaper-sourced audit_logs row.
	Audit(ctx context.Context, rec AuditRecord) error
}

// Cluster is the lifecycle (Kubernetes) face: read the CRD, stop the server
// and hold its world, and delete the world PVC. These are the only cluster
// operations §18 performs.
type Cluster interface {
	// Inspect returns the exemption flag and world PVC name for a server, or
	// ErrNotFound if the CRD is gone.
	Inspect(ctx context.Context, name string) (ServerCRD, error)
	// HoldWorld keeps everything else off the server's world until release is
	// called: it sets desiredState=Stopped if the server is still meant to run,
	// and once the server is fully down (not ready, phase Stopped, no game pod)
	// and no restore, backup or file write holds the world, it takes the world
	// maintenance lock (internal/maintenance, KindReap) and keeps it fresh.
	// Until then it returns ErrNotQuiet. The returned context ends if the lock
	// cannot be kept; the world must not be read or deleted on it after that.
	HoldWorld(ctx context.Context, name string) (held context.Context, release func(), err error)
	// WorldExists reports whether the world PersistentVolumeClaim exists.
	WorldExists(ctx context.Context, pvc string) (bool, error)
	// DeletePVC deletes the world PersistentVolumeClaim.
	DeletePVC(ctx context.Context, pvc string) error
}

// Warner delivers an impending-reap notice. It is optional and best-effort: a
// nil Warner or a delivery error never blocks a reap (red line ⑤). Warn returns
// nil only when the notice was handed to the delivery channel; an error (or a
// nil Warner) leaves warned_* unstamped, so the next daily run retries instead
// of silently burning the owner's only warning.
type Warner interface {
	Warn(ctx context.Context, ownerID, server, remaining string) error
}

// Reaper runs the §18 batch. Now and IDGen are injectable for hermetic tests;
// Log defaults to slog.Default(); Warner may be nil.
type Reaper struct {
	Cfg      Config
	Store    Store
	Cluster  Cluster
	Archiver backup.WorldArchiver
	Warner   Warner
	Log      *slog.Logger
	Now      func() time.Time
	IDGen    func() string
}

// Summary is the per-run tally (feeds §23 metrics).
type Summary struct {
	Evaluated    int
	WorldsReaped int
	Warned       int
	// Skipped are servers the run failed on (archive, store, cluster or
	// capacity errors); their worlds are kept and retried next run. Exempt
	// servers and rows whose CRD is gone are not counted.
	Skipped int
	// StoreFull are the Skipped servers kept because the backup store was at
	// capacity and eviction could not make room.
	StoreFull      int
	EvictedEarly   int
	BackupsExpired int
	// ExpireFailed are expired backups the retention pass could not remove.
	ExpireFailed int
	// AwaitingOffsite are idle worlds that are archived and kept until the
	// archive's off-site copy lands.
	AwaitingOffsite int
	// AwaitingStop are idle servers left for the next run because they were not
	// fully down yet (the run told a running one to stop) or another operation
	// held their world.
	AwaitingStop int
	// Verified are archives read back in full and found matching; Corrupt are
	// the ones that were not (marked, and never reused or restored from);
	// VerifyFailed are the ones that could not be read back at all this run.
	Verified     int
	Corrupt      int
	VerifyFailed int
	// Swept are leftover files of interrupted archives removed; OrphanArchives
	// are finished archives no backup records, kept for now (see
	// backup.Swept); SweepFailed reports that the sweep did not complete.
	Swept          int
	OrphanArchives int
	SweepFailed    bool
}

// Failed reports whether the run left work undone or found damage: a server it
// could not process, an expired backup it could not remove, an archive that did
// not read back or could not be read, or a sweep that did not complete. The
// worlds are safe either way, but whoever operates the run must hear.
func (s Summary) Failed() bool {
	return s.Skipped > 0 || s.ExpireFailed > 0 || s.Corrupt > 0 || s.VerifyFailed > 0 || s.SweepFailed
}

func (r *Reaper) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reaper) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Reaper) id() string {
	if r.IDGen != nil {
		return r.IDGen()
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "bk-" + strconv.FormatInt(r.now().UnixNano(), 16)
	}
	return "bk-" + hex.EncodeToString(b[:])
}

// RunOnce executes one full batch: a world pass over active servers, then a
// retention pass over expired backups. It is idempotent and restart-safe, so a
// Kubernetes CronJob can drive the daily cadence (spec §18). Per-server
// failures are logged and counted as Skipped without aborting the batch; only
// an inability to list servers is a hard error. Summary.Failed tells the
// caller whether the run as a whole should report failure.
func (r *Reaper) RunOnce(ctx context.Context) (Summary, error) {
	var sum Summary

	cands, err := r.Store.ListActiveServers(ctx)
	if err != nil {
		return sum, fmt.Errorf("reaper: list active servers: %w", err)
	}

	// Warning thresholds derive from the deadline, so the offsets must be
	// largest-first (earliest warning first) to honor §18's elif precedence.
	offs := append([]time.Duration(nil), r.Cfg.WarnBefore...)
	sort.Slice(offs, func(i, j int) bool { return offs[i] > offs[j] })

	now := r.now()
	for _, c := range cands {
		sum.Evaluated++
		if err := r.evaluate(ctx, now, offs, c, &sum); err != nil {
			if errors.Is(err, ErrNotQuiet) {
				sum.AwaitingStop++
				r.log().Warn("reaper: idle world not quiet, retrying next run", "server", c.Name, "why", err)
				continue
			}
			r.log().Error("reaper: skipping server", "server", c.Name, "err", err)
			sum.Skipped++
			if errors.Is(err, errStoreFull) {
				sum.StoreFull++
			}
		}
	}

	r.retain(ctx, now, &sum)
	return sum, nil
}

// RunRetention is the archive-store half of RunOnce on its own: backups past
// their expiry are deleted, archives are read back, and leftovers are swept,
// while no server is looked at and no world is touched. It needs no Cluster,
// which is what lets it run where the worlds are out of reach (an install with
// no worlds root): backups still leave the store when they expire there.
func (r *Reaper) RunRetention(ctx context.Context) Summary {
	var sum Summary
	r.retain(ctx, r.now(), &sum)
	return sum
}

func (r *Reaper) retain(ctx context.Context, now time.Time, sum *Summary) {
	r.expireBackups(ctx, now, sum)
	r.verifyBackups(ctx, now, sum)
	r.sweepArchives(ctx, now, sum)
}

// evaluate handles one server: exemption, reap, or warning. A returned error
// means the server was skipped (counted by the caller); nil covers the normal
// outcomes including "exempt" and "warned".
func (r *Reaper) evaluate(ctx context.Context, now time.Time, offs []time.Duration, c Candidate, sum *Summary) error {
	crd, err := r.Cluster.Inspect(ctx, c.Name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// CRD gone but the row lingers — nothing safe to do; not a failure.
			r.log().Warn("reaper: CRD missing, skipping", "server", c.Name)
			return nil
		}
		return fmt.Errorf("inspect: %w", err)
	}
	if crd.Exempt {
		// Red line ①: system servers (lobby/proxy) are never reaped.
		return nil
	}

	idle := now.Sub(c.LastActiveAt)
	if idle > r.Cfg.IdleBeforeReap {
		return r.reap(ctx, now, c, crd, sum)
	}
	r.maybeWarn(ctx, now, idle, offs, c, sum)
	return nil
}

// reap stops the server and holds its world, archives the world, records the
// backup, and only then deletes the PVC and releases ownership — the strict
// ordering of red line ④. Nothing can start the server or touch its world while
// it is held, so the archive is of a world at rest and the PVC deleted is the
// one archived.
func (r *Reaper) reap(ctx context.Context, now time.Time, c Candidate, crd ServerCRD, sum *Summary) error {
	held, release, err := r.Cluster.HoldWorld(ctx, c.Name)
	if err != nil {
		return fmt.Errorf("hold world: %w", err)
	}
	defer release()
	ctx = held

	exists, err := r.Cluster.WorldExists(ctx, crd.PVC)
	if err != nil {
		return fmt.Errorf("look up world volume: %w", err)
	}
	if !exists {
		return r.reapNoWorld(ctx, now, c, sum)
	}

	// §26 soft cap: free space before adding a backup. If the store cannot be
	// brought under cap, preserve the world rather than delete it unbacked.
	if r.Cfg.MaxLocalBytes > 0 {
		ok, err := r.ensureCapacity(ctx, now, sum)
		if err != nil {
			return fmt.Errorf("ensure capacity: %w", err)
		}
		if !ok {
			r.log().Error("reaper: backup store full, world preserved", "server", c.Name)
			return errStoreFull
		}
	}

	// Idempotent archive: if a prior run already archived this (unchanged)
	// world but failed before deleting the PVC, reuse that backup rather than
	// writing a duplicate. The world has not changed since last_active_at, so
	// any present backup created after it still describes the current world.
	fresh, ok, err := r.Store.FreshBackup(ctx, c.Name, c.LastActiveAt)
	if err != nil {
		return fmt.Errorf("lookup fresh backup: %w", err)
	}
	if ok {
		// The archive may have sat on disk for days (a delete that failed, the
		// wait for its off-site copy); it is about to become the only copy, so it
		// is read back first. One that fails is marked and replaced by a fresh
		// archive of the world, which is still there.
		good, err := r.verifyOne(ctx, now, fresh.ID, fresh.Ref, fresh.SHA256, sum)
		if err != nil {
			return fmt.Errorf("read back archive %s: %w", fresh.ID, err)
		}
		ok = good
	}
	ref, offsite := fresh.Ref, fresh.Offsite
	if !ok {
		a, err := r.Archiver.Archive(ctx, c.Name, crd.PVC)
		if err != nil {
			// Red line ④: archive failed → the PVC is untouched, the world
			// survives, and this server is retried next run.
			return fmt.Errorf("archive: %w", err)
		}
		if len(a.Skipped) > 0 {
			r.log().Warn("reaper: archive leaves out entries that are not plain files or directories",
				"server", c.Name, "count", len(a.Skipped), "first", a.Skipped[:min(len(a.Skipped), 5)])
		}
		rec := BackupRecord{
			ID:             r.id(),
			ServerName:     c.Name,
			FormerOwner:    c.OwnerID,
			BackupRef:      string(a.Ref),
			SizeBytes:      a.Size,
			Reason:         ReasonInactive,
			ExpiresAt:      now.Add(r.Cfg.Retention),
			SHA256:         a.SHA256,
			SkippedEntries: len(a.Skipped),
		}
		if err := r.Store.InsertBackup(ctx, rec); err != nil {
			// The archive exists but is untracked. Delete the orphan so it does
			// not leak, then fail without touching the PVC.
			if derr := r.Archiver.Delete(ctx, a.Ref); derr != nil {
				r.log().Error("reaper: orphan archive cleanup failed", "server", c.Name, "ref", a.Ref, "err", derr)
			}
			return fmt.Errorf("insert backup: %w", err)
		}
		ref, offsite = string(a.Ref), false
	}

	// With an off-site bucket configured, the archive on this node's disk is
	// not enough on its own: a lost disk would take it along with the world.
	if r.Cfg.RequireOffsite && !offsite {
		sum.AwaitingOffsite++
		r.log().Info("reaper: world archived, kept until the archive's off-site copy is confirmed", "server", c.Name, "backup_ref", ref)
		return nil
	}

	// World is safely archived and recorded — now (and only now) delete it.
	// The lock must still be held: a lapsed one could have let the server start.
	if err := context.Cause(ctx); err != nil {
		return fmt.Errorf("world no longer held: %w", err)
	}
	if err := r.Cluster.DeletePVC(ctx, crd.PVC); err != nil {
		// The backup row persists; next run's FreshBackup reuses it and retries
		// the delete, so no duplicate archive is created.
		return fmt.Errorf("delete pvc: %w", err)
	}
	return r.finishReap(ctx, now, c, ref, sum)
}

// finishReap releases a world whose PVC is gone and records the reap.
func (r *Reaper) finishReap(ctx context.Context, now time.Time, c Candidate, ref string, sum *Summary) error {
	if err := r.Store.ReleaseWorld(ctx, c.Name, now); err != nil {
		return fmt.Errorf("release world: %w", err)
	}
	if err := r.Store.Audit(ctx, AuditRecord{Action: ActionReapWorld, ServerName: c.Name, FormerOwner: c.OwnerID}); err != nil {
		r.log().Error("reaper: audit reap_world failed", "server", c.Name, "err", err)
	}

	sum.WorldsReaped++
	// felis_reaper_worlds_deleted_total (spec §23) advances in lockstep with the
	// per-run Summary tally — incremented once per world whose PVC went, not at
	// evaluation time.
	metrics.ReaperWorldsDeletedTotal.Inc()
	r.log().Info("reaper: world reaped", "server", c.Name, "former_owner", c.OwnerID, "backup_ref", ref)
	return nil
}

// reapNoWorld handles an idle server whose world PVC does not exist. With an
// archive of the current world on record, an earlier run deleted the PVC and
// stopped before releasing it, so the reap is finished now. Otherwise there was
// no world to reclaim: an owner who never started the server gives it up
// (nothing to back up), and an unowned one — typically a world reaped earlier —
// only has its clock restarted, so it is not reaped over and over.
func (r *Reaper) reapNoWorld(ctx context.Context, now time.Time, c Candidate, sum *Summary) error {
	fresh, ok, err := r.Store.FreshBackup(ctx, c.Name, c.LastActiveAt)
	if err != nil {
		return fmt.Errorf("lookup fresh backup: %w", err)
	}
	if ok {
		return r.finishReap(ctx, now, c, fresh.Ref, sum)
	}
	if c.OwnerID == "" {
		if err := r.Store.RestartClock(ctx, c.Name, now); err != nil {
			return fmt.Errorf("restart clock: %w", err)
		}
		return nil
	}
	if err := r.Store.ReleaseWorld(ctx, c.Name, now); err != nil {
		return fmt.Errorf("release world: %w", err)
	}
	if err := r.Store.Audit(ctx, AuditRecord{Action: ActionReapWorld, ServerName: c.Name, FormerOwner: c.OwnerID}); err != nil {
		r.log().Error("reaper: audit reap_world failed", "server", c.Name, "err", err)
	}
	r.log().Info("reaper: idle server released; it had no world to archive", "server", c.Name, "former_owner", c.OwnerID)
	return nil
}

// ensureCapacity frees the backup store down under MaxLocalBytes by evicting the
// oldest present backups early. Early eviction is destructive (it removes
// not-yet-expired backups), so each eviction is alerted and audited. It returns
// whether the store is now under cap.
func (r *Reaper) ensureCapacity(ctx context.Context, now time.Time, sum *Summary) (bool, error) {
	used, err := r.Store.PresentBackupBytes(ctx)
	if err != nil {
		return false, err
	}
	if used < r.Cfg.MaxLocalBytes {
		return true, nil
	}
	r.log().Warn("reaper: backup store at capacity, evicting on-demand and off-site-copied backups early",
		"used", used, "max", r.Cfg.MaxLocalBytes)

	old, err := r.Store.EvictableBackups(ctx)
	if err != nil {
		return false, err
	}
	for _, b := range old {
		if used < r.Cfg.MaxLocalBytes {
			break
		}
		if err := r.Archiver.Delete(ctx, backup.ArchiveRef(b.BackupRef)); err != nil {
			r.log().Error("reaper: early-evict delete failed", "id", b.ID, "err", err)
			continue
		}
		if err := r.Store.MarkBackupDeleted(ctx, b.ID, now); err != nil {
			r.log().Error("reaper: early-evict mark failed", "id", b.ID, "err", err)
			continue
		}
		if err := r.Store.Audit(ctx, AuditRecord{Action: ActionEvictBackup, ServerName: b.ServerName}); err != nil {
			r.log().Error("reaper: audit evict failed", "id", b.ID, "err", err)
		}
		used -= b.SizeBytes
		sum.EvictedEarly++
		r.log().Warn("reaper: backup evicted early", "id", b.ID, "server", b.ServerName, "reason", b.Reason, "bytes", b.SizeBytes)
	}
	if used >= r.Cfg.MaxLocalBytes {
		r.log().Error("reaper: backup store still full; what remains are the only copies of reaped worlds, kept until they expire",
			"used", used, "max", r.Cfg.MaxLocalBytes)
	}
	return used < r.Cfg.MaxLocalBytes, nil
}

// maybeWarn sends at most one impending-reap notice per run, honoring §18's
// elif precedence (earliest unsent warning first). Unowned servers are never
// warned but are still reaped at the deadline (red line ⑤). The warned_* stamp
// records a DELIVERED notice: a nil Warner or a delivery error is logged and
// leaves the stamp untouched, so the next run retries — bounded by the warning
// window, since the reap itself removes the candidate. A real join (RecordJoin)
// clears the stamps when a player renews.
func (r *Reaper) maybeWarn(ctx context.Context, now time.Time, idle time.Duration, offs []time.Duration, c Candidate, sum *Summary) {
	if c.OwnerID == "" {
		return
	}
	for i := 0; i < len(offs) && i < 2; i++ {
		threshold := r.Cfg.IdleBeforeReap - offs[i]
		if idle <= threshold {
			continue
		}
		tier := Tier(i)
		if !c.warnedAt(tier).IsZero() {
			continue // already sent this tier
		}
		if r.Warner == nil {
			// No delivery channel is wired at all. Do not stamp: an operator who
			// wires one later must still be able to warn, and a stamp here would
			// have recorded a notice nobody received. Logged every run so silence
			// is never mistaken for delivery.
			r.log().Warn("reaper: warning suppressed — no warner wired",
				"server", c.Name, "owner", c.OwnerID, "remaining", formatRemaining(offs[i]))
			return
		}
		if err := r.Warner.Warn(ctx, c.OwnerID, c.Name, formatRemaining(offs[i])); err != nil {
			// Best-effort: the reap still proceeds on schedule, but the stamp
			// stays empty so the next daily run retries the delivery instead of
			// permanently suppressing the owner's only notice.
			r.log().Warn("reaper: warn delivery failed; will retry next run", "server", c.Name, "err", err)
			return
		}
		if err := r.Store.MarkWarned(ctx, c.Name, tier, now); err != nil {
			r.log().Error("reaper: mark warned failed", "server", c.Name, "err", err)
			return
		}
		sum.Warned++
		return // one warning per run
	}
}

// expireBackups is the retention pass: delete archives whose expires_at has
// passed and mark them deleted. Per-backup failures are logged, not fatal.
func (r *Reaper) expireBackups(ctx context.Context, now time.Time, sum *Summary) {
	exp, err := r.Store.ListExpiredBackups(ctx, now)
	if err != nil {
		r.log().Error("reaper: list expired backups", "err", err)
		sum.ExpireFailed++
		return
	}
	for _, b := range exp {
		if err := r.Archiver.Delete(ctx, backup.ArchiveRef(b.BackupRef)); err != nil {
			r.log().Error("reaper: delete expired archive", "id", b.ID, "err", err)
			sum.ExpireFailed++
			continue
		}
		if err := r.Store.MarkBackupDeleted(ctx, b.ID, now); err != nil {
			r.log().Error("reaper: mark expired deleted", "id", b.ID, "err", err)
			sum.ExpireFailed++
			continue
		}
		sum.BackupsExpired++
	}
}

// verifyBackups is the read-back pass: up to VerifyPerRun present archives not
// read back within VerifyEvery are read end to end and checked against their
// recorded digest. A backend that cannot read its archives back skips it.
func (r *Reaper) verifyBackups(ctx context.Context, now time.Time, sum *Summary) {
	if _, ok := r.Archiver.(backup.Verifier); !ok || r.Cfg.VerifyPerRun <= 0 {
		return
	}
	list, err := r.Store.BackupsToVerify(ctx, now.Add(-r.Cfg.VerifyEvery), r.Cfg.VerifyPerRun)
	if err != nil {
		r.log().Error("reaper: list backups to read back", "err", err)
		sum.VerifyFailed++
		return
	}
	for _, b := range list {
		if _, err := r.verifyOne(ctx, now, b.ID, b.BackupRef, b.SHA256, sum); err != nil {
			r.log().Error("reaper: read back archive", "id", b.ID, "server", b.ServerName, "err", err)
			sum.VerifyFailed++
		}
	}
}

// verifyOne reads one archive back and records the outcome. It reports whether
// the archive is good; an error means it could not be read back at all (the
// store is not mounted, the run was cancelled), which says nothing about the
// archive. A backend that cannot read its archives back reports every archive
// good, as before read-backs existed.
func (r *Reaper) verifyOne(ctx context.Context, now time.Time, id, ref, want string, sum *Summary) (bool, error) {
	v, ok := r.Archiver.(backup.Verifier)
	if !ok {
		return true, nil
	}
	got, err := v.Verify(ctx, backup.ArchiveRef(ref), want)
	switch {
	case errors.Is(err, backup.ErrCorrupt):
		sum.Corrupt++
		r.log().Error("reaper: archive is corrupt; it will not be reused or offered for restore", "id", id, "ref", ref, "err", err)
		if err := r.Store.MarkBackupCorrupt(ctx, id, now); err != nil {
			return false, fmt.Errorf("mark corrupt: %w", err)
		}
		return false, nil
	case err != nil:
		return false, err
	}
	if err := r.Store.MarkBackupVerified(ctx, id, got, now); err != nil {
		r.log().Error("reaper: record read-back", "id", id, "err", err)
	}
	sum.Verified++
	return true, nil
}

// sweepArchives removes what interrupted archives left in the store (see
// backup.Sweeper): unfinished files older than PartialAfter, and finished ones
// no backup records once they are older than Retention, the longest any backup
// is kept. Younger unrecorded archives are reported and kept.
func (r *Reaper) sweepArchives(ctx context.Context, now time.Time, sum *Summary) {
	sw, ok := r.Archiver.(backup.Sweeper)
	if !ok {
		return
	}
	refs, err := r.Store.LiveBackupRefs(ctx)
	if err != nil {
		// Without the list every archive would look unclaimed.
		r.log().Error("reaper: list recorded archives; sweep skipped", "err", err)
		sum.SweepFailed = true
		return
	}
	live := make(map[backup.ArchiveRef]bool, len(refs))
	for _, ref := range refs {
		live[backup.ArchiveRef(ref)] = true
	}
	res, err := sw.Sweep(ctx, func(ref backup.ArchiveRef) bool { return live[ref] },
		now.Add(-r.Cfg.PartialAfter), now.Add(-r.Cfg.Retention))
	if err != nil {
		r.log().Error("reaper: sweep archive store", "err", err)
		sum.SweepFailed = true
	}
	for _, p := range res.Removed {
		r.log().Info("reaper: removed leftover of an interrupted archive", "path", p)
	}
	sum.Swept += len(res.Removed)
	sum.OrphanArchives += len(res.Orphans)
	if len(res.Orphans) > 0 {
		r.log().Warn("reaper: archives no backup records are kept until they are older than the retention",
			"count", len(res.Orphans), "bytes", res.OrphanBytes, "first", res.Orphans[:min(len(res.Orphans), 5)])
	}
}

// formatRemaining renders an offset as the human-facing time left before reap.
func formatRemaining(d time.Duration) string {
	if d%Day == 0 {
		return strconv.FormatInt(int64(d/Day), 10) + "d"
	}
	if d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	return d.String()
}
