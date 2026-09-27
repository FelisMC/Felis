//go:build pgint

package pgint

import (
	"context"
	"slices"
	"testing"
	"time"

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
