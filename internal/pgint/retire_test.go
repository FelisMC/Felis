//go:build pgint

package pgint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/reaper"
)

// ---- retirement (migration 0035) --------------------------------------------------

func retireColumns(t *testing.T, name string) (sql.NullTime, bool) {
	t.Helper()
	var at sql.NullTime
	var del bool
	if err := db.QueryRow(`SELECT retire_requested_at, retire_delete FROM servers WHERE name = $1`, name).Scan(&at, &del); err != nil {
		t.Fatalf("retirement of %s: %v", name, err)
	}
	return at, del
}

// TestRetireRequestContract pins PUT and DELETE /servers/{name}/retirement at the
// SQL: asking again keeps the first request's time, a deletion once asked for
// stays one whoever asks after, only an admin takes a deletion back, and a
// server that is gone is not found.
func TestRetireRequestContract(t *testing.T) {
	ctx := context.Background()
	owner := newUser(t, "user", "retire")
	name, sub := "rt-"+suffix(t), "rts-"+suffix(t)
	if err := repo.SeedServer(ctx, name, sub, 100, 128, 1); err != nil {
		t.Fatalf("SeedServer: %v", err)
	}
	mustExec(t, `UPDATE servers SET owner_id = $2 WHERE name = $1`, name, owner.ID)
	if rec, err := repo.ServerByName(ctx, name); err != nil || rec.Retire != nil {
		t.Fatalf("before any request: ServerByName = %+v, %v; want no retirement", rec, err)
	}

	first, err := repo.RequestRetire(ctx, name, false)
	if err != nil || first.Delete || time.Since(first.RequestedAt) > time.Minute {
		t.Fatalf("RequestRetire = %+v, %v; want a release asked just now", first, err)
	}
	for _, ask := range []struct {
		del, want bool
	}{{false, false}, {true, true}, {false, true}} {
		st, err := repo.RequestRetire(ctx, name, ask.del)
		if err != nil || !st.RequestedAt.Equal(first.RequestedAt) || st.Delete != ask.want {
			t.Fatalf("RequestRetire(delete=%v) = %+v, %v; want the first time %v and delete=%v",
				ask.del, st, err, first.RequestedAt, ask.want)
		}
	}
	for how, get := range map[string]func() (*api.ServerRecord, error){
		"name":      func() (*api.ServerRecord, error) { return repo.ServerByName(ctx, name) },
		"subdomain": func() (*api.ServerRecord, error) { return repo.ServerBySubdomain(ctx, sub) },
	} {
		rec, err := get()
		if err != nil || rec.Retire == nil || !rec.Retire.Delete || !rec.Retire.RequestedAt.Equal(first.RequestedAt) {
			t.Fatalf("server by %s = %+v, %v; want the pending deletion", how, rec, err)
		}
	}

	if err := repo.CancelRetire(ctx, name, false); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("the owner cancelling a deletion = %v, want ErrConflict", err)
	}
	if at, del := retireColumns(t, name); !at.Valid || !at.Time.Equal(first.RequestedAt) || !del {
		t.Fatalf("a refused cancel changed the request: at=%v delete=%v", at, del)
	}
	if err := repo.CancelRetire(ctx, name, true); err != nil {
		t.Fatalf("an admin cancelling a deletion: %v", err)
	}
	if at, del := retireColumns(t, name); at.Valid || del {
		t.Fatalf("after the cancel: at=%v delete=%v; want no request", at, del)
	}
	if rec, err := repo.ServerByName(ctx, name); err != nil || rec.Retire != nil {
		t.Fatalf("after the cancel: ServerByName = %+v, %v", rec, err)
	}

	// A give-up is the owner's to take back, and a new one starts its own clock.
	again, err := repo.RequestRetire(ctx, name, false)
	if err != nil || again.Delete || !again.RequestedAt.After(first.RequestedAt) {
		t.Fatalf("a new request = %+v, %v; want a release after %v", again, err, first.RequestedAt)
	}
	if err := repo.CancelRetire(ctx, name, false); err != nil {
		t.Fatalf("the owner cancelling a give-up: %v", err)
	}
	if at, del := retireColumns(t, name); at.Valid || del {
		t.Fatalf("after the owner's cancel: at=%v delete=%v", at, del)
	}

	gone := "rtg-" + suffix(t)
	seedOwnedServer(t, gone, owner.ID, true)
	for _, n := range []string{gone, "rt-none-" + suffix(t)} {
		if st, err := repo.RequestRetire(ctx, n, true); !errors.Is(err, api.ErrNotFound) {
			t.Fatalf("RequestRetire(%s) = %+v, %v; want ErrNotFound", n, st, err)
		}
		if err := repo.CancelRetire(ctx, n, true); !errors.Is(err, api.ErrNotFound) {
			t.Fatalf("CancelRetire(%s) = %v; want ErrNotFound", n, err)
		}
	}
	if at, _ := retireColumns(t, gone); at.Valid {
		t.Fatalf("a deleted server's row took a request")
	}
}

// TestRetiringServerIsNotClaimable: a server an admin is releasing or deleting
// cannot be claimed, even unowned, and the claim lists say so; only its owner is
// told of the request, and the admin fleet read sees it on every row.
func TestRetiringServerIsNotClaimable(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "rtclaim")
	other := newUser(t, "user", "rtclaim-other")
	sfx := suffix(t)
	free, freeRetiring, mine, mineRetiring, theirs := "rcf-"+sfx, "rcr-"+sfx, "rco-"+sfx, "rcm-"+sfx, "rct-"+sfx
	for _, n := range []string{free, freeRetiring} {
		mustExec(t, `INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, 100, 128, 1)`, n)
	}
	seedOwnedServer(t, mine, u.ID, false)
	seedOwnedServer(t, mineRetiring, u.ID, false)
	seedOwnedServer(t, theirs, other.ID, false)
	released, err := repo.RequestRetire(ctx, freeRetiring, false)
	if err != nil {
		t.Fatalf("RequestRetire(%s): %v", freeRetiring, err)
	}
	deleting, err := repo.RequestRetire(ctx, mineRetiring, true)
	if err != nil {
		t.Fatalf("RequestRetire(%s): %v", mineRetiring, err)
	}
	if _, err := repo.RequestRetire(ctx, theirs, false); err != nil {
		t.Fatalf("RequestRetire(%s): %v", theirs, err)
	}

	if ok, err := repo.ClaimServer(ctx, freeRetiring, u.ID); err != nil || ok {
		t.Fatalf("claiming a server being released = (%v, %v), want (false, nil)", ok, err)
	}
	if o := serverOwner(t, freeRetiring); o != "" {
		t.Fatalf("the refused claim left owner %q", o)
	}

	views, err := repo.MyServers(ctx, u.ID)
	if err != nil {
		t.Fatalf("MyServers: %v", err)
	}
	got := map[string]string{}
	for _, v := range views {
		if strings.HasSuffix(v.Name, sfx) {
			retiring := "none"
			if v.Retiring != nil {
				retiring = fmt.Sprintf("delete=%v", v.Retiring.Delete)
				if !v.Retiring.RequestedAt.Equal(deleting.RequestedAt) {
					retiring += " at another time"
				}
			}
			got[v.Name] = fmt.Sprintf("owned=%v claimable=%v retiring=%s", v.Owned, v.Claimable, retiring)
		}
	}
	want := map[string]string{
		free:         "owned=false claimable=true retiring=none",
		freeRetiring: "owned=false claimable=false retiring=none",
		mine:         "owned=true claimable=false retiring=none",
		mineRetiring: "owned=true claimable=false retiring=delete=true",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("MyServers = %v\nwant       %v", got, want)
	}

	owners, err := repo.ServerOwners(ctx)
	if err != nil {
		t.Fatalf("ServerOwners: %v", err)
	}
	for n, w := range map[string]*api.RetireState{free: nil, mine: nil, freeRetiring: &released, mineRetiring: &deleting} {
		r := owners[n].Retire
		if (r == nil) != (w == nil) || (r != nil && (r.Delete != w.Delete || !r.RequestedAt.Equal(w.RequestedAt))) {
			t.Errorf("ServerOwners[%s].Retire = %+v, want %+v", n, r, w)
		}
	}
	if r := owners[theirs].Retire; r == nil || r.Delete {
		t.Errorf("ServerOwners[%s].Retire = %+v, want another owner's give-up", theirs, r)
	}

	// Taken back, the server may be claimed again.
	if err := repo.CancelRetire(ctx, freeRetiring, true); err != nil {
		t.Fatalf("CancelRetire: %v", err)
	}
	if ok, err := repo.ClaimServer(ctx, freeRetiring, u.ID); err != nil || !ok {
		t.Fatalf("claim after the cancel = (%v, %v)", ok, err)
	}
	for _, n := range []string{mineRetiring, theirs} {
		if err := repo.CancelRetire(ctx, n, true); err != nil {
			t.Fatalf("CancelRetire(%s): %v", n, err)
		}
	}
}

// TestRetireReaperStore pins the reaper's reads and writes of a retirement: the
// request reaches its candidates, a retirement's archive holds the world as an
// idle reap's does and is never evicted while it is the only copy, releasing the
// world finishes a give-up and leaves a deletion for the next run, and marking
// the row deleted takes only a server asked to be deleted.
func TestRetireReaperStore(t *testing.T) {
	ctx := context.Background()
	st := reaper.NewPGStore(db)
	u := newUser(t, "user", "rtstore")
	sfx := suffix(t)
	given, doomed, plain := "rsg-"+sfx, "rsd-"+sfx, "rsp-"+sfx
	for _, n := range []string{given, doomed, plain} {
		seedOwnedServer(t, n, u.ID, false)
	}
	givenAt, err := repo.RequestRetire(ctx, given, false)
	if err != nil {
		t.Fatalf("RequestRetire(%s): %v", given, err)
	}
	doomedAt, err := repo.RequestRetire(ctx, doomed, true)
	if err != nil {
		t.Fatalf("RequestRetire(%s): %v", doomed, err)
	}

	cands, err := st.ListActiveServers(ctx)
	if err != nil {
		t.Fatalf("ListActiveServers: %v", err)
	}
	seen := map[string]reaper.Candidate{}
	for _, c := range cands {
		seen[c.Name] = c
	}
	for n, w := range map[string]api.RetireState{given: givenAt, doomed: doomedAt, plain: {}} {
		c, ok := seen[n]
		if !ok || !c.RetireRequestedAt.Equal(w.RequestedAt) || c.RetireDelete != w.Delete {
			t.Errorf("candidate %s = %+v (listed %v); want retirement %+v", n, c, ok, w)
		}
	}

	// A retirement's archive is the reaper's own: it holds the world, and it is
	// kept like an idle reap's until it has its off-site copy.
	now := time.Now()
	backup := func(server, id, reason string, created time.Time, offsite bool) {
		t.Helper()
		var offsiteAt any
		if offsite {
			offsiteAt = created
		}
		mustExec(t, `INSERT INTO world_backups (id, server_name, backup_ref, size_bytes, reason, status, created_at, expires_at, offsite_at)
			VALUES ($1, $2, $3, 1, $4, 'present', $5, $6, $7)`,
			id, server, "/archives/"+id+".tar.gz", reason, created, created.Add(90*reaper.Day), offsiteAt)
	}
	manualID, soleID, copiedID := "bk-rsm-"+sfx, "bk-rss-"+sfx, "bk-rsc-"+sfx
	backup(plain, manualID, "manual", now.Add(-time.Minute), false)
	if f, ok, err := st.FreshBackup(ctx, plain, now.Add(-time.Hour)); err != nil || ok {
		t.Fatalf("FreshBackup over a manual backup = (%+v, %v, %v); only the reaper's archives count", f, ok, err)
	}
	backup(plain, soleID, reaper.ReasonReleased, now.Add(-3*time.Minute), false)
	backup(plain, copiedID, reaper.ReasonReleased, now.Add(-2*time.Minute), true)
	f, ok, err := st.FreshBackup(ctx, plain, now.Add(-time.Hour))
	if err != nil || !ok || f.ID != copiedID || !f.Offsite {
		t.Fatalf("FreshBackup = (%+v, %v, %v); want the copied retirement archive %s", f, ok, err, copiedID)
	}
	if f, ok, err := st.FreshBackup(ctx, plain, now); err != nil || ok {
		t.Fatalf("FreshBackup since now = (%+v, %v, %v); an archive older than since holds no world", f, ok, err)
	}
	all, err := st.EvictableBackups(ctx)
	if err != nil {
		t.Fatalf("EvictableBackups: %v", err)
	}
	var order []string
	for _, b := range all {
		if b.ServerName == plain {
			order = append(order, b.ID)
		}
	}
	if strings.Join(order, ",") != manualID+","+copiedID {
		t.Fatalf("eviction order = %v; want %s, then %s, and never the only copy %s", order, manualID, copiedID, soleID)
	}

	// Each server keeps an allowlist entry and an extra alias to see what goes.
	for _, n := range []string{given, doomed, plain} {
		mustExec(t, `INSERT INTO server_aliases (subdomain, server_name) VALUES ($1, $2)`, n+"-a", n)
		mustExec(t, `INSERT INTO server_allowlist (server_name, mc_uuid) VALUES ($1, $2)`, n, testUUID(t))
	}
	state := func(n string) string {
		t.Helper()
		var owner sql.NullString
		var deleted, retireAt sql.NullTime
		var del bool
		var aliases, allow int
		if err := db.QueryRowContext(ctx,
			`SELECT owner_id, deleted_at, retire_requested_at, retire_delete,
			        (SELECT count(*) FROM server_aliases WHERE server_name = s.name),
			        (SELECT count(*) FROM server_allowlist WHERE server_name = s.name)
			 FROM servers s WHERE name = $1`, n).Scan(&owner, &deleted, &retireAt, &del, &aliases, &allow); err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		return fmt.Sprintf("owner=%v deleted=%v retire=%v/%v aliases=%d allow=%d",
			owner.Valid, deleted.Valid, retireAt.Valid, del, aliases, allow)
	}

	// Only a server asked to be deleted loses its row.
	for _, n := range []string{given, plain} {
		before := state(n)
		if err := st.DeleteServerRow(ctx, n, now); err != nil {
			t.Fatalf("DeleteServerRow(%s): %v", n, err)
		}
		if got := state(n); got != before {
			t.Fatalf("DeleteServerRow(%s) touched a server nobody is deleting: %s, was %s", n, got, before)
		}
	}

	// Releasing the world finishes a give-up; a deletion stays pending.
	for _, n := range []string{given, doomed} {
		if err := st.ReleaseWorld(ctx, n, now); err != nil {
			t.Fatalf("ReleaseWorld(%s): %v", n, err)
		}
	}
	if got, want := state(given), "owner=false deleted=false retire=false/false aliases=1 allow=0"; got != want {
		t.Fatalf("given up and released: %s, want %s", got, want)
	}
	if got, want := state(doomed), "owner=false deleted=false retire=true/true aliases=1 allow=0"; got != want {
		t.Fatalf("released while being deleted: %s, want %s", got, want)
	}
	if at, _ := retireColumns(t, doomed); !at.Time.Equal(doomedAt.RequestedAt) {
		t.Fatalf("the release moved the deletion's request time to %v, want %v", at.Time, doomedAt.RequestedAt)
	}

	mustExec(t, `INSERT INTO server_allowlist (server_name, mc_uuid) VALUES ($1, $2)`, doomed, testUUID(t))
	if err := st.DeleteServerRow(ctx, doomed, now); err != nil {
		t.Fatalf("DeleteServerRow(%s): %v", doomed, err)
	}
	if got, want := state(doomed), "owner=false deleted=true retire=false/false aliases=0 allow=0"; got != want {
		t.Fatalf("deleted: %s, want %s", got, want)
	}
	if got, want := state(plain), "owner=true deleted=false retire=false/false aliases=1 allow=1"; got != want {
		t.Fatalf("a bystander changed: %s, want %s", got, want)
	}
}

// fleetCluster knows a set of servers by uid; every other row in the shared
// schema reads as a server whose CRD is gone and whose world volume is still
// there, which the reaper never touches.
type fleetCluster struct {
	uids          map[string]string
	deletedPVC    []string
	deletedServer []string
}

func (c *fleetCluster) Inspect(_ context.Context, name string) (reaper.ServerCRD, error) {
	uid, ok := c.uids[name]
	if !ok {
		return reaper.ServerCRD{}, reaper.ErrNotFound
	}
	return reaper.ServerCRD{PVC: reaper.WorldPVCName(name), UID: uid}, nil
}

func (c *fleetCluster) HoldWorld(ctx context.Context, _ string) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func (c *fleetCluster) WorldExists(_ context.Context, pvc string) (bool, error) {
	for _, gone := range c.deletedPVC {
		if gone == pvc {
			return false, nil
		}
	}
	return true, nil
}

func (c *fleetCluster) DeletePVC(_ context.Context, pvc string) error {
	c.deletedPVC = append(c.deletedPVC, pvc)
	return nil
}

func (c *fleetCluster) DeleteServer(_ context.Context, name, uid string) error {
	if c.uids[name] != uid {
		return fmt.Errorf("DeleteServer(%s, %s): the server has uid %s", name, uid, c.uids[name])
	}
	c.deletedServer = append(c.deletedServer, name)
	delete(c.uids, name)
	return nil
}

// TestRetirementThroughTheReaper runs the reaper over a give-up and a deletion
// felis-api recorded, beside a server played minutes ago: the two go on the
// first run however recently they were played, each archived first (in name
// order, as the reaper lists them), and a second
// run finds nothing left to do. The deleted server's name and subdomain are free
// for a new server, which starts clean.
func TestRetirementThroughTheReaper(t *testing.T) {
	ctx := context.Background()
	st := reaper.NewPGStore(db)
	u := newUser(t, "user", "rtreap")
	sfx := suffix(t)
	given, doomed, bystander := "rrg-"+sfx, "rrd-"+sfx, "rrb-"+sfx
	cl := &fleetCluster{uids: map[string]string{}}
	for _, n := range []string{given, doomed, bystander} {
		if err := repo.SeedServer(ctx, n, n+"-s", 100, 128, 1); err != nil {
			t.Fatalf("SeedServer(%s): %v", n, err)
		}
		mustExec(t, `UPDATE servers SET owner_id = $2, claimed_at = now() - interval '30 days', last_active_at = now() - interval '5 minutes'
			WHERE name = $1`, n, u.ID)
		mustExec(t, `INSERT INTO server_allowlist (server_name, mc_uuid) VALUES ($1, $2)`, n, testUUID(t))
		cl.uids[n] = "uid-" + n
	}
	if _, err := repo.RequestRetire(ctx, given, false); err != nil {
		t.Fatalf("RequestRetire(%s): %v", given, err)
	}
	if _, err := repo.RequestRetire(ctx, doomed, true); err != nil {
		t.Fatalf("RequestRetire(%s): %v", doomed, err)
	}

	ar := &reclaimArchiver{}
	r := &reaper.Reaper{Cfg: reaper.DefaultConfig(), Store: st, Cluster: cl, Archiver: ar}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	mineOnly := func(refs []string) string {
		var out []string
		for _, ref := range refs {
			for _, n := range []string{given, doomed, bystander} {
				if strings.Contains(ref, n) {
					out = append(out, strings.Replace(ref, n, map[string]string{given: "given", doomed: "doomed", bystander: "bystander"}[n], 1))
				}
			}
		}
		return strings.Join(out, " ")
	}
	if got, want := mineOnly(cl.deletedPVC), "world-doomed-0 world-given-0"; got != want {
		t.Fatalf("volumes deleted: %q, want %q", got, want)
	}
	if got, want := mineOnly(cl.deletedServer), "doomed"; got != want {
		t.Fatalf("servers deleted: %q, want %q", got, want)
	}
	if got, want := mineOnly(ar.archived), "/archives/doomed-new.tar.gz /archives/given-new.tar.gz"; got != want {
		t.Fatalf("archived: %q, want %q", got, want)
	}

	type row struct {
		owner          sql.NullString
		deleted, retir sql.NullTime
		aliases, allow int
		archives       string
		audit          string
	}
	read := func(n string) row {
		t.Helper()
		var r row
		if err := db.QueryRowContext(ctx,
			`SELECT owner_id, deleted_at, retire_requested_at,
			        (SELECT count(*) FROM server_aliases WHERE server_name = s.name),
			        (SELECT count(*) FROM server_allowlist WHERE server_name = s.name),
			        (SELECT COALESCE(string_agg(reason || ':' || status || ':' || COALESCE(former_owner, ''), ','), '') FROM world_backups WHERE server_name = s.name),
			        (SELECT COALESCE(string_agg(action, ',' ORDER BY id), '') FROM audit_logs WHERE server_name = s.name AND source = 'reaper')
			 FROM servers s WHERE name = $1`, n).Scan(&r.owner, &r.deleted, &r.retir, &r.aliases, &r.allow, &r.archives, &r.audit); err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		return r
	}
	g := read(given)
	if g.owner.Valid || g.deleted.Valid || g.retir.Valid || g.aliases != 1 || g.allow != 0 ||
		g.archives != "released:present:"+u.ID || g.audit != reaper.ActionReleaseWorld {
		t.Fatalf("given up: %+v; want released, still listed at its subdomain, its archive kept as the owner's", g)
	}
	d := read(doomed)
	if d.owner.Valid || !d.deleted.Valid || d.retir.Valid || d.aliases != 0 || d.allow != 0 ||
		d.archives != "released:present:"+u.ID || d.audit != reaper.ActionDeleteServer {
		t.Fatalf("deleted: %+v; want the row marked deleted, its subdomain freed, its archive kept", d)
	}
	b := read(bystander)
	if b.owner.String != u.ID || b.deleted.Valid || b.aliases != 1 || b.allow != 1 || b.archives != "" || b.audit != "" {
		t.Fatalf("the bystander changed: %+v", b)
	}

	// Nothing is left for the next run.
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if got := mineOnly(ar.archived) + " | " + mineOnly(cl.deletedServer); got != "/archives/doomed-new.tar.gz /archives/given-new.tar.gz | doomed" {
		t.Fatalf("the second run did more: %s", got)
	}

	if err := repo.SeedServer(ctx, doomed, doomed+"-s", 200, 256, 2); err != nil {
		t.Fatalf("a new server under the deleted one's name: %v", err)
	}
	if at, del := retireColumns(t, doomed); at.Valid || del {
		t.Fatalf("the new server inherited a retirement: at=%v delete=%v", at, del)
	}
	if rec, err := repo.ServerBySubdomain(ctx, doomed+"-s"); err != nil || rec.Name != doomed || rec.OwnerID != "" || rec.Retire != nil {
		t.Fatalf("the new server = %+v, %v; want it unowned at the freed subdomain", rec, err)
	}
}
