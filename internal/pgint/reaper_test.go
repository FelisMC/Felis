//go:build pgint

package pgint

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
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

func (c *reclaimCluster) HoldWorld(ctx context.Context, _ string) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func (c *reclaimCluster) WorldExists(context.Context, string) (bool, error) { return true, nil }

func (c *reclaimCluster) ClearOwnerSettings(context.Context, string, string) error { return nil }

// DeleteServer is only reached by a retirement, which these tests do not ask for.
func (c *reclaimCluster) DeleteServer(_ context.Context, name, _ string) error {
	return errors.New("unexpected deletion of " + name)
}

type reclaimArchiver struct{ archived []string }

func (a *reclaimArchiver) Archive(_ context.Context, server, _ string) (backup.Archived, error) {
	ref := "/archives/" + server + "-new.tar.gz"
	a.archived = append(a.archived, ref)
	return backup.Archived{Ref: backup.ArchiveRef(ref), Size: 42}, nil
}

func (a *reclaimArchiver) Restore(context.Context, backup.ArchiveRef, string) error { return nil }
func (a *reclaimArchiver) Delete(context.Context, backup.ArchiveRef) error          { return nil }

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
	if ok, err := repo.ClaimServer(ctx, name, first.ID, nil); err != nil || !ok {
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
	if err := st.ReleaseWorld(ctx, name, reapedAt, func(string) error { return nil }); err != nil {
		t.Fatalf("ReleaseWorld: %v", err)
	}
	// Leftover warning stamps must not survive into the next ownership.
	if _, err := db.ExecContext(ctx,
		`UPDATE servers SET warned_3d_at = $2, warned_1d_at = $2 WHERE name = $1`, name, reapedAt); err != nil {
		t.Fatal(err)
	}

	second := newUser(t, "user", "reclaim-b")
	before := time.Now()
	if ok, err := repo.ClaimServer(ctx, name, second.ID, nil); err != nil || !ok {
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

	excess, err := st.ExcessBackups(ctx, name, "", "manual", 5, "")
	if err != nil {
		t.Fatalf("ExcessBackups: %v", err)
	}
	if len(excess) != 2 || excess[0].ID != manual[0] || excess[1].ID != manual[1] {
		t.Fatalf("excess = %+v; want the two oldest manual backups, oldest first", excess)
	}
	// The backup a chained restore will extract is never pruned.
	excess, err = st.ExcessBackups(ctx, name, "", "manual", 5, manual[0])
	if err != nil {
		t.Fatalf("ExcessBackups(protect): %v", err)
	}
	if len(excess) != 1 || excess[0].ID != manual[1] {
		t.Fatalf("excess with %s protected = %+v; want only %s", manual[0], excess, manual[1])
	}
	if excess, err = st.ExcessBackups(ctx, name, "", "pre_restore", 0, ""); err != nil || len(excess) != 0 {
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

func TestBackupReadBack(t *testing.T) {
	ctx := context.Background()
	st := reaper.NewPGStore(db)
	name := "readback-" + suffix(t)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, 100, 128, 1)`,
		name); err != nil {
		t.Fatalf("seed server: %v", err)
	}
	now := time.Now()
	sfx := suffix(t)
	older, newer := "bk-old-"+sfx, "bk-new-"+sfx
	for _, r := range []reaper.BackupRecord{
		{ID: older, ServerName: name, BackupRef: "/archives/" + older + ".tar.gz", SizeBytes: 10,
			Reason: "inactive_15d", ExpiresAt: now.Add(90 * reaper.Day), SHA256: "aa", SkippedEntries: 2},
		{ID: newer, ServerName: name, BackupRef: "/archives/" + newer + ".tar.gz", SizeBytes: 10,
			Reason: "inactive_15d", ExpiresAt: now.Add(90 * reaper.Day)},
	} {
		if err := st.InsertBackup(ctx, r); err != nil {
			t.Fatalf("InsertBackup %s: %v", r.ID, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE world_backups SET created_at = CASE id WHEN $1 THEN $3::timestamptz ELSE $4::timestamptz END WHERE id IN ($1, $2)`,
		older, newer, now.Add(-2*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("age backups: %v", err)
	}

	if f, ok, err := st.FreshBackup(ctx, name, now.Add(-reaper.Day)); err != nil || !ok || f.ID != newer || f.SHA256 != "" {
		t.Fatalf("FreshBackup = (%+v, %v, %v); want %s with no digest yet", f, ok, err, newer)
	}

	// A read-back fills in a missing digest and never rewrites a recorded one.
	checked := now.Add(-time.Minute)
	for id, sum := range map[string]string{newer: "bb", older: "zz"} {
		if err := st.MarkBackupVerified(ctx, id, sum, checked); err != nil {
			t.Fatalf("MarkBackupVerified %s: %v", id, err)
		}
	}
	due := func(before time.Time) map[string]string {
		t.Helper()
		bs, err := st.BackupsToVerify(ctx, before, 100000)
		if err != nil {
			t.Fatalf("BackupsToVerify: %v", err)
		}
		out := map[string]string{}
		for _, b := range bs {
			if b.ServerName == name {
				out[b.ID] = b.SHA256
			}
		}
		return out
	}
	if got := due(checked.Add(-time.Second)); len(got) != 0 {
		t.Fatalf("due before their read-back = %v; want none", got)
	}
	if got := due(checked.Add(time.Second)); got[newer] != "bb" || got[older] != "aa" || len(got) != 2 {
		t.Fatalf("due after their read-back = %v; want %s=bb and %s=aa", got, newer, older)
	}

	corruptAt := now.Add(-30 * time.Second)
	if err := st.MarkBackupCorrupt(ctx, newer, corruptAt); err != nil {
		t.Fatalf("MarkBackupCorrupt: %v", err)
	}
	if err := st.MarkBackupCorrupt(ctx, newer, now); err != nil {
		t.Fatalf("MarkBackupCorrupt again: %v", err)
	}
	var first time.Time
	if err := db.QueryRowContext(ctx, `SELECT corrupt_at FROM world_backups WHERE id = $1`, newer).Scan(&first); err != nil ||
		!first.Equal(corruptAt.Truncate(time.Microsecond)) {
		t.Fatalf("corrupt_at = (%v, %v); want the first finding %v kept", first, err, corruptAt)
	}

	if f, ok, err := st.FreshBackup(ctx, name, now.Add(-reaper.Day)); err != nil || !ok || f.ID != older || f.SHA256 != "aa" {
		t.Fatalf("FreshBackup after corruption = (%+v, %v, %v); want the intact %s", f, ok, err, older)
	}
	if got := due(now.Add(reaper.Day)); len(got) != 1 || got[older] == "" {
		t.Fatalf("due after corruption = %v; want only %s", got, older)
	}
	if b, err := repo.LatestBackup(ctx, name); err != nil || b.ID != older {
		t.Fatalf("LatestBackup = (%+v, %v); want %s", b, err, older)
	}
	if b, err := repo.BackupByID(ctx, newer); err != nil || !b.Corrupt {
		t.Fatalf("BackupByID(corrupt) = (%+v, %v); want Corrupt", b, err)
	}
	if b, err := repo.BackupByID(ctx, older); err != nil || b.Corrupt || b.SHA256 != "aa" {
		t.Fatalf("BackupByID(intact) = (%+v, %v); want not Corrupt, sha256 aa", b, err)
	}

	views, _, err := repo.AllBackups(ctx, api.BackupListOpts{Server: name, Limit: api.MaxBackupListLimit})
	if err != nil {
		t.Fatalf("AllBackups: %v", err)
	}
	seen := 0
	for _, v := range views {
		switch v.ID {
		case newer:
			seen++
			if !v.Corrupt || v.VerifiedAt == nil || v.SkippedEntries != 0 {
				t.Fatalf("listed corrupt backup = %+v; want corrupt, verified_at set", v)
			}
		case older:
			seen++
			if v.Corrupt || v.VerifiedAt == nil || v.SkippedEntries != 2 {
				t.Fatalf("listed intact backup = %+v; want verified_at set and 2 skipped entries", v)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("AllBackups listed %d of the 2 backups", seen)
	}

	if excess, err := st.ExcessBackups(ctx, name, "", "inactive_15d", 1, ""); err != nil || len(excess) != 1 || excess[0].ID != newer {
		t.Fatalf("ExcessBackups(keep 1) = (%+v, %v); want the corrupt %s pruned first", excess, err, newer)
	}

	live := func() map[string]bool {
		t.Helper()
		refs, err := st.LiveBackupRefs(ctx)
		if err != nil {
			t.Fatalf("LiveBackupRefs: %v", err)
		}
		out := map[string]bool{}
		for _, r := range refs {
			out[r] = true
		}
		return out
	}
	if l := live(); !l["/archives/"+older+".tar.gz"] || !l["/archives/"+newer+".tar.gz"] {
		t.Fatalf("live refs miss a present backup")
	}
	if err := st.MarkBackupDeleted(ctx, newer, now); err != nil {
		t.Fatalf("MarkBackupDeleted: %v", err)
	}
	if l := live(); l["/archives/"+newer+".tar.gz"] || !l["/archives/"+older+".tar.gz"] {
		t.Fatalf("live refs after deleting %s still claim its archive", newer)
	}
}

func TestReleasedWorldKeepsItsSize(t *testing.T) {
	ctx := context.Background()
	st := reaper.NewPGStore(db)
	name := "released-" + suffix(t)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, 2000, 4096, 10240)`,
		name); err != nil {
		t.Fatalf("seed server: %v", err)
	}
	first := newUser(t, "user", "released-a")
	if ok, err := repo.ClaimServer(ctx, name, first.ID, nil); err != nil || !ok {
		t.Fatalf("first claim = (%v, %v)", ok, err)
	}
	if err := st.ReleaseWorld(ctx, name, time.Now(), func(string) error { return nil }); err != nil {
		t.Fatalf("ReleaseWorld: %v", err)
	}
	var owner sql.NullString
	var cpu, mem, stor int
	if err := db.QueryRowContext(ctx,
		`SELECT owner_id, cached_cpu_milli, cached_memory_mb, cached_storage_mb FROM servers WHERE name = $1`, name).
		Scan(&owner, &cpu, &mem, &stor); err != nil {
		t.Fatal(err)
	}
	if owner.Valid || cpu != 2000 || mem != 4096 || stor != 10240 {
		t.Fatalf("after ReleaseWorld: owner=%v cache=%d/%d/%d; want no owner, cache 2000/4096/10240", owner, cpu, mem, stor)
	}

	// One CPU of quota does not cover a two-CPU server.
	small := newUser(t, "user", "released-b")
	oneCPU := 1000
	if _, err := repo.SetQuotas(ctx, small.ID, api.QuotaInput{MaxCPUMilli: &oneCPU}, "pgint"); err != nil {
		t.Fatalf("SetQuotas: %v", err)
	}
	if ok, err := repo.ClaimServer(ctx, name, small.ID, nil); !errors.Is(err, api.ErrQuotaExceeded) {
		t.Fatalf("claim over the CPU quota = (%v, %v), want ErrQuotaExceeded", ok, err)
	}

	// A claim that fits counts the server at its size.
	fits := newUser(t, "user", "released-c")
	if ok, err := repo.ClaimServer(ctx, name, fits.ID, nil); err != nil || !ok {
		t.Fatalf("claim within quota = (%v, %v)", ok, err)
	}
	var used int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cached_cpu_milli), 0) FROM servers WHERE owner_id = $1 AND deleted_at IS NULL`, fits.ID).
		Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 2000 {
		t.Fatalf("the new owner's CPU use = %d, want 2000", used)
	}
}

func TestRestartClock(t *testing.T) {
	ctx := context.Background()
	st := reaper.NewPGStore(db)
	name := "clock-" + suffix(t)
	old := time.Now().Add(-20 * reaper.Day).UTC().Truncate(time.Microsecond)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, 100, 128, 1)`,
		name); err != nil {
		t.Fatalf("seed server: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE servers SET last_active_at = $2, warned_3d_at = $2, warned_1d_at = $2 WHERE name = $1`, name, old); err != nil {
		t.Fatalf("age server: %v", err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.RestartClock(ctx, name, at); err != nil {
		t.Fatalf("RestartClock: %v", err)
	}
	var last time.Time
	var w3, w1 sql.NullTime
	var cpu int
	if err := db.QueryRowContext(ctx,
		`SELECT last_active_at, warned_3d_at, warned_1d_at, cached_cpu_milli FROM servers WHERE name = $1`, name).
		Scan(&last, &w3, &w1, &cpu); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !last.Equal(at) || w3.Valid || w1.Valid || cpu != 100 {
		t.Fatalf("after RestartClock: last_active_at=%v warned=%v/%v cpu=%d; want %v, cleared, cache kept", last, w3, w1, cpu, at)
	}
}

type stubOpsAuth struct{}

func (stubOpsAuth) Authenticate(*http.Request) (api.Caller, error) { return api.CallerOps, nil }

type stubRestoreCluster struct {
	api.Cluster
	ensured []string
}

func (c *stubRestoreCluster) GetServer(_ context.Context, name string) (*api.ServerInfo, error) {
	return &api.ServerInfo{Name: name, Phase: "Stopped", DesiredState: string(v1alpha1.DesiredStopped)}, nil
}

func (c *stubRestoreCluster) EnsureWorldVolume(_ context.Context, name string) error {
	c.ensured = append(c.ensured, name)
	return nil
}

func (c *stubRestoreCluster) AcquireMaintenance(context.Context, string, string) error { return nil }
func (c *stubRestoreCluster) ReleaseMaintenance(context.Context, string) error         { return nil }

type stubRestorer struct{ refs []string }

func (r *stubRestorer) Restore(_ context.Context, _, ref string) error {
	r.refs = append(r.refs, ref)
	return nil
}

func TestInternalRestoreScopesBackupsToCurrentOwner(t *testing.T) {
	ctx := context.Background()
	name := "rebuild-" + suffix(t)
	userA := newUser(t, "user", "rebuild-a")
	userB := newUser(t, "user", "rebuild-b")
	seedOwnedServer(t, name, userB.ID, false)

	now := time.Now().UTC().Truncate(time.Microsecond)
	seedBackup := func(id string, owner any, ref string, created time.Time) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`INSERT INTO world_backups (id, server_name, former_owner, backup_ref, size_bytes, reason, status, created_at, expires_at)
			 VALUES ($1, $2, $3, $4, 64, 'inactive_15d', 'present', $5, $6)`,
			id, name, owner, ref, created, created.Add(90*reaper.Day)); err != nil {
			t.Fatalf("seed backup %s: %v", id, err)
		}
	}
	seedBackup("bk-a-"+suffix(t), userA.ID, "/archives/"+name+"-a.tar.gz", now)

	cl := &stubRestoreCluster{}
	restorer := &stubRestorer{}
	h := (&api.API{Repo: repo, Cluster: cl, Internal: stubOpsAuth{}, Restorer: restorer}).InternalHandler()
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/servers/"+name+"/restore", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := call(); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"no_backup"`) || len(restorer.refs) != 0 || len(cl.ensured) != 0 {
		t.Fatalf("with only former owner's backup = %d %s, refs=%v, ensured=%v; want 404 no_backup", w.Code, w.Body.String(), restorer.refs, cl.ensured)
	}

	refB := "/archives/" + name + "-b.tar.gz"
	seedBackup("bk-b-"+suffix(t), userB.ID, refB, now.Add(-time.Hour))
	if w := call(); w.Code != http.StatusAccepted || len(restorer.refs) != 1 || restorer.refs[0] != refB {
		t.Fatalf("with current owner's older backup = %d %s, refs=%v; want 202 and %s", w.Code, w.Body.String(), restorer.refs, refB)
	}

	restorer.refs, cl.ensured = nil, nil
	if _, err := db.ExecContext(ctx, `UPDATE servers SET owner_id = NULL WHERE name = $1`, name); err != nil {
		t.Fatal(err)
	}
	if w := call(); w.Code != http.StatusNotFound || len(restorer.refs) != 0 {
		t.Fatalf("unclaimed server with tenant backups = %d %s, refs=%v; want 404 no_backup", w.Code, w.Body.String(), restorer.refs)
	}

	refUnclaimed := "/archives/" + name + "-unclaimed.tar.gz"
	seedBackup("bk-u-"+suffix(t), nil, refUnclaimed, now.Add(-2*time.Hour))
	if w := call(); w.Code != http.StatusAccepted || len(restorer.refs) != 1 || restorer.refs[0] != refUnclaimed {
		t.Fatalf("unclaimed server with unclaimed backup = %d %s, refs=%v; want 202 and %s", w.Code, w.Body.String(), restorer.refs, refUnclaimed)
	}
}
