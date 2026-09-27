//go:build pgint

package pgint

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/offsite"
	"felis.lolicon.best/internal/reaper"
)

// TestOffsiteCatalogNewestCopyAndKeptRefs: the two reads behind the off-site
// copy's snapshot and sweep. NewestOffsite is the latest copy of any row;
// KeptRefs keeps a present archive past its expiry (the reaper expires it, not
// the sweep) and a deleted one until its expires_at, and nothing after.
func TestOffsiteCatalogNewestCopyAndKeptRefs(t *testing.T) {
	ctx := context.Background()
	cat := offsite.PGCatalog{DB: db}
	sfx := suffix(t)
	server := "offsite-" + sfx
	t.Cleanup(func() { db.ExecContext(ctx, `DELETE FROM world_backups WHERE server_name = $1`, server) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	// Later than any copy another test records.
	latest := now.Add(1000 * reaper.Day)
	insert := func(id, status string, expires time.Time, offsiteAt any) string {
		t.Helper()
		ref := "/archives/" + id + "-" + sfx + ".tar.gz"
		mustExec(t, `INSERT INTO world_backups (id, server_name, backup_ref, size_bytes, reason, status, created_at, expires_at, offsite_at)
			VALUES ($1, $2, $3, 1, 'manual', $4, $5, $6, $7)`,
			id+"-"+sfx, server, ref, status, now.Add(-reaper.Day), expires, offsiteAt)
		return ref
	}
	presentPast := insert("present-past", "present", now.Add(-time.Minute), nil)
	deletedLive := insert("deleted-live", "deleted", now.Add(reaper.Day), now)
	deletedGone := insert("deleted-gone", "deleted", now.Add(-time.Minute), now)
	expiredGone := insert("expired-gone", "expired", now.Add(-time.Minute), nil)
	insert("newest-copy", "deleted", now.Add(-time.Minute), latest)

	got, err := cat.NewestOffsite(ctx)
	if err != nil || !got.Equal(latest) {
		t.Fatalf("NewestOffsite = %v, %v; want %v", got, err, latest)
	}
	refs, err := cat.KeptRefs(ctx, now)
	if err != nil {
		t.Fatalf("KeptRefs: %v", err)
	}
	for _, want := range []string{presentPast, deletedLive} {
		if !slices.Contains(refs, want) {
			t.Errorf("KeptRefs lacks %s", want)
		}
	}
	for _, gone := range []string{deletedGone, expiredGone} {
		if slices.Contains(refs, gone) {
			t.Errorf("KeptRefs keeps %s, past its expiry", gone)
		}
	}
}

// TestOwnerDeletedBackup: ExpireBackup turns one present backup expired with
// expires_at pulled to the delete (never pushed later), after which no read
// finds it, the backup budget drops its bytes, the reaper's retention pass
// lists it whatever its expires_at, and the off-site sweep drops its copy.
func TestOwnerDeletedBackup(t *testing.T) {
	ctx := context.Background()
	sfx := suffix(t)
	server := "owner-del-" + sfx
	t.Cleanup(func() { db.ExecContext(ctx, `DELETE FROM world_backups WHERE server_name = $1`, server) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	insert := func(id string, size int64, expires time.Time) (string, string) {
		t.Helper()
		id += "-" + sfx
		ref := "/archives/" + id + ".tar.gz"
		mustExec(t, `INSERT INTO world_backups (id, server_name, backup_ref, size_bytes, reason, status, created_at, expires_at, offsite_at)
			VALUES ($1, $2, $3, $4, 'manual', 'present', $5, $6, $7)`,
			id, server, ref, size, now.Add(-reaper.Day), expires, now.Add(-time.Hour))
		return id, ref
	}
	later := now.Add(90 * reaper.Day)
	del, delRef := insert("del", 700, later)
	keep, keepRef := insert("keep", 11, later)
	past, _ := insert("past", 13, now.Add(-time.Minute))
	row := func(id string) (string, time.Time) {
		t.Helper()
		var status string
		var expires time.Time
		if err := db.QueryRowContext(ctx, `SELECT status, expires_at FROM world_backups WHERE id = $1`, id).Scan(&status, &expires); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return status, expires
	}

	before, err := repo.BackupStoreBytes(ctx)
	if err != nil {
		t.Fatalf("BackupStoreBytes: %v", err)
	}
	if err := repo.ExpireBackup(ctx, del, now); err != nil {
		t.Fatalf("ExpireBackup: %v", err)
	}
	if s, e := row(del); s != "expired" || !e.Equal(now) {
		t.Fatalf("deleted row = %s expiring %v, want expired expiring %v", s, e, now)
	}
	if s, e := row(keep); s != "present" || !e.Equal(later) {
		t.Fatalf("other row = %s expiring %v, want untouched", s, e)
	}
	if after, err := repo.BackupStoreBytes(ctx); err != nil || after != before-700 {
		t.Fatalf("BackupStoreBytes = %d, %v; want %d", after, err, before-700)
	}
	if _, err := repo.BackupByID(ctx, del); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("BackupByID(deleted) = %v, want ErrNotFound", err)
	}
	for _, id := range []string{del, "missing-" + sfx} {
		if err := repo.ExpireBackup(ctx, id, now); !errors.Is(err, api.ErrNotFound) {
			t.Fatalf("ExpireBackup(%s) = %v, want ErrNotFound", id, err)
		}
	}
	if err := repo.ExpireBackup(ctx, past, now); err != nil {
		t.Fatalf("ExpireBackup(past): %v", err)
	}
	if s, e := row(past); s != "expired" || !e.Equal(now.Add(-time.Minute)) {
		t.Fatalf("past row = %s expiring %v, want expired keeping %v", s, e, now.Add(-time.Minute))
	}

	// The reaper takes it even at a time before its expires_at.
	exp, err := reaper.NewPGStore(db).ListExpiredBackups(ctx, now.Add(-reaper.Day))
	if err != nil {
		t.Fatalf("ListExpiredBackups: %v", err)
	}
	var ids []string
	for _, b := range exp {
		ids = append(ids, b.ID)
	}
	if !slices.Contains(ids, del) || slices.Contains(ids, keep) {
		t.Fatalf("ListExpiredBackups = %v, want %s and not %s", ids, del, keep)
	}

	cat := offsite.PGCatalog{DB: db}
	gone, err := cat.ExpiredRefs(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatalf("ExpiredRefs: %v", err)
	}
	if !slices.Contains(gone, delRef) || slices.Contains(gone, keepRef) {
		t.Fatalf("ExpiredRefs = %v, want %s and not %s", gone, delRef, keepRef)
	}
	kept, err := cat.KeptRefs(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatalf("KeptRefs: %v", err)
	}
	if slices.Contains(kept, delRef) || !slices.Contains(kept, keepRef) {
		t.Fatalf("KeptRefs = %v, want %s and not %s", kept, keepRef, delRef)
	}

	// The undo troubleshooting.md §10 gives, before the reaper has run: the backup
	// is restorable again and the next sync copies it off site anew.
	mustExec(t, `UPDATE world_backups SET status = 'present', expires_at = now() + interval '30 days', offsite_at = NULL
   WHERE id = '`+del+`' AND status = 'expired'`)
	if b, err := repo.BackupByID(ctx, del); err != nil || b.BackupRef != delRef {
		t.Fatalf("BackupByID after the undo = (%+v, %v), want %s", b, err, delRef)
	}
	pending, err := cat.PendingWorlds(ctx)
	if err != nil {
		t.Fatalf("PendingWorlds: %v", err)
	}
	if !slices.ContainsFunc(pending, func(w offsite.WorldBackup) bool { return w.ID == del }) {
		t.Fatalf("PendingWorlds after the undo lacks %s", del)
	}
}
