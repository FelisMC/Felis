//go:build pgint

package pgint

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/reaper"
)

// reclaimCluster knows one server; every other row in the shared schema reads
// as a server whose CRD is gone, which the reaper skips.
type reclaimCluster struct {
	name       string
	deletedPVC []string
}

func (c *reclaimCluster) Inspect(_ context.Context, name string) (reaper.ServerCRD, error) {
	if name != c.name {
		return reaper.ServerCRD{}, reaper.ErrNotFound
	}
	return reaper.ServerCRD{PVC: "world-" + name + "-0"}, nil
}

func (c *reclaimCluster) DeletePVC(_ context.Context, pvc string) error {
	c.deletedPVC = append(c.deletedPVC, pvc)
	return nil
}

func (c *reclaimCluster) Stop(context.Context, string) error { return nil }

type reclaimArchiver struct{ archived []string }

func (a *reclaimArchiver) Archive(_ context.Context, server, _ string) (backup.ArchiveRef, int64, error) {
	ref := "/archives/" + server + "-new.tar.gz"
	a.archived = append(a.archived, ref)
	return backup.ArchiveRef(ref), 42, nil
}

func (a *reclaimArchiver) Restore(context.Context, backup.ArchiveRef, string) error { return nil }
func (a *reclaimArchiver) Delete(context.Context, backup.ArchiveRef) error          { return nil }

// TestReclaimRestartsReaperClock: a world reaped weeks ago and claimed by a new
// owner is not reaped again on the next run, and when it does go idle the reap
// archives the new owner's world instead of reusing the previous owner's
// archive (data-durability-5).
func TestReclaimRestartsReaperClock(t *testing.T) {
	ctx := context.Background()
	st := reaper.NewPGStore(db)
	name := "reclaim-" + suffix(t)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, 100, 128, 1)`,
		name); err != nil {
		t.Fatalf("seed server: %v", err)
	}
	first := newUser(t, "user", "reclaim-a")
	if ok, err := repo.ClaimServer(ctx, name, first.ID); err != nil || !ok {
		t.Fatalf("first claim = (%v, %v)", ok, err)
	}

	// The first owner went idle and the reaper took the world 20 days ago.
	reapedAt := time.Now().Add(-20 * reaper.Day)
	if _, err := db.ExecContext(ctx,
		`UPDATE servers SET last_active_at = $2, claimed_at = $3 WHERE name = $1`,
		name, reapedAt.Add(-16*reaper.Day), reapedAt.Add(-60*reaper.Day)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO world_backups (id, server_name, former_owner, backup_ref, size_bytes, reason, status, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, 7, 'inactive_15d', 'present', $5, $6)`,
		"bk-"+suffix(t), name, first.ID, "/archives/"+name+"-old.tar.gz", reapedAt, reapedAt.Add(90*reaper.Day)); err != nil {
		t.Fatalf("seed the old archive: %v", err)
	}
	if err := st.ReleaseWorld(ctx, name, reapedAt); err != nil {
		t.Fatalf("ReleaseWorld: %v", err)
	}
	// Leftover warning stamps must not survive into the next ownership.
	if _, err := db.ExecContext(ctx,
		`UPDATE servers SET warned_3d_at = $2, warned_1d_at = $2 WHERE name = $1`, name, reapedAt); err != nil {
		t.Fatal(err)
	}

	second := newUser(t, "user", "reclaim-b")
	before := time.Now()
	if ok, err := repo.ClaimServer(ctx, name, second.ID); err != nil || !ok {
		t.Fatalf("second claim = (%v, %v)", ok, err)
	}
	var lastActive time.Time
	var w3, w1 sql.NullTime
	if err := db.QueryRowContext(ctx,
		`SELECT last_active_at, warned_3d_at, warned_1d_at FROM servers WHERE name = $1`, name).Scan(&lastActive, &w3, &w1); err != nil {
		t.Fatal(err)
	}
	if lastActive.Before(before.Add(-time.Minute)) {
		t.Fatalf("last_active_at = %v after the claim at %v: the claim did not restart the clock", lastActive, before)
	}
	if w3.Valid || w1.Valid {
		t.Fatalf("warnings survived the claim: 3d=%v 1d=%v", w3, w1)
	}
	// The previous owner's archive is not this owner's world, whatever since says.
	if f, ok, err := st.FreshBackup(ctx, name, reapedAt.Add(-30*reaper.Day)); err != nil || ok {
		t.Fatalf("FreshBackup = (%+v, %v, %v): the old archive counted as the new world's", f, ok, err)
	}

	cl := &reclaimCluster{name: name}
	ar := &reclaimArchiver{}
	now := time.Now()
	r := &reaper.Reaper{Cfg: reaper.DefaultConfig(), Store: st, Cluster: cl, Archiver: ar,
		Now: func() time.Time { return now }}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(cl.deletedPVC) != 0 || len(ar.archived) != 0 {
		t.Fatalf("the reaper took a world claimed moments ago: deleted %v, archived %v", cl.deletedPVC, ar.archived)
	}

	// Sixteen idle days after the claim it goes, with a fresh archive.
	now = time.Now().Add(16 * reaper.Day)
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce after 16 days: %v", err)
	}
	if len(ar.archived) != 1 || len(cl.deletedPVC) != 1 {
		t.Fatalf("after 16 idle days: archived %v, deleted %v; want one new archive, then the delete", ar.archived, cl.deletedPVC)
	}
	var owner sql.NullString
	var newest string
	if err := db.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE name = $1`, name).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT backup_ref FROM world_backups WHERE server_name = $1 ORDER BY created_at DESC LIMIT 1`, name).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	if owner.Valid || newest != ar.archived[0] {
		t.Fatalf("owner = %v, newest backup = %q; want released and %q", owner, newest, ar.archived[0])
	}
}

// TestManualBackupRationing pins the SQL behind data-durability-9: keep-N
// pruning picks a server's oldest on-demand backups, capacity eviction takes
// on-demand backups before copied reaper archives and never offers the only
// copy of a reaped world, and the API's cooldown and store-size reads see the
// rows the rest of the platform writes.
func TestManualBackupRationing(t *testing.T) {
	ctx := context.Background()
	st := reaper.NewPGStore(db)
	name := "ration-" + suffix(t)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, 100, 128, 1)`,
		name); err != nil {
		t.Fatalf("seed server: %v", err)
	}
	storeBefore, err := repo.BackupStoreBytes(ctx)
	if err != nil {
		t.Fatalf("BackupStoreBytes: %v", err)
	}

	now := time.Now()
	insert := func(id, reason string, created time.Time, offsite bool, size int64) {
		t.Helper()
		var offsiteAt any
		if offsite {
			offsiteAt = created
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO world_backups (id, server_name, backup_ref, size_bytes, reason, status, created_at, expires_at, offsite_at)
			 VALUES ($1, $2, $3, $4, $5, 'present', $6, $7, $8)`,
			id, name, "/archives/"+id+".tar.gz", size, reason, created, created.Add(90*reaper.Day), offsiteAt); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	sfx := suffix(t)
	var manual []string
	for i := 0; i < 7; i++ {
		id := "bk-m" + string(rune('0'+i)) + "-" + sfx
		manual = append(manual, id)
		insert(id, "manual", now.Add(time.Duration(i-7)*time.Hour), false, 10)
	}
	sole := "bk-sole-" + sfx
	copied := "bk-copied-" + sfx
	insert(sole, "inactive_15d", now.Add(-100*reaper.Day), false, 1000)
	insert(copied, "inactive_15d", now.Add(-50*reaper.Day), true, 100)

	excess, err := st.ExcessBackups(ctx, name, "manual", 5, "")
	if err != nil {
		t.Fatalf("ExcessBackups: %v", err)
	}
	if len(excess) != 2 || excess[0].ID != manual[0] || excess[1].ID != manual[1] {
		t.Fatalf("excess = %+v; want the two oldest manual backups, oldest first", excess)
	}
	// The backup a chained restore will extract is never pruned.
	excess, err = st.ExcessBackups(ctx, name, "manual", 5, manual[0])
	if err != nil {
		t.Fatalf("ExcessBackups(protect): %v", err)
	}
	if len(excess) != 1 || excess[0].ID != manual[1] {
		t.Fatalf("excess with %s protected = %+v; want only %s", manual[0], excess, manual[1])
	}
	if excess, err = st.ExcessBackups(ctx, name, "pre_restore", 0, ""); err != nil || len(excess) != 0 {
		t.Fatalf("pre_restore excess = %+v, %v; the manual ones are not its to prune", excess, err)
	}

	all, err := st.EvictableBackups(ctx)
	if err != nil {
		t.Fatalf("EvictableBackups: %v", err)
	}
	var got []string
	for _, b := range all {
		if b.ServerName == name {
			got = append(got, b.ID)
		}
	}
	want := append(append([]string{}, manual...), copied)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("eviction order = %v\nwant %v (manual oldest first, then the copied archive, never the sole copy)", got, want)
	}

	storeAfter, err := repo.BackupStoreBytes(ctx)
	if err != nil {
		t.Fatalf("BackupStoreBytes: %v", err)
	}
	if storeAfter-storeBefore != 7*10+1000+100 {
		t.Fatalf("store grew by %d, want %d", storeAfter-storeBefore, 7*10+1000+100)
	}

	if at, err := repo.LastBackupRequest(ctx, name, now.Add(-10*time.Minute)); err != nil || !at.IsZero() {
		t.Fatalf("before any request: LastBackupRequest = (%v, %v)", at, err)
	}
	if err := repo.Audit(ctx, api.AuditEntry{Actor: "owner@example.net", Source: "external",
		Action: "backup.create", ServerName: name}); err != nil {
		t.Fatalf("Audit: %v", err)
	}
	at, err := repo.LastBackupRequest(ctx, name, time.Now().Add(-10*time.Minute))
	if err != nil || at.IsZero() || time.Since(at) > time.Minute {
		t.Fatalf("after a request: LastBackupRequest = (%v, %v)", at, err)
	}
	if at, err := repo.LastBackupRequest(ctx, name, time.Now().Add(time.Minute)); err != nil || !at.IsZero() {
		t.Fatalf("a request before since still counted: (%v, %v)", at, err)
	}
}
