// Package reaper archives and records worlds before deleting their PVCs, preserving backup access independently of server ownership.
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

const Day = 24 * time.Hour

const ReasonInactive = "inactive_15d"

const ReasonReleased = "released"

const (
	ActionReapWorld    = "reap_world"
	ActionEvictBackup  = "evict_backup_early"
	ActionReleaseWorld = "release_world"
	ActionDeleteServer = "delete_server"
)

// ErrNotFound skips worlds whose missing CRD prevents checking system-server exemptions.
var ErrNotFound = errors.New("reaper: server not found")

// ErrNotQuiet preserves the world for a later run while shutdown or another volume operation is in progress.
var ErrNotQuiet = errors.New("reaper: world not quiet")

var errStoreFull = errors.New("reaper: backup store full, world preserved")

// Tier names follow warning columns, not fixed durations; offsets come from Config.WarnBefore.
type Tier int

const (
	Tier3d Tier = iota
	Tier1d
)

type Config struct {
	IdleBeforeReap  time.Duration   // idle duration before reap
	WarnBefore      []time.Duration // warning offsets before the deadline
	Retention       time.Duration   // archive retention after world deletion
	MaxLocalBytes   int64           // backup-store soft cap; zero is unlimited
	ManualRetention time.Duration
	ManualKeep      int
	ManualCooldown  time.Duration
	// ScheduledEvery=0 disables scheduled points; their keep and retention limits are separate from manual backups.
	ScheduledEvery     time.Duration
	ScheduledKeep      int
	ScheduledRetention time.Duration
	// RequireOffsite delays PVC deletion until the archive's offsite copy is recorded.
	RequireOffsite bool
	// VerifyEvery and VerifyPerRun bound full read-back checks, oldest unchecked archives first.
	VerifyEvery  time.Duration
	VerifyPerRun int
	PartialAfter time.Duration
}

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

// Candidate activity advances on real joins, not stops or wakes; newly created servers use creation time.
type Candidate struct {
	Name         string
	OwnerID      string // empty when unowned; still reaped but not warned
	LastActiveAt time.Time
	Warned3dAt   time.Time // zero = not yet sent
	Warned1dAt   time.Time // zero = not yet sent
	// RetireRequestedAt zero means no request; pending release or deletion bypasses the idle deadline.
	RetireRequestedAt time.Time
	RetireDelete      bool
}

func (c Candidate) warnedAt(t Tier) time.Time {
	if t == Tier1d {
		return c.Warned1dAt
	}
	return c.Warned3dAt
}

func (c Candidate) retiring() bool { return !c.RetireRequestedAt.IsZero() }

// worldSince requires archives newer than the last join and any retirement request.
func (c Candidate) worldSince() time.Time {
	if c.RetireRequestedAt.After(c.LastActiveAt) {
		return c.RetireRequestedAt
	}
	return c.LastActiveAt
}

type ServerCRD struct {
	Exempt bool
	PVC    string
	UID    string
}

// BackupRecord.FormerOwner is retained without a foreign key so backups can outlive server rows.
type BackupRecord struct {
	ID          string
	ServerName  string
	FormerOwner string
	BackupRef   string
	SizeBytes   int64
	Reason      string
	ExpiresAt   time.Time
	// SHA256 is empty when the backend keeps no digest; SkippedEntries counts unsupported source entries.
	SHA256         string
	SkippedEntries int
}

type Fresh struct {
	ID      string
	Ref     string
	SHA256  string
	Offsite bool
}

type StoredBackup struct {
	ID         string
	ServerName string
	BackupRef  string
	SizeBytes  int64
	Reason     string
	SHA256     string // "" when none was recorded
}

// AuditRecord stores FormerOwner in payload because audit_logs has no dedicated former-owner column.
type AuditRecord struct {
	Action      string
	ServerName  string
	FormerOwner string
}

type Store interface {
	ListActiveServers(ctx context.Context) ([]Candidate, error)

	// FreshBackup selects intact reaper archives since activity and claim, preferring offsite copies so retries reuse existing backups.
	FreshBackup(ctx context.Context, server string, since time.Time) (b Fresh, ok bool, err error)

	InsertBackup(ctx context.Context, rec BackupRecord) error

	// ReleaseWorld clears ownership, warnings and wake grants while preserving the server row and pending deletion.
	ReleaseWorld(ctx context.Context, name string, at time.Time) error

	// DeleteServerRow soft-deletes only a pending deletion, leaving a newly recreated server unchanged.
	DeleteServerRow(ctx context.Context, name string, at time.Time) error

	// RestartClock clears warnings on servers with no world to avoid retrying them as idle every run.
	RestartClock(ctx context.Context, name string, at time.Time) error

	MarkWarned(ctx context.Context, name string, tier Tier, at time.Time) error

	PresentBackupBytes(ctx context.Context) (int64, error)

	// EvictableBackups prioritizes manual backups, then offsite-copied reaper archives; the only copy of a deleted world is never evictable.
	EvictableBackups(ctx context.Context) ([]StoredBackup, error)

	// ListExpiredBackups includes present backups past retention and rows explicitly expired by their owner.
	ListExpiredBackups(ctx context.Context, now time.Time) ([]StoredBackup, error)

	MarkBackupDeleted(ctx context.Context, id string, at time.Time) error

	// BackupsToVerify orders never-checked archives first, then oldest verification times.
	BackupsToVerify(ctx context.Context, checkedBefore time.Time, limit int) ([]StoredBackup, error)
	// MarkBackupVerified also records the digest when older archives have none.
	MarkBackupVerified(ctx context.Context, id, sha256 string, at time.Time) error
	// MarkBackupCorrupt prevents reuse and restoration after read-back failure.
	MarkBackupCorrupt(ctx context.Context, id string, at time.Time) error
	LiveBackupRefs(ctx context.Context) ([]string, error)

	Audit(ctx context.Context, rec AuditRecord) error
}

type Cluster interface {
	Inspect(ctx context.Context, name string) (ServerCRD, error)
	// HoldWorld returns a context cancelled when the renewable lock is lost; never archive or delete after cancellation.
	HoldWorld(ctx context.Context, name string) (held context.Context, release func(), err error)
	WorldExists(ctx context.Context, pvc string) (bool, error)
	DeletePVC(ctx context.Context, pvc string) error
	// DeleteServer requires the inspected UID and treats absence as success, leaving a same-name replacement untouched.
	DeleteServer(ctx context.Context, name, uid string) error
}

// Warner failures never block reaping; stamp warning delivery only after a successful send so later runs can retry.
type Warner interface {
	Warn(ctx context.Context, ownerID, server, remaining string) error
}

// Reaper defaults Log to slog.Default and permits nil Warner.
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

type Summary struct {
	Evaluated      int
	WorldsReaped   int
	Released       int
	ServersDeleted int
	Warned         int
	// Skipped counts failures requiring retry, excluding exempt and missing-CRD servers unless an unguarded leftover world prevents deletion.
	Skipped         int
	StoreFull       int
	EvictedEarly    int
	BackupsExpired  int
	ExpireFailed    int
	AwaitingOffsite int
	AwaitingStop    int
	Verified        int
	Corrupt         int
	VerifyFailed    int
	Swept           int
	OrphanArchives  int
	SweepFailed     bool
}

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

func (r *Reaper) RunOnce(ctx context.Context) (Summary, error) {
	var sum Summary

	cands, err := r.Store.ListActiveServers(ctx)
	if err != nil {
		return sum, fmt.Errorf("reaper: list active servers: %w", err)
	}

	// Sort offsets largest-first so earlier warnings precede later warning tiers.
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

// RunRetention expires, verifies and sweeps archives without requiring a Cluster or touching worlds.
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

func (r *Reaper) evaluate(ctx context.Context, now time.Time, offs []time.Duration, c Candidate, sum *Summary) error {
	crd, err := r.Cluster.Inspect(ctx, c.Name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			if c.RetireDelete {
				return r.forgetServer(ctx, now, c, sum)
			}
			r.log().Warn("reaper: CRD missing, skipping", "server", c.Name)
			return nil
		}
		return fmt.Errorf("inspect: %w", err)
	}
	if crd.Exempt {
		if c.retiring() {
			r.log().Warn("reaper: a system server is never given up or deleted; request ignored", "server", c.Name)
		}
		return nil
	}
	if c.retiring() {
		return r.reap(ctx, now, c, crd, sum)
	}

	idle := now.Sub(c.LastActiveAt)
	if idle > r.Cfg.IdleBeforeReap {
		return r.reap(ctx, now, c, crd, sum)
	}
	r.maybeWarn(ctx, now, idle, offs, c, sum)
	return nil
}

// reap holds the stopped world, writes and records its archive, then deletes the PVC and releases ownership in that order.
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
		return r.reapNoWorld(ctx, now, c, crd, sum)
	}

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

	// Reuse an intact archive of the current world after partial failures instead of creating duplicates.
	fresh, ok, err := r.Store.FreshBackup(ctx, c.Name, c.worldSince())
	if err != nil {
		return fmt.Errorf("lookup fresh backup: %w", err)
	}
	if ok {
		// Read back a reused archive before it becomes the only copy; corrupt archives are replaced while the world still exists.
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
			return fmt.Errorf("archive: %w", err)
		}
		if len(a.Skipped) > 0 {
			r.log().Warn("reaper: archive leaves out entries that are not plain files or directories",
				"server", c.Name, "count", len(a.Skipped), "first", a.Skipped[:min(len(a.Skipped), 5)])
		}
		reason := ReasonInactive
		if c.retiring() {
			reason = ReasonReleased
		}
		rec := BackupRecord{
			ID:             r.id(),
			ServerName:     c.Name,
			FormerOwner:    c.OwnerID,
			BackupRef:      string(a.Ref),
			SizeBytes:      a.Size,
			Reason:         reason,
			ExpiresAt:      now.Add(r.Cfg.Retention),
			SHA256:         a.SHA256,
			SkippedEntries: len(a.Skipped),
		}
		if err := r.Store.InsertBackup(ctx, rec); err != nil {
			if derr := r.Archiver.Delete(ctx, a.Ref); derr != nil {
				r.log().Error("reaper: orphan archive cleanup failed", "server", c.Name, "ref", a.Ref, "err", derr)
			}
			return fmt.Errorf("insert backup: %w", err)
		}
		ref, offsite = string(a.Ref), false
	}

	// With offsite enabled, retain the world until its archive has a recorded remote copy.
	if r.Cfg.RequireOffsite && !offsite {
		sum.AwaitingOffsite++
		r.log().Info("reaper: world archived, kept until the archive's off-site copy is confirmed", "server", c.Name, "backup_ref", ref)
		return nil
	}

	// Delete only after archival is recorded and while the world lock is still held.
	if err := context.Cause(ctx); err != nil {
		return fmt.Errorf("world no longer held: %w", err)
	}
	if err := r.Cluster.DeletePVC(ctx, crd.PVC); err != nil {
		// Keep the backup row on deletion failure so FreshBackup can resume without duplicating the archive.
		return fmt.Errorf("delete pvc: %w", err)
	}
	return r.finishReap(ctx, now, c, crd, ref, sum)
}

func (r *Reaper) finishReap(ctx context.Context, now time.Time, c Candidate, crd ServerCRD, ref string, sum *Summary) error {
	if c.retiring() {
		if err := r.retire(ctx, now, c, crd, sum); err != nil {
			return err
		}
	} else {
		if err := r.Store.ReleaseWorld(ctx, c.Name, now); err != nil {
			return fmt.Errorf("release world: %w", err)
		}
		if err := r.Store.Audit(ctx, AuditRecord{Action: ActionReapWorld, ServerName: c.Name, FormerOwner: c.OwnerID}); err != nil {
			r.log().Error("reaper: audit reap_world failed", "server", c.Name, "err", err)
		}
	}

	sum.WorldsReaped++
	metrics.ReaperWorldsDeletedTotal.Inc()
	r.log().Info("reaper: world reaped", "server", c.Name, "former_owner", c.OwnerID, "backup_ref", ref)
	return nil
}

// reapNoWorld finishes interrupted deletions with a fresh archive; otherwise release an owned empty server or reset its idle clock.
func (r *Reaper) reapNoWorld(ctx context.Context, now time.Time, c Candidate, crd ServerCRD, sum *Summary) error {
	fresh, ok, err := r.Store.FreshBackup(ctx, c.Name, c.worldSince())
	if err != nil {
		return fmt.Errorf("lookup fresh backup: %w", err)
	}
	if ok {
		return r.finishReap(ctx, now, c, crd, fresh.Ref, sum)
	}
	if c.retiring() {
		return r.retire(ctx, now, c, crd, sum)
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

func (r *Reaper) retire(ctx context.Context, now time.Time, c Candidate, crd ServerCRD, sum *Summary) error {
	if !c.RetireDelete {
		if err := r.Store.ReleaseWorld(ctx, c.Name, now); err != nil {
			return fmt.Errorf("release world: %w", err)
		}
		if err := r.Store.Audit(ctx, AuditRecord{Action: ActionReleaseWorld, ServerName: c.Name, FormerOwner: c.OwnerID}); err != nil {
			r.log().Error("reaper: audit release_world failed", "server", c.Name, "err", err)
		}
		sum.Released++
		r.log().Info("reaper: server given up and released", "server", c.Name, "former_owner", c.OwnerID)
		return nil
	}
	// Mark the row deleted last so interrupted retirement remains pending for the next run.
	if err := r.Cluster.DeleteServer(ctx, c.Name, crd.UID); err != nil {
		return fmt.Errorf("delete server: %w", err)
	}
	return r.deleteRow(ctx, now, c, sum)
}

// forgetServer refuses unarchived leftover PVCs when no CRD remains to hold the world safely for archival.
func (r *Reaper) forgetServer(ctx context.Context, now time.Time, c Candidate, sum *Summary) error {
	if cluster, ok := r.Cluster.(interface {
		RetainedWorlds(context.Context, string) (bool, error)
	}); ok {
		retained, err := cluster.RetainedWorlds(ctx, c.Name)
		if err != nil {
			return err
		}
		if retained {
			return fmt.Errorf("retained world volumes require explicit administrator cleanup")
		}
	}
	pvc := WorldPVCName(c.Name)
	exists, err := r.Cluster.WorldExists(ctx, pvc)
	if err != nil {
		return fmt.Errorf("look up world volume: %w", err)
	}
	if exists {
		return fmt.Errorf("its MinecraftServer is gone but world volume %s is still there; archive and remove it by hand to finish the deletion", pvc)
	}
	return r.deleteRow(ctx, now, c, sum)
}

func (r *Reaper) deleteRow(ctx context.Context, now time.Time, c Candidate, sum *Summary) error {
	if err := r.Store.DeleteServerRow(ctx, c.Name, now); err != nil {
		return fmt.Errorf("mark server deleted: %w", err)
	}
	if err := r.Store.Audit(ctx, AuditRecord{Action: ActionDeleteServer, ServerName: c.Name, FormerOwner: c.OwnerID}); err != nil {
		r.log().Error("reaper: audit delete_server failed", "server", c.Name, "err", err)
	}
	sum.ServersDeleted++
	r.log().Info("reaper: server deleted", "server", c.Name, "former_owner", c.OwnerID)
	return nil
}

// ensureCapacity audits and alerts each early eviction because it removes backups before normal expiry.
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
			continue
		}
		if r.Warner == nil {
			r.log().Warn("reaper: warning suppressed — no warner wired",
				"server", c.Name, "owner", c.OwnerID, "remaining", formatRemaining(offs[i]))
			return
		}
		if err := r.Warner.Warn(ctx, c.OwnerID, c.Name, formatRemaining(offs[i])); err != nil {
			r.log().Warn("reaper: warn delivery failed; will retry next run", "server", c.Name, "err", err)
			return
		}
		if err := r.Store.MarkWarned(ctx, c.Name, tier, now); err != nil {
			r.log().Error("reaper: mark warned failed", "server", c.Name, "err", err)
			return
		}
		sum.Warned++
		return
	}
}

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

// verifyOne distinguishes unreadable archives from confirmed corruption; backends without read-back support skip verification.
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

// sweepArchives removes stale partials and orphan archives only after retention; younger unrecorded archives remain reported.
func (r *Reaper) sweepArchives(ctx context.Context, now time.Time, sum *Summary) {
	sw, ok := r.Archiver.(backup.Sweeper)
	if !ok {
		return
	}
	refs, err := r.Store.LiveBackupRefs(ctx)
	if err != nil {
		// Without live references every archive would appear orphaned; skip sweeping if the query fails.
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

func formatRemaining(d time.Duration) string {
	if d%Day == 0 {
		return strconv.FormatInt(int64(d/Day), 10) + "d"
	}
	if d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	return d.String()
}
