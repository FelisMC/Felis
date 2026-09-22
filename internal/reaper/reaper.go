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
}

// DefaultConfig is the spec's §24 default window set.
func DefaultConfig() Config {
	return Config{
		IdleBeforeReap: 15 * Day,
		WarnBefore:     []time.Duration{3 * Day, 1 * Day},
		Retention:      90 * Day,
		MaxLocalBytes:  0,
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
}

// StoredBackup is an existing world_backups row, used by both the expiry pass
// and the capacity-eviction path.
type StoredBackup struct {
	ID         string
	ServerName string
	BackupRef  string
	SizeBytes  int64
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

	// FreshBackup reports an existing present backup for server whose world is
	// still current — created at or after since (the world's last_active_at).
	// It makes a reap idempotent across a DeletePVC failure: the retry reuses
	// the archive instead of writing a duplicate.
	FreshBackup(ctx context.Context, server string, since time.Time) (ref string, ok bool, err error)

	// InsertBackup records a world_backups row (status=present).
	InsertBackup(ctx context.Context, rec BackupRecord) error

	// ReleaseWorld is the post-delete business mutation: owner_id→NULL,
	// last_active_at→at (clock reset), warned_*→NULL. It does NOT delete the
	// row (red line ②).
	ReleaseWorld(ctx context.Context, name string, at time.Time) error

	// MarkWarned stamps the warned_3d_at / warned_1d_at column for tier.
	MarkWarned(ctx context.Context, name string, tier Tier, at time.Time) error

	// PresentBackupBytes is the total size of status=present backups (§26 cap).
	PresentBackupBytes(ctx context.Context) (int64, error)

	// OldestPresentBackups lists status=present backups oldest-first, for
	// early eviction when the store is full.
	OldestPresentBackups(ctx context.Context) ([]StoredBackup, error)

	// ListExpiredBackups lists status=present backups whose expires_at < now.
	ListExpiredBackups(ctx context.Context, now time.Time) ([]StoredBackup, error)

	// MarkBackupDeleted flips a backup to status=deleted, deleted_at=at.
	MarkBackupDeleted(ctx context.Context, id string, at time.Time) error

	// Audit appends a reaper-sourced audit_logs row.
	Audit(ctx context.Context, rec AuditRecord) error
}

// Cluster is the lifecycle (Kubernetes) face: read the CRD, delete the world
// PVC, and flip desiredState to Stopped. These are the only cluster operations
// §18 performs.
type Cluster interface {
	// Inspect returns the exemption flag and world PVC name for a server, or
	// ErrNotFound if the CRD is gone.
	Inspect(ctx context.Context, name string) (ServerCRD, error)
	// DeletePVC deletes the world PersistentVolumeClaim.
	DeletePVC(ctx context.Context, pvc string) error
	// Stop sets spec.desiredState=Stopped.
	Stop(ctx context.Context, name string) error
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
	Evaluated      int
	WorldsReaped   int
	Warned         int
	Skipped        int // exempt, CRD gone, or could not back up
	EvictedEarly   int
	BackupsExpired int
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
// an inability to list servers is a hard error.
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
			r.log().Error("reaper: skipping server", "server", c.Name, "err", err)
			sum.Skipped++
		}
	}

	r.expireBackups(ctx, now, &sum)
	return sum, nil
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

// reap archives the world, records the backup, and only then deletes the PVC,
// releases ownership, and stops the server — the strict ordering of red line ④.
func (r *Reaper) reap(ctx context.Context, now time.Time, c Candidate, crd ServerCRD, sum *Summary) error {
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
	ref, ok, err := r.Store.FreshBackup(ctx, c.Name, c.LastActiveAt)
	if err != nil {
		return fmt.Errorf("lookup fresh backup: %w", err)
	}
	if !ok {
		aref, size, err := r.Archiver.Archive(ctx, c.Name, crd.PVC)
		if err != nil {
			// Red line ④: archive failed → the PVC is untouched, the world
			// survives, and this server is retried next run.
			return fmt.Errorf("archive: %w", err)
		}
		rec := BackupRecord{
			ID:          r.id(),
			ServerName:  c.Name,
			FormerOwner: c.OwnerID,
			BackupRef:   string(aref),
			SizeBytes:   size,
			Reason:      ReasonInactive,
			ExpiresAt:   now.Add(r.Cfg.Retention),
		}
		if err := r.Store.InsertBackup(ctx, rec); err != nil {
			// The archive exists but is untracked. Delete the orphan so it does
			// not leak, then fail without touching the PVC.
			if derr := r.Archiver.Delete(ctx, aref); derr != nil {
				r.log().Error("reaper: orphan archive cleanup failed", "server", c.Name, "ref", aref, "err", derr)
			}
			return fmt.Errorf("insert backup: %w", err)
		}
		ref = string(aref)
	}

	// World is safely archived and recorded — now (and only now) delete it.
	if err := r.Cluster.DeletePVC(ctx, crd.PVC); err != nil {
		// The backup row persists; next run's FreshBackup reuses it and retries
		// the delete, so no duplicate archive is created.
		return fmt.Errorf("delete pvc: %w", err)
	}
	if err := r.Store.ReleaseWorld(ctx, c.Name, now); err != nil {
		return fmt.Errorf("release world: %w", err)
	}
	if err := r.Cluster.Stop(ctx, c.Name); err != nil {
		// The world is already deleted and ownership released; the desiredState
		// flip is cosmetic by comparison. Log, but the reap stands.
		r.log().Error("reaper: set desiredState=Stopped failed", "server", c.Name, "err", err)
	}
	if err := r.Store.Audit(ctx, AuditRecord{Action: ActionReapWorld, ServerName: c.Name, FormerOwner: c.OwnerID}); err != nil {
		r.log().Error("reaper: audit reap_world failed", "server", c.Name, "err", err)
	}

	sum.WorldsReaped++
	// felis_reaper_worlds_deleted_total (spec §23) advances in lockstep with the
	// per-run Summary tally — incremented here, at the one point a world's PVC has
	// actually been deleted, not at evaluation time.
	metrics.ReaperWorldsDeletedTotal.Inc()
	r.log().Info("reaper: world reaped", "server", c.Name, "former_owner", c.OwnerID, "backup_ref", ref)
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
	r.log().Warn("reaper: backup store at capacity, evicting oldest backups early",
		"used", used, "max", r.Cfg.MaxLocalBytes)

	old, err := r.Store.OldestPresentBackups(ctx)
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
		return
	}
	for _, b := range exp {
		if err := r.Archiver.Delete(ctx, backup.ArchiveRef(b.BackupRef)); err != nil {
			r.log().Error("reaper: delete expired archive", "id", b.ID, "err", err)
			continue
		}
		if err := r.Store.MarkBackupDeleted(ctx, b.ID, now); err != nil {
			r.log().Error("reaper: mark expired deleted", "id", b.ID, "err", err)
			continue
		}
		sum.BackupsExpired++
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
