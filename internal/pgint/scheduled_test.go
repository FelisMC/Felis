//go:build pgint

package pgint

import (
	"context"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/reaper"
)

// TestScheduledBackupCandidates pins the SQL behind felis-api's scheduled
// backups: an owned world joined since its owner's newest intact scheduled
// backup is due once that backup is older than the period, worlds without one
// first. A previous owner's, a corrupt, a deleted, a pre-claim or a manual
// backup is no restore point of the current owner's world.
func TestScheduledBackupCandidates(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "sched")
	prev := newUser(t, "user", "sched-prev")
	now := time.Now().UTC()
	sfx := suffix(t)
	server := func(tag, owner string, claimed, active time.Time, deleted bool) string {
		t.Helper()
		name := "sch-" + tag + "-" + sfx
		var deletedAt any
		if deleted {
			deletedAt = now
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO servers (name, owner_id, claimed_at, last_active_at, deleted_at, cached_cpu_milli, cached_memory_mb, cached_storage_mb)
			 VALUES ($1, NULLIF($2, ''), $3, $4, $5, 100, 128, 1)`,
			name, owner, claimed, active, deletedAt); err != nil {
			t.Fatalf("seed server %s: %v", name, err)
		}
		return name
	}
	n := 0
	backup := func(server, owner, reason, status string, created time.Time, corrupt bool) {
		t.Helper()
		n++
		id := "bk-sch-" + sfx + "-" + string(rune('a'+n))
		var corruptAt any
		if corrupt {
			corruptAt = created
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO world_backups (id, server_name, former_owner, backup_ref, size_bytes, reason, status, created_at, expires_at, corrupt_at)
			 VALUES ($1, $2, NULLIF($3, ''), $4, 1, $5, $6, $7, $8, $9)`,
			id, server, owner, "/archives/"+id+".tar.gz", reason, status, created, created.Add(90*reaper.Day), corruptAt); err != nil {
			t.Fatalf("seed backup of %s: %v", server, err)
		}
	}
	h := time.Hour
	claimed := now.Add(-10 * reaper.Day)

	fresh := server("fresh", u.ID, claimed, now.Add(-h), false)
	stale := server("stale", u.ID, claimed, now.Add(-h), false)
	backup(stale, u.ID, "scheduled", "present", now.Add(-30*h), false)
	recent := server("recent", u.ID, claimed, now.Add(-h), false)
	backup(recent, u.ID, "scheduled", "present", now.Add(-2*h), false)
	idle := server("idle", u.ID, claimed, now.Add(-40*h), false)
	backup(idle, u.ID, "scheduled", "present", now.Add(-30*h), false)
	prevOwner := server("prevowner", u.ID, claimed, now.Add(-h), false)
	backup(prevOwner, prev.ID, "scheduled", "present", now.Add(-2*h), false)
	preClaim := server("preclaim", u.ID, now.Add(-2*h), now.Add(-2*h), false)
	backup(preClaim, u.ID, "scheduled", "present", now.Add(-3*h), false)
	corrupt := server("corrupt", u.ID, claimed, now.Add(-h), false)
	backup(corrupt, u.ID, "scheduled", "present", now.Add(-30*time.Minute), true)
	deletedBk := server("deletedbk", u.ID, claimed, now.Add(-h), false)
	backup(deletedBk, u.ID, "scheduled", "deleted", now.Add(-30*time.Minute), false)
	manual := server("manual", u.ID, claimed, now.Add(-h), false)
	backup(manual, u.ID, "manual", "present", now.Add(-30*time.Minute), false)
	server("unowned", "", claimed, now.Add(-h), false)
	server("gone", u.ID, claimed, now.Add(-h), true)

	got, err := repo.ScheduledBackupCandidates(ctx, now.Add(-24*h))
	if err != nil {
		t.Fatalf("ScheduledBackupCandidates: %v", err)
	}
	var mine []string
	for _, c := range got {
		if strings.HasSuffix(c.Name, sfx) {
			if c.OwnerID != u.ID {
				t.Fatalf("candidate %+v; want owner %s", c, u.ID)
			}
			mine = append(mine, c.Name)
		}
	}
	want := []string{corrupt, deletedBk, fresh, manual, preClaim, prevOwner, stale}
	if strings.Join(mine, " ") != strings.Join(want, " ") {
		t.Fatalf("due = %v\nwant  %v (worlds without a point by name, then the longest waiting)", mine, want)
	}
}

// TestExcessBackupsPerOwner pins that the backup Job's keep-N prune counts only
// the new backup's owner's backups: the ones a previous owner took of the same
// world stay theirs to restore until their own retention runs out.
func TestExcessBackupsPerOwner(t *testing.T) {
	ctx := context.Background()
	st := reaper.NewPGStore(db)
	u := newUser(t, "user", "keep")
	prev := newUser(t, "user", "keep-prev")
	sfx := suffix(t)
	name := "keep-" + sfx
	if _, err := db.ExecContext(ctx,
		`INSERT INTO servers (name, owner_id, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, $2, 100, 128, 1)`,
		name, u.ID); err != nil {
		t.Fatalf("seed server: %v", err)
	}
	now := time.Now().UTC()
	var ids []string
	for i, owner := range []string{prev.ID, prev.ID, prev.ID, u.ID, u.ID} {
		id := "bk-keep-" + sfx + "-" + string(rune('0'+i))
		ids = append(ids, id)
		created := now.Add(time.Duration(i-5) * time.Hour)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO world_backups (id, server_name, former_owner, backup_ref, size_bytes, reason, status, created_at, expires_at)
			 VALUES ($1, $2, $3, $4, 1, 'scheduled', 'present', $5, $6)`,
			id, name, owner, "/archives/"+id+".tar.gz", created, created.Add(90*reaper.Day)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	excess, err := st.ExcessBackups(ctx, name, u.ID, "scheduled", 1, "")
	if err != nil {
		t.Fatalf("ExcessBackups: %v", err)
	}
	if len(excess) != 1 || excess[0].ID != ids[3] {
		t.Fatalf("excess for the owner = %+v; want only %s", excess, ids[3])
	}
	excess, err = st.ExcessBackups(ctx, name, prev.ID, "scheduled", 1, "")
	if err != nil {
		t.Fatalf("ExcessBackups(previous owner): %v", err)
	}
	if len(excess) != 2 || excess[0].ID != ids[0] || excess[1].ID != ids[1] {
		t.Fatalf("excess for the previous owner = %+v; want %s, %s", excess, ids[0], ids[1])
	}
}
