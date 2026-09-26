package reaper

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// retiring is a candidate whose owner gave it up an hour ago, a day after they
// last played: nowhere near idle, so only the request makes the reaper act.
func retiring(name, owner string, del bool) Candidate {
	return Candidate{Name: name, OwnerID: owner, LastActiveAt: idleBy(Day),
		RetireRequestedAt: idleBy(time.Hour), RetireDelete: del}
}

// A server its owner gave up goes on the next run however recently it was
// played: archived as a "released" backup under the owner, volume deleted, and
// released for someone else to claim, with the request done with.
func TestRetireReleaseArchivesThenReleases(t *testing.T) {
	r, st, cl, _ := newReaper(DefaultConfig(), retiring("alpha", "user-7", false))

	sum := mustRun(t, r)

	want := []string{"hold", "archive", "insert", "deletePVC", "release", "audit:" + ActionReleaseWorld, "unhold"}
	if !reflect.DeepEqual(st.rec.events, want) {
		t.Fatalf("call order = %v, want %v", st.rec.events, want)
	}
	if len(st.backups) != 1 || st.backups[0].reason != ReasonReleased || st.backups[0].owner != "user-7" {
		t.Fatalf("backups = %+v, want one released archive recorded against user-7", st.backups)
	}
	if want := testNow.Add(DefaultConfig().Retention); !st.backups[0].expires.Equal(want) {
		t.Fatalf("archive expires %v, want %v", st.backups[0].expires, want)
	}
	if len(st.audits) != 1 || st.audits[0].FormerOwner != "user-7" {
		t.Fatalf("audits = %+v", st.audits)
	}
	c := st.byName["alpha"]
	if c.OwnerID != "" || !c.RetireRequestedAt.IsZero() {
		t.Fatalf("after release: %+v, want no owner and no pending request", c)
	}
	if len(cl.deletedServers) != 0 || len(st.deleted) != 0 {
		t.Fatalf("a release deleted the server: %v %v", cl.deletedServers, st.deleted)
	}
	if sum.WorldsReaped != 1 || sum.Released != 1 || sum.ServersDeleted != 0 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v", sum)
	}
}

// An admin's deletion archives the world the same way, then removes the
// MinecraftServer that was inspected (by uid) and only after it the row.
func TestRetireDeleteRemovesServerThenRow(t *testing.T) {
	r, st, cl, _ := newReaper(DefaultConfig(), retiring("beta", "user-2", true))

	sum := mustRun(t, r)

	want := []string{"hold", "archive", "insert", "deletePVC", "deleteServer", "deleteRow", "audit:" + ActionDeleteServer, "unhold"}
	if !reflect.DeepEqual(st.rec.events, want) {
		t.Fatalf("call order = %v, want %v", st.rec.events, want)
	}
	if !reflect.DeepEqual(cl.deletedServers, []string{"beta/uid-beta"}) {
		t.Fatalf("deleted servers = %v, want [beta/uid-beta]", cl.deletedServers)
	}
	if !reflect.DeepEqual(st.deleted, []string{"beta"}) || len(st.released) != 0 {
		t.Fatalf("rows deleted %v released %v", st.deleted, st.released)
	}
	if len(st.backups) != 1 || st.backups[0].reason != ReasonReleased || st.backups[0].owner != "user-2" {
		t.Fatalf("backups = %+v", st.backups)
	}
	if sum.WorldsReaped != 1 || sum.ServersDeleted != 1 || sum.Released != 0 {
		t.Fatalf("summary = %+v", sum)
	}
}

// The archive a retirement leaves must be of the world as its owner left it: one
// taken before the request (a reap waiting for its off-site copy, say) is not
// reused, one taken after it is.
func TestRetireReusesOnlyAnArchiveTakenAfterTheRequest(t *testing.T) {
	c := retiring("gamma", "user-3", false)

	r, st, _, ar := newReaper(DefaultConfig(), c)
	st.backups = append(st.backups, &fakeBackup{id: "old", server: "gamma", ref: "ref-old", reason: ReasonInactive,
		status: "present", createdAt: c.RetireRequestedAt.Add(-1), sha: "sha-ref-old"})
	mustRun(t, r)
	if ar.archives != 1 {
		t.Fatalf("archives = %d: an archive older than the request was reused", ar.archives)
	}

	r, st, _, ar = newReaper(DefaultConfig(), c)
	st.backups = append(st.backups, &fakeBackup{id: "new", server: "gamma", ref: "ref-new", reason: ReasonReleased,
		status: "present", createdAt: c.RetireRequestedAt.Add(1), sha: "sha-ref-new"})
	mustRun(t, r)
	if ar.archives != 0 {
		t.Fatalf("archives = %d: the archive taken after the request was not reused", ar.archives)
	}
}

// A retired server that never had a world has nothing to archive: it is
// released (an unowned one too, where an idle one only restarts its clock), or
// deleted.
func TestRetireWithNoWorld(t *testing.T) {
	for _, tc := range []struct {
		name  string
		c     Candidate
		want  []string
		check func(*testing.T, *fakeStore, *fakeCluster, Summary)
	}{
		{"owned release", retiring("a", "user-1", false),
			[]string{"hold", "release", "audit:" + ActionReleaseWorld, "unhold"},
			func(t *testing.T, st *fakeStore, _ *fakeCluster, sum Summary) {
				if sum.Released != 1 || sum.WorldsReaped != 0 {
					t.Errorf("summary = %+v", sum)
				}
			}},
		{"unowned release", retiring("a", "", false),
			[]string{"hold", "release", "audit:" + ActionReleaseWorld, "unhold"},
			func(t *testing.T, st *fakeStore, _ *fakeCluster, _ Summary) {
				if !st.byName["a"].RetireRequestedAt.IsZero() {
					t.Errorf("request still pending: %+v", st.byName["a"])
				}
			}},
		{"delete", retiring("a", "user-1", true),
			[]string{"hold", "deleteServer", "deleteRow", "audit:" + ActionDeleteServer, "unhold"},
			func(t *testing.T, _ *fakeStore, cl *fakeCluster, sum Summary) {
				if sum.ServersDeleted != 1 || sum.WorldsReaped != 0 || len(cl.deletedServers) != 1 {
					t.Errorf("summary = %+v, deleted %v", sum, cl.deletedServers)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st, cl, ar := newReaper(DefaultConfig(), tc.c)
			cl.noWorld = map[string]bool{"world-a-0": true}
			sum := mustRun(t, r)
			if !reflect.DeepEqual(st.rec.events, tc.want) {
				t.Fatalf("call order = %v, want %v", st.rec.events, tc.want)
			}
			if ar.archives != 0 || cl.deletePVCCalls != 0 {
				t.Fatalf("archived %d, deleted %d PVCs of a server with no world", ar.archives, cl.deletePVCCalls)
			}
			tc.check(t, st, cl, sum)
		})
	}
}

// A deletion interrupted after the MinecraftServer went (or one removed by hand)
// is finished from the row alone, unless a world volume is still there: that is
// never deleted unarchived, and the run reports it. A release request with no
// MinecraftServer is left alone as before.
func TestRetireDeleteWithTheServerGone(t *testing.T) {
	r, st, cl, _ := newReaper(DefaultConfig(), retiring("gone", "user-4", true))
	delete(cl.crds, "gone")
	cl.noWorld = map[string]bool{"world-gone-0": true}
	sum := mustRun(t, r)
	if !reflect.DeepEqual(st.rec.events, []string{"deleteRow", "audit:" + ActionDeleteServer}) || sum.ServersDeleted != 1 {
		t.Fatalf("events %v, summary %+v", st.rec.events, sum)
	}

	r, st, cl, _ = newReaper(DefaultConfig(), retiring("gone", "user-4", true))
	delete(cl.crds, "gone")
	sum = mustRun(t, r)
	if len(st.deleted) != 0 || sum.Skipped != 1 || sum.ServersDeleted != 0 {
		t.Fatalf("with its world volume left: deleted %v, summary %+v", st.deleted, sum)
	}

	r, st, cl, _ = newReaper(DefaultConfig(), retiring("gone", "user-4", false))
	delete(cl.crds, "gone")
	sum = mustRun(t, r)
	if len(st.rec.events) != 0 || sum.Skipped != 0 {
		t.Fatalf("release with no server: events %v, summary %+v", st.rec.events, sum)
	}
}

// A failed MinecraftServer delete keeps the row asking for its deletion, and the
// next run finishes it from the archive the first one left.
func TestRetireDeleteServerFailureIsRetried(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(), retiring("delta", "user-5", true))
	cl.deleteServerErr = errors.New("apiserver down")

	sum := mustRun(t, r)
	if sum.Skipped != 1 || len(st.deleted) != 0 || st.byName["delta"] == nil || !st.byName["delta"].RetireDelete {
		t.Fatalf("after a failed delete: summary %+v, deleted %v, row %+v", sum, st.deleted, st.byName["delta"])
	}

	cl.deleteServerErr = nil
	cl.noWorld = map[string]bool{"world-delta-0": true} // the first run deleted it
	sum = mustRun(t, r)
	if ar.archives != 1 || !reflect.DeepEqual(st.deleted, []string{"delta"}) || sum.ServersDeleted != 1 {
		t.Fatalf("retry: archives %d, deleted %v, summary %+v", ar.archives, st.deleted, sum)
	}
}

// A system server is never given up, whatever its row says.
func TestRetireExemptServerIgnored(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(), retiring("lobby", "", true))
	cl.crds["lobby"] = ServerCRD{Exempt: true, PVC: "world-lobby-0", UID: "uid-lobby"}
	sum := mustRun(t, r)
	if len(st.rec.events) != 0 || ar.archives != 0 || sum.Skipped != 0 {
		t.Fatalf("exempt server touched: events %v, summary %+v", st.rec.events, sum)
	}
}

// With the off-site copy required, a retirement waits for it like an idle reap.
func TestRetireWaitsForTheOffsiteCopy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RequireOffsite = true
	r, st, cl, _ := newReaper(cfg, retiring("eps", "user-6", true))
	sum := mustRun(t, r)
	if sum.AwaitingOffsite != 1 || cl.deletePVCCalls != 0 || len(cl.deletedServers) != 0 || len(st.deleted) != 0 {
		t.Fatalf("summary %+v, deletePVC %d, servers %v", sum, cl.deletePVCCalls, cl.deletedServers)
	}
}
