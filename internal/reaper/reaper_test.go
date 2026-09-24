package reaper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"testing"
	"time"

	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/metrics"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// testNow is the frozen clock for every hermetic case. Idle is expressed as an
// offset back from here.
var testNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func idleBy(d time.Duration) time.Time { return testNow.Add(-d) }

// recorder captures the cross-fake call order so a test can assert the strict
// hold→archive→insert→deletePVC→release→audit→unhold sequence of red line ④.
type recorder struct{ events []string }

func (r *recorder) add(e string) { r.events = append(r.events, e) }

// ---- fake backup.WorldArchiver -------------------------------------------

type fakeArchiver struct {
	rec        *recorder
	onArchive  func()
	archiveErr error
	deleteErr  error
	archives   int
	deletes    []backup.ArchiveRef
	seq        int
}

func (f *fakeArchiver) Archive(_ context.Context, server, _ string) (backup.Archived, error) {
	if f.archiveErr != nil {
		return backup.Archived{}, f.archiveErr
	}
	f.archives++
	f.seq++
	f.rec.add("archive")
	if f.onArchive != nil {
		f.onArchive()
	}
	ref := fmt.Sprintf("ref-%s-%d", server, f.seq)
	return backup.Archived{Ref: backup.ArchiveRef(ref), Size: 10, SHA256: "sha-" + ref}, nil
}

// checkingArchiver is a fakeArchiver that also reads archives back
// (backup.Verifier) and sweeps its store (backup.Sweeper).
type checkingArchiver struct {
	*fakeArchiver
	corrupt   map[string]bool // refs that no longer read back
	verifyErr error           // every read-back fails with this
	verified  []string

	sweepErr      error
	sweepLive     func(backup.ArchiveRef) bool
	sweepCutoffs  []time.Time
	sweepRemoved  []string
	sweepOrphaned []string
}

func (c *checkingArchiver) Verify(_ context.Context, ref backup.ArchiveRef, want string) (string, error) {
	if c.verifyErr != nil {
		return "", c.verifyErr
	}
	c.verified = append(c.verified, string(ref))
	c.rec.add("verify")
	if c.corrupt[string(ref)] {
		return "", fmt.Errorf("%w: %s", backup.ErrCorrupt, ref)
	}
	return "sha-" + string(ref), nil
}

func (c *checkingArchiver) Sweep(_ context.Context, live func(backup.ArchiveRef) bool, partialBefore, orphanBefore time.Time) (backup.Swept, error) {
	c.sweepLive = live
	c.sweepCutoffs = []time.Time{partialBefore, orphanBefore}
	return backup.Swept{Removed: c.sweepRemoved, Orphans: c.sweepOrphaned, OrphanBytes: int64(len(c.sweepOrphaned))}, c.sweepErr
}

func (f *fakeArchiver) Restore(context.Context, backup.ArchiveRef, string) error { return nil }

func (f *fakeArchiver) Delete(_ context.Context, ref backup.ArchiveRef) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletes = append(f.deletes, ref)
	f.rec.add("delete")
	return nil
}

// ---- fake Cluster ---------------------------------------------------------

type fakeCluster struct {
	rec            *recorder
	crds           map[string]ServerCRD
	inspectErr     map[string]error
	deletePVCErr   error
	deletePVCCalls int
	deletedPVCs    []string

	notQuiet map[string]bool // HoldWorld refuses these with ErrNotQuiet
	holdErr  error           // HoldWorld fails with this
	noWorld  map[string]bool // PVCs that do not exist
	held     map[string]bool // servers held right now
	holds    []string
	lost     context.CancelCauseFunc
}

func (c *fakeCluster) HoldWorld(ctx context.Context, name string) (context.Context, func(), error) {
	if c.holdErr != nil {
		return nil, nil, c.holdErr
	}
	if c.notQuiet[name] {
		return nil, nil, fmt.Errorf("%w: still stopping", ErrNotQuiet)
	}
	if c.held == nil {
		c.held = map[string]bool{}
	}
	c.held[name] = true
	c.holds = append(c.holds, name)
	c.rec.add("hold")
	held, cancel := context.WithCancelCause(ctx)
	c.lost = cancel
	return held, func() {
		cancel(nil)
		delete(c.held, name)
		c.rec.add("unhold")
	}, nil
}

func (c *fakeCluster) WorldExists(_ context.Context, pvc string) (bool, error) {
	return !c.noWorld[pvc], nil
}

func (c *fakeCluster) Inspect(_ context.Context, name string) (ServerCRD, error) {
	if e := c.inspectErr[name]; e != nil {
		return ServerCRD{}, e
	}
	crd, ok := c.crds[name]
	if !ok {
		return ServerCRD{}, ErrNotFound
	}
	return crd, nil
}

func (c *fakeCluster) DeletePVC(ctx context.Context, pvc string) error {
	c.deletePVCCalls++
	if c.deletePVCErr != nil {
		return c.deletePVCErr
	}
	if len(c.held) == 0 || ctx.Err() != nil {
		return fmt.Errorf("deleted %s without holding its world", pvc)
	}
	c.deletedPVCs = append(c.deletedPVCs, pvc)
	c.rec.add("deletePVC")
	return nil
}

// ---- fake Store -----------------------------------------------------------

type fakeBackup struct {
	id, server, ref    string
	reason             string
	size               int64
	status             string // present | deleted
	createdAt, expires time.Time
	offsite            bool
	sha                string
	skipped            int
	verifiedAt         time.Time
	corruptAt          time.Time
}

type fakeStore struct {
	rec       *recorder
	clock     time.Time
	order     []string
	byName    map[string]*Candidate
	backups   []*fakeBackup
	audits    []AuditRecord
	released  []string
	listErr   error
	insertErr error
	liveErr   error
	idn       int
}

func (s *fakeStore) ListActiveServers(context.Context) ([]Candidate, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]Candidate, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, *s.byName[n])
	}
	return out, nil
}

func (s *fakeStore) FreshBackup(_ context.Context, server string, since time.Time) (Fresh, bool, error) {
	var found *fakeBackup
	for _, b := range s.backups {
		if b.server == server && b.status == "present" && b.reason == ReasonInactive && !b.createdAt.Before(since) && b.corruptAt.IsZero() {
			if found == nil || (b.offsite && !found.offsite) {
				found = b
			}
		}
	}
	if found == nil {
		return Fresh{}, false, nil
	}
	return Fresh{ID: found.id, Ref: found.ref, SHA256: found.sha, Offsite: found.offsite}, true, nil
}

func (s *fakeStore) InsertBackup(_ context.Context, rec BackupRecord) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.backups = append(s.backups, &fakeBackup{
		id: rec.ID, server: rec.ServerName, ref: rec.BackupRef, reason: rec.Reason, size: rec.SizeBytes,
		status: "present", createdAt: s.clock, expires: rec.ExpiresAt, sha: rec.SHA256, skipped: rec.SkippedEntries,
	})
	s.rec.add("insert")
	return nil
}

func (s *fakeStore) ReleaseWorld(_ context.Context, name string, at time.Time) error {
	c := s.byName[name]
	c.OwnerID = ""
	c.LastActiveAt = at
	c.Warned3dAt = time.Time{}
	c.Warned1dAt = time.Time{}
	s.released = append(s.released, name)
	s.rec.add("release")
	return nil
}

func (s *fakeStore) RestartClock(_ context.Context, name string, at time.Time) error {
	c := s.byName[name]
	c.LastActiveAt = at
	c.Warned3dAt = time.Time{}
	c.Warned1dAt = time.Time{}
	s.rec.add("restartClock")
	return nil
}

func (s *fakeStore) MarkWarned(_ context.Context, name string, tier Tier, at time.Time) error {
	c := s.byName[name]
	if tier == Tier1d {
		c.Warned1dAt = at
	} else {
		c.Warned3dAt = at
	}
	return nil
}

func (s *fakeStore) PresentBackupBytes(context.Context) (int64, error) {
	var total int64
	for _, b := range s.backups {
		if b.status == "present" {
			total += b.size
		}
	}
	return total, nil
}

func (s *fakeStore) EvictableBackups(context.Context) ([]StoredBackup, error) {
	var ps []*fakeBackup
	for _, b := range s.backups {
		if b.status == "present" && (b.reason != ReasonInactive || b.offsite) {
			ps = append(ps, b)
		}
	}
	sort.SliceStable(ps, func(i, j int) bool {
		if ri, rj := ps[i].reason == ReasonInactive, ps[j].reason == ReasonInactive; ri != rj {
			return rj
		}
		return ps[i].createdAt.Before(ps[j].createdAt)
	})
	out := make([]StoredBackup, 0, len(ps))
	for _, b := range ps {
		out = append(out, StoredBackup{ID: b.id, ServerName: b.server, BackupRef: b.ref, SizeBytes: b.size, Reason: b.reason})
	}
	return out, nil
}

func (s *fakeStore) ListExpiredBackups(_ context.Context, now time.Time) ([]StoredBackup, error) {
	var out []StoredBackup
	for _, b := range s.backups {
		if b.status == "present" && b.expires.Before(now) {
			out = append(out, StoredBackup{ID: b.id, ServerName: b.server, BackupRef: b.ref, SizeBytes: b.size})
		}
	}
	return out, nil
}

func (s *fakeStore) MarkBackupDeleted(_ context.Context, id string, at time.Time) error {
	for _, b := range s.backups {
		if b.id == id {
			b.status = "deleted"
			return nil
		}
	}
	return fmt.Errorf("no backup %s", id)
}

func (s *fakeStore) BackupsToVerify(_ context.Context, checkedBefore time.Time, limit int) ([]StoredBackup, error) {
	var ps []*fakeBackup
	for _, b := range s.backups {
		if b.status == "present" && b.corruptAt.IsZero() && b.verifiedAt.Before(checkedBefore) {
			ps = append(ps, b)
		}
	}
	sort.SliceStable(ps, func(i, j int) bool {
		if !ps[i].verifiedAt.Equal(ps[j].verifiedAt) {
			return ps[i].verifiedAt.Before(ps[j].verifiedAt)
		}
		return ps[i].createdAt.Before(ps[j].createdAt)
	})
	var out []StoredBackup
	for _, b := range ps {
		if len(out) == limit {
			break
		}
		out = append(out, StoredBackup{ID: b.id, ServerName: b.server, BackupRef: b.ref, SizeBytes: b.size, Reason: b.reason, SHA256: b.sha})
	}
	return out, nil
}

func (s *fakeStore) find(id string) (*fakeBackup, error) {
	for _, b := range s.backups {
		if b.id == id {
			return b, nil
		}
	}
	return nil, fmt.Errorf("no backup %s", id)
}

func (s *fakeStore) MarkBackupVerified(_ context.Context, id, sha string, at time.Time) error {
	b, err := s.find(id)
	if err != nil {
		return err
	}
	b.verifiedAt = at
	if b.sha == "" {
		b.sha = sha
	}
	return nil
}

func (s *fakeStore) MarkBackupCorrupt(_ context.Context, id string, at time.Time) error {
	b, err := s.find(id)
	if err != nil {
		return err
	}
	b.corruptAt = at
	s.rec.add("corrupt")
	return nil
}

func (s *fakeStore) LiveBackupRefs(context.Context) ([]string, error) {
	if s.liveErr != nil {
		return nil, s.liveErr
	}
	var out []string
	for _, b := range s.backups {
		if b.status != "deleted" {
			out = append(out, b.ref)
		}
	}
	return out, nil
}

func (s *fakeStore) Audit(_ context.Context, rec AuditRecord) error {
	s.audits = append(s.audits, rec)
	s.rec.add("audit:" + rec.Action)
	return nil
}

// ---- fixture --------------------------------------------------------------

func newReaper(cfg Config, cands ...Candidate) (*Reaper, *fakeStore, *fakeCluster, *fakeArchiver) {
	rec := &recorder{}
	st := &fakeStore{rec: rec, clock: testNow, byName: map[string]*Candidate{}}
	cl := &fakeCluster{rec: rec, crds: map[string]ServerCRD{}, inspectErr: map[string]error{}}
	for i := range cands {
		cc := cands[i]
		st.byName[cc.Name] = &cc
		st.order = append(st.order, cc.Name)
		cl.crds[cc.Name] = ServerCRD{PVC: "world-" + cc.Name + "-0"}
	}
	ar := &fakeArchiver{rec: rec}
	r := &Reaper{
		Cfg: cfg, Store: st, Cluster: cl, Archiver: ar,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:   func() time.Time { return testNow },
		IDGen: func() string { st.idn++; return fmt.Sprintf("bk-%d", st.idn) },
	}
	return r, st, cl, ar
}

func mustRun(t *testing.T, r *Reaper) Summary {
	t.Helper()
	sum, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	return sum
}

// ---- tests ----------------------------------------------------------------

// Red line ①: a reaperExempt server is never archived, deleted, or warned no
// matter how idle it is — losing the lobby would be total ingress loss.
func TestReapExemptServerNeverTouched(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "lobby", OwnerID: "", LastActiveAt: idleBy(100 * Day)})
	cl.crds["lobby"] = ServerCRD{Exempt: true, PVC: "world-lobby-0"}

	sum := mustRun(t, r)
	if sum.WorldsReaped != 0 || sum.Skipped != 0 || ar.archives != 0 || cl.deletePVCCalls != 0 {
		t.Fatalf("exempt server was touched: %+v archives=%d deletePVC=%d", sum, ar.archives, cl.deletePVCCalls)
	}
	if st.byName["lobby"].OwnerID != "" || len(st.released) != 0 {
		t.Fatalf("exempt server ownership mutated")
	}
}

// The happy path, asserting the strict ordering of red line ④ and that the
// reap audit carries former_owner (spec §18 audit(reap_world, s, former_owner)).
func TestReapIdleWorldFullSequence(t *testing.T) {
	r, st, cl, _ := newReaper(DefaultConfig(),
		Candidate{Name: "alpha", OwnerID: "user-7", LastActiveAt: idleBy(20 * Day)})

	sum := mustRun(t, r)

	if sum.WorldsReaped != 1 {
		t.Fatalf("WorldsReaped = %d, want 1", sum.WorldsReaped)
	}
	want := []string{"hold", "archive", "insert", "deletePVC", "release", "audit:" + ActionReapWorld, "unhold"}
	if !reflect.DeepEqual(st.rec.events, want) {
		t.Fatalf("call order = %v, want %v", st.rec.events, want)
	}
	if got := cl.deletedPVCs; len(got) != 1 || got[0] != "world-alpha-0" {
		t.Fatalf("deleted PVCs = %v, want [world-alpha-0]", got)
	}
	// Backup carries the former owner and a retention deadline 3mo out.
	if len(st.backups) != 1 || st.backups[0].server != "alpha" {
		t.Fatalf("backup not recorded: %+v", st.backups)
	}
	if want := testNow.Add(DefaultConfig().Retention); !st.backups[0].expires.Equal(want) {
		t.Fatalf("backup expires = %v, want %v", st.backups[0].expires, want)
	}
	if len(st.audits) != 1 || st.audits[0].FormerOwner != "user-7" || st.audits[0].Action != ActionReapWorld {
		t.Fatalf("audit = %+v, want reap_world former_owner=user-7", st.audits)
	}
	// Red line ②: the row survives (ownership released, not deleted).
	c := st.byName["alpha"]
	if c.OwnerID != "" || !c.LastActiveAt.Equal(testNow) || !c.Warned3dAt.IsZero() {
		t.Fatalf("post-reap state wrong: %+v", c)
	}
}

// §23 instrumentation: felis_reaper_worlds_deleted_total advances by exactly one
// per world whose PVC is actually deleted — in lockstep with Summary.WorldsReaped,
// and never for a skipped/preserved world. Asserted as a delta because the counter
// is a process-global singleton other tests in this package also advance.
func TestReapIncrementsDeletedWorldsMetric(t *testing.T) {
	before := testutil.ToFloat64(metrics.ReaperWorldsDeletedTotal)

	r, _, cl, _ := newReaper(DefaultConfig(),
		Candidate{Name: "metric-a", OwnerID: "user-9", LastActiveAt: idleBy(20 * Day)},
		Candidate{Name: "metric-b", OwnerID: "user-9", LastActiveAt: idleBy(20 * Day)},
		// Fresh server: under the deadline, must NOT be reaped or counted.
		Candidate{Name: "metric-fresh", OwnerID: "user-9", LastActiveAt: idleBy(2 * Day)},
	)

	sum := mustRun(t, r)
	if sum.WorldsReaped != 2 {
		t.Fatalf("WorldsReaped = %d, want 2", sum.WorldsReaped)
	}
	if cl.deletePVCCalls != 2 {
		t.Fatalf("deletePVC calls = %d, want 2", cl.deletePVCCalls)
	}

	delta := testutil.ToFloat64(metrics.ReaperWorldsDeletedTotal) - before
	if delta != float64(sum.WorldsReaped) {
		t.Fatalf("felis_reaper_worlds_deleted_total advanced by %v, want %d (one per reaped world)", delta, sum.WorldsReaped)
	}
}

// CENTERPIECE — red line ④: when the archive fails, the PVC is never deleted,
// ownership is untouched, no backup row is written, and the same server is
// retried (state unchanged) on the next run.
func TestReapArchiveFailurePreservesWorld(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "beta", OwnerID: "user-1", LastActiveAt: idleBy(20 * Day)})
	ar.archiveErr = errors.New("backend offline")

	sum := mustRun(t, r)

	if sum.WorldsReaped != 0 || sum.Skipped != 1 {
		t.Fatalf("summary = %+v, want 0 reaped / 1 skipped", sum)
	}
	if cl.deletePVCCalls != 0 {
		t.Fatalf("DeletePVC was called %d times despite archive failure", cl.deletePVCCalls)
	}
	if len(st.backups) != 0 {
		t.Fatalf("a backup row was written despite archive failure: %+v", st.backups)
	}
	if len(st.released) != 0 {
		t.Fatalf("ReleaseWorld ran despite archive failure")
	}
	c := st.byName["beta"]
	if c.OwnerID != "user-1" || !c.LastActiveAt.Equal(idleBy(20*Day)) {
		t.Fatalf("server state changed despite archive failure: %+v", c)
	}

	// Recovery: backend returns, next run reaps cleanly.
	ar.archiveErr = nil
	sum2 := mustRun(t, r)
	if sum2.WorldsReaped != 1 || cl.deletePVCCalls != 1 {
		t.Fatalf("recovery run: summary=%+v deletePVC=%d, want 1 reaped / 1 delete", sum2, cl.deletePVCCalls)
	}
}

// Red line ④ (recording arm): if the archive succeeds but recording it fails,
// the orphan archive is cleaned up and the PVC is still never deleted.
func TestReapInsertBackupFailurePreservesWorld(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "gamma", OwnerID: "user-2", LastActiveAt: idleBy(20 * Day)})
	st.insertErr = errors.New("db down")

	sum := mustRun(t, r)

	if sum.WorldsReaped != 0 || sum.Skipped != 1 {
		t.Fatalf("summary = %+v, want 0 reaped / 1 skipped", sum)
	}
	if cl.deletePVCCalls != 0 {
		t.Fatalf("DeletePVC called despite insert failure")
	}
	if ar.archives != 1 || len(ar.deletes) != 1 {
		t.Fatalf("orphan archive not cleaned up: archives=%d deletes=%d", ar.archives, len(ar.deletes))
	}
	if len(st.backups) != 0 {
		t.Fatalf("backup row present despite insert failure")
	}
}

// Idempotency (the deliberate disk-growth choice): a DeletePVC failure leaves a
// recorded backup; the retry reuses it via FreshBackup instead of writing a
// duplicate archive.
func TestReapDeletePVCFailureIsIdempotent(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "delta", OwnerID: "user-3", LastActiveAt: idleBy(20 * Day)})
	cl.deletePVCErr = errors.New("apiserver timeout")

	sum := mustRun(t, r)
	if sum.WorldsReaped != 0 || sum.Skipped != 1 {
		t.Fatalf("run1 summary = %+v, want 0 reaped / 1 skipped", sum)
	}
	if ar.archives != 1 || len(st.backups) != 1 {
		t.Fatalf("run1: archives=%d backups=%d, want 1/1", ar.archives, len(st.backups))
	}

	// apiserver recovers; the retry must NOT re-archive.
	cl.deletePVCErr = nil
	sum2 := mustRun(t, r)
	if sum2.WorldsReaped != 1 {
		t.Fatalf("run2 WorldsReaped = %d, want 1", sum2.WorldsReaped)
	}
	if ar.archives != 1 {
		t.Fatalf("retry re-archived: archives=%d, want 1 (reuse via FreshBackup)", ar.archives)
	}
	if len(st.backups) != 1 {
		t.Fatalf("retry duplicated the backup row: %d rows, want 1", len(st.backups))
	}
}

// A manual backup taken after the last join is not the reaper's archive: the
// owner may have edited the world from the panel since, which does not move
// last_active_at. The reap writes its own archive.
func TestReapDoesNotReuseManualBackup(t *testing.T) {
	r, st, _, ar := newReaper(DefaultConfig(),
		Candidate{Name: "eta", OwnerID: "user-7", LastActiveAt: idleBy(20 * Day)})
	st.backups = []*fakeBackup{
		{id: "man", server: "eta", ref: "ref-man", reason: "manual", size: 5, status: "present", createdAt: idleBy(10 * Day), expires: testNow.Add(80 * Day)},
	}
	sum := mustRun(t, r)
	if sum.WorldsReaped != 1 || ar.archives != 1 {
		t.Fatalf("summary = %+v, archives = %d: want the reap to archive afresh", sum, ar.archives)
	}
}

// With an off-site bucket configured the reaper archives an idle world but
// keeps it until the archive's copy is confirmed; the next run reuses that
// archive and deletes the world, without archiving it again.
func TestReapWaitsForOffsiteCopy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RequireOffsite = true
	r, st, cl, ar := newReaper(cfg,
		Candidate{Name: "echo", OwnerID: "user-5", LastActiveAt: idleBy(20 * Day)})

	sum := mustRun(t, r)
	if sum.WorldsReaped != 0 || sum.AwaitingOffsite != 1 || sum.Skipped != 0 {
		t.Fatalf("run1 summary = %+v, want archived and awaiting the copy", sum)
	}
	if ar.archives != 1 || len(st.backups) != 1 || cl.deletePVCCalls != 0 {
		t.Fatalf("run1: archives=%d backups=%d deletes=%d, want 1/1/0", ar.archives, len(st.backups), cl.deletePVCCalls)
	}

	// The copy has not landed yet: still kept, still one archive.
	if sum := mustRun(t, r); sum.AwaitingOffsite != 1 || ar.archives != 1 || cl.deletePVCCalls != 0 {
		t.Fatalf("run2 = %+v archives=%d deletes=%d, want the world still kept", sum, ar.archives, cl.deletePVCCalls)
	}

	st.backups[0].offsite = true
	sum = mustRun(t, r)
	if sum.WorldsReaped != 1 || sum.AwaitingOffsite != 0 {
		t.Fatalf("run3 summary = %+v, want reaped", sum)
	}
	if ar.archives != 1 || len(cl.deletedPVCs) != 1 {
		t.Fatalf("run3: archives=%d deleted=%v, want the copied archive reused and the PVC gone", ar.archives, cl.deletedPVCs)
	}
}

// Red line ⑤ (reap arm): an unowned server is still reaped on time; the audit
// records an empty former_owner.
func TestReapUnownedServerStillReaped(t *testing.T) {
	r, st, cl, _ := newReaper(DefaultConfig(),
		Candidate{Name: "orphan", OwnerID: "", LastActiveAt: idleBy(20 * Day)})

	sum := mustRun(t, r)
	if sum.WorldsReaped != 1 || cl.deletePVCCalls != 1 {
		t.Fatalf("unowned server not reaped: %+v deletePVC=%d", sum, cl.deletePVCCalls)
	}
	if len(st.audits) != 1 || st.audits[0].FormerOwner != "" {
		t.Fatalf("audit former_owner = %q, want empty", st.audits[0].FormerOwner)
	}
}

func TestNoReapBeforeDeadline(t *testing.T) {
	r, _, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "fresh", OwnerID: "user-4", LastActiveAt: idleBy(10 * Day)})

	sum := mustRun(t, r)
	if sum.WorldsReaped != 0 || sum.Warned != 0 || ar.archives != 0 || cl.deletePVCCalls != 0 {
		t.Fatalf("acted on a 10d-idle server with a 15d deadline: %+v", sum)
	}
}

// Warning thresholds are DERIVED from the deadline, not hardcoded. Using a
// non-default 10d deadline, warnings must fire at 10d-3d=7d and 10d-1d=9d, in
// elif precedence, deduped per tier, and never for an unowned server.
func TestWarningsDerivedFromNonDefaultDeadline(t *testing.T) {
	cfg := Config{
		IdleBeforeReap: 10 * Day,
		WarnBefore:     []time.Duration{1 * Day, 3 * Day}, // intentionally unsorted
		Retention:      90 * Day,
	}
	r, st, cl, _ := newReaper(cfg,
		// past 7d, under 9d, nothing sent -> 3d (Tier3d) warning
		Candidate{Name: "a", OwnerID: "u-a", LastActiveAt: idleBy(8 * Day)},
		// past 9d, 3d already sent -> 1d (Tier1d) warning
		Candidate{Name: "b", OwnerID: "u-b", LastActiveAt: idleBy(95 * Day / 10), Warned3dAt: idleBy(2 * Day)},
		// past 7d, 3d already sent, under 9d -> no new warning (dedup)
		Candidate{Name: "c", OwnerID: "u-c", LastActiveAt: idleBy(8 * Day), Warned3dAt: idleBy(2 * Day)},
		// under 7d -> no warning yet
		Candidate{Name: "d", OwnerID: "u-d", LastActiveAt: idleBy(6 * Day)},
		// past 7d but unowned -> never warned (red line ⑤)
		Candidate{Name: "e", OwnerID: "", LastActiveAt: idleBy(8 * Day)},
	)
	rw := &recordingWarner{}
	r.Warner = rw

	sum := mustRun(t, r)
	if sum.WorldsReaped != 0 {
		t.Fatalf("nothing should be reaped under a 10d deadline at <=9.5d idle: %+v", sum)
	}
	if sum.Warned != 2 {
		t.Fatalf("Warned = %d, want 2 (a:3d, b:1d)", sum.Warned)
	}
	if len(rw.sent) != 2 {
		t.Fatalf("deliveries = %d, want 2 (a:3d, b:1d)", len(rw.sent))
	}
	if cl.deletePVCCalls != 0 {
		t.Fatalf("a warning path deleted a PVC")
	}
	if st.byName["a"].Warned3dAt.IsZero() || !st.byName["a"].Warned1dAt.IsZero() {
		t.Fatalf("server a: expected only a 3d warning, got %+v", st.byName["a"])
	}
	if st.byName["b"].Warned1dAt.IsZero() {
		t.Fatalf("server b: expected a 1d warning, got %+v", st.byName["b"])
	}
	if !st.byName["e"].Warned3dAt.IsZero() {
		t.Fatalf("unowned server e was warned")
	}
}

// Red line ⑤ (best-effort) with delivery honesty: a Warner failure does not
// abort the run, and it does NOT stamp — the stamp records a DELIVERED notice,
// so the next daily run retries (the warning window bounds the retries, and the
// reap clears the candidate either way).
func TestWarningDeliveryRetriedAfterFailure(t *testing.T) {
	r, st, _, _ := newReaper(DefaultConfig(),
		Candidate{Name: "h", OwnerID: "u-h", LastActiveAt: idleBy(13 * Day)})
	r.Warner = failWarner{}

	sum := mustRun(t, r)
	if sum.Warned != 0 {
		t.Fatalf("Warned = %d, want 0 (nothing was delivered)", sum.Warned)
	}
	if !st.byName["h"].Warned3dAt.IsZero() {
		t.Fatal("a failed delivery must not stamp warned_3d_at")
	}

	// Next run with a working channel: the SAME warning goes out and stamps.
	rw := &recordingWarner{}
	r.Warner = rw
	sum = mustRun(t, r)
	if sum.Warned != 1 || len(rw.sent) != 1 {
		t.Fatalf("retry: Warned=%d sent=%d, want 1/1", sum.Warned, len(rw.sent))
	}
	if st.byName["h"].Warned3dAt.IsZero() {
		t.Fatal("a delivered warning must stamp warned_3d_at")
	}
}

// A nil Warner suppresses the warning WITHOUT stamping it: nothing was sent, so
// nothing is recorded as sent — and the day a channel is wired, the owner can
// still be warned.
func TestWarningSuppressedWithoutWarner(t *testing.T) {
	r, st, _, _ := newReaper(DefaultConfig(),
		Candidate{Name: "n", OwnerID: "u-n", LastActiveAt: idleBy(13 * Day)})

	sum := mustRun(t, r)
	if sum.Warned != 0 {
		t.Fatalf("Warned = %d, want 0 with no warner wired", sum.Warned)
	}
	if !st.byName["n"].Warned3dAt.IsZero() || !st.byName["n"].Warned1dAt.IsZero() {
		t.Fatal("a suppressed warning must not stamp either tier")
	}
}

type failWarner struct{}

func (failWarner) Warn(context.Context, string, string, string) error {
	return errors.New("smtp unavailable")
}

// recordingWarner captures deliveries so the threshold tests exercise the real
// deliver-then-stamp path.
type recordingWarner struct{ sent []string }

func (w *recordingWarner) Warn(_ context.Context, ownerID, server, remaining string) error {
	w.sent = append(w.sent, ownerID+"/"+server+"/"+remaining)
	return nil
}

// §26 capacity: when the store is over its cap, the oldest backup is evicted
// early (destructive — audited) to make room, then the reap proceeds.
func TestCapacityEvictsOldestThenReaps(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxLocalBytes = 100
	r, st, _, ar := newReaper(cfg,
		Candidate{Name: "epsilon", OwnerID: "user-5", LastActiveAt: idleBy(20 * Day)})
	// Two present backups of 75 each = 150 > 100. Oldest must be evicted first.
	st.backups = []*fakeBackup{
		{id: "old", server: "zzz", ref: "ref-old", reason: "manual", size: 75, status: "present", createdAt: idleBy(40 * Day), expires: testNow.Add(30 * Day)},
		{id: "new", server: "yyy", ref: "ref-new", reason: "manual", size: 75, status: "present", createdAt: idleBy(5 * Day), expires: testNow.Add(60 * Day)},
	}

	sum := mustRun(t, r)
	if sum.EvictedEarly != 1 {
		t.Fatalf("EvictedEarly = %d, want 1", sum.EvictedEarly)
	}
	if sum.WorldsReaped != 1 {
		t.Fatalf("WorldsReaped = %d, want 1 after eviction freed room", sum.WorldsReaped)
	}
	var oldStatus, newStatus string
	for _, b := range st.backups {
		switch b.id {
		case "old":
			oldStatus = b.status
		case "new":
			newStatus = b.status
		}
	}
	if oldStatus != "deleted" || newStatus != "present" {
		t.Fatalf("eviction hit wrong backup: old=%s new=%s, want deleted/present", oldStatus, newStatus)
	}
	// The destructive eviction must be audited.
	var evicted bool
	for _, a := range st.audits {
		if a.Action == ActionEvictBackup {
			evicted = true
		}
	}
	if !evicted {
		t.Fatalf("early eviction was not audited")
	}
	if ar.archives != 1 {
		t.Fatalf("reap did not archive after eviction: archives=%d", ar.archives)
	}
}

// §26 capacity + red line ④: if eviction cannot free enough space, the reap is
// skipped and the world is preserved rather than deleted unbacked.
func TestCapacityStillFullSkipsReap(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxLocalBytes = 100
	r, st, cl, ar := newReaper(cfg,
		Candidate{Name: "zeta", OwnerID: "user-6", LastActiveAt: idleBy(20 * Day)})
	st.backups = []*fakeBackup{
		{id: "stuck", server: "zzz", ref: "ref-stuck", reason: "manual", size: 150, status: "present", createdAt: idleBy(40 * Day), expires: testNow.Add(30 * Day)},
	}
	// The archive backend can't delete, so eviction cannot free space.
	r.Archiver.(*fakeArchiver).deleteErr = errors.New("evict unavailable")

	sum := mustRun(t, r)
	if sum.WorldsReaped != 0 || sum.Skipped != 1 || sum.StoreFull != 1 || !sum.Failed() {
		t.Fatalf("summary = %+v, want 0 reaped / 1 skipped, store full, failed", sum)
	}
	if cl.deletePVCCalls != 0 {
		t.Fatalf("world was deleted while the store was full")
	}
	if ar.archives != 0 {
		t.Fatalf("archived into a full store")
	}
	if st.byName["zeta"].OwnerID != "user-6" {
		t.Fatalf("ownership changed while store full")
	}
}

// Eviction order: on-demand backups go first, then reaper archives that have an
// off-site copy; the only copy of a reaped world is never evicted early, even
// when that leaves the store full and the idle world waits.
func TestCapacityEvictionOrderSparesSoleCopies(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxLocalBytes = 100
	r, st, _, _ := newReaper(cfg,
		Candidate{Name: "theta", OwnerID: "user-8", LastActiveAt: idleBy(20 * Day)})
	st.backups = []*fakeBackup{
		{id: "sole", server: "gone1", ref: "ref-sole", reason: ReasonInactive, size: 60, status: "present", createdAt: idleBy(80 * Day), expires: testNow.Add(10 * Day)},
		{id: "copied", server: "gone2", ref: "ref-copied", reason: ReasonInactive, offsite: true, size: 30, status: "present", createdAt: idleBy(70 * Day), expires: testNow.Add(20 * Day)},
		{id: "man", server: "live", ref: "ref-man", reason: "manual", size: 30, status: "present", createdAt: idleBy(2 * Day), expires: testNow.Add(28 * Day)},
	}
	sum := mustRun(t, r)
	status := map[string]string{}
	for _, b := range st.backups {
		status[b.id] = b.status
	}
	// 120 over a cap of 100: the on-demand backup (30) alone brings it to 90.
	if status["man"] != "deleted" || status["copied"] != "present" || status["sole"] != "present" {
		t.Fatalf("evicted %v; want only the on-demand backup", status)
	}
	if sum.EvictedEarly != 1 || sum.WorldsReaped != 1 {
		t.Fatalf("summary = %+v, want 1 evicted, 1 reaped", sum)
	}

	// Over a cap of 50 with only reaper archives left: the copied one goes,
	// the sole copy stays, the store is still full, and the idle world waits.
	cfg.MaxLocalBytes = 50
	r2, st2, cl2, _ := newReaper(cfg,
		Candidate{Name: "iota", OwnerID: "user-9", LastActiveAt: idleBy(20 * Day)})
	st2.backups = []*fakeBackup{
		{id: "sole", server: "gone1", ref: "ref-sole", reason: ReasonInactive, size: 60, status: "present", createdAt: idleBy(80 * Day), expires: testNow.Add(10 * Day)},
		{id: "copied", server: "gone2", ref: "ref-copied", reason: ReasonInactive, offsite: true, size: 30, status: "present", createdAt: idleBy(70 * Day), expires: testNow.Add(20 * Day)},
	}
	sum2 := mustRun(t, r2)
	if st2.backups[0].status != "present" || st2.backups[1].status != "deleted" {
		t.Fatalf("sole=%s copied=%s, want present/deleted", st2.backups[0].status, st2.backups[1].status)
	}
	if sum2.StoreFull != 1 || sum2.WorldsReaped != 0 || cl2.deletePVCCalls != 0 {
		t.Fatalf("summary = %+v, deletes = %d: the world should wait while the store is full", sum2, cl2.deletePVCCalls)
	}
}

// Retention pass: backups past expires_at are deleted from the backend and
// marked deleted; unexpired backups are untouched.
func TestExpiredBackupsDeleted(t *testing.T) {
	r, st, _, ar := newReaper(DefaultConfig())
	st.backups = []*fakeBackup{
		{id: "gone", server: "s1", ref: "ref-gone", size: 5, status: "present", createdAt: idleBy(120 * Day), expires: idleBy(1 * Day)},
		{id: "keep", server: "s2", ref: "ref-keep", size: 5, status: "present", createdAt: idleBy(10 * Day), expires: testNow.Add(80 * Day)},
	}

	sum := mustRun(t, r)
	if sum.BackupsExpired != 1 {
		t.Fatalf("BackupsExpired = %d, want 1", sum.BackupsExpired)
	}
	if len(ar.deletes) != 1 || ar.deletes[0] != "ref-gone" {
		t.Fatalf("deleted archives = %v, want [ref-gone]", ar.deletes)
	}
	byID := map[string]string{}
	for _, b := range st.backups {
		byID[b.id] = b.status
	}
	if byID["gone"] != "deleted" || byID["keep"] != "present" {
		t.Fatalf("expiry hit wrong rows: %v", byID)
	}
}

// An expired backup the backend cannot delete stays present and marks the run
// failed, so the Job reports it instead of succeeding every day.
func TestExpiryFailureFailsTheRun(t *testing.T) {
	r, st, _, _ := newReaper(DefaultConfig())
	st.backups = []*fakeBackup{
		{id: "gone", server: "s1", ref: "ref-gone", size: 5, status: "present", createdAt: idleBy(120 * Day), expires: idleBy(1 * Day)},
	}
	r.Archiver.(*fakeArchiver).deleteErr = errors.New("permission denied")

	sum := mustRun(t, r)
	if sum.BackupsExpired != 0 || sum.ExpireFailed != 1 || !sum.Failed() {
		t.Fatalf("summary = %+v, want 0 expired / 1 expire_failed, failed", sum)
	}
	if st.backups[0].status != "present" {
		t.Fatalf("the undeleted archive's row was marked %s", st.backups[0].status)
	}
}

// A servers row whose CRD has been deleted is skipped (not a failure) — the
// reaper never deletes world data it cannot first inspect for the exemption.
func TestMissingCRDSkipped(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "ghost", OwnerID: "u", LastActiveAt: idleBy(20 * Day)})
	delete(cl.crds, "ghost") // CRD gone, servers row lingers

	sum := mustRun(t, r)
	if sum.WorldsReaped != 0 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v, want 0/0 (skipped without error)", sum)
	}
	if ar.archives != 0 || cl.deletePVCCalls != 0 {
		t.Fatalf("acted on a server with no CRD")
	}
	if st.byName["ghost"].OwnerID != "u" {
		t.Fatalf("mutated a server with no CRD")
	}
}

// A failure to list servers is the one hard error that aborts the batch.
func TestListErrorAbortsBatch(t *testing.T) {
	r, st, _, _ := newReaper(DefaultConfig())
	st.listErr = errors.New("db unreachable")
	if _, err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("expected a hard error when listing servers fails")
	}
}

// checking swaps the fixture's archiver for one that reads archives back and
// sweeps.
func checking(r *Reaper, ar *fakeArchiver) *checkingArchiver {
	c := &checkingArchiver{fakeArchiver: ar, corrupt: map[string]bool{}}
	r.Archiver = c
	return c
}

// An archive left from an earlier run is read back before the world it holds is
// deleted, and the read-back is recorded.
func TestReapReadsBackReusedArchive(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "kilo", OwnerID: "user-1", LastActiveAt: idleBy(20 * Day)})
	ca := checking(r, ar)
	st.backups = []*fakeBackup{{id: "old", server: "kilo", ref: "ref-old", reason: ReasonInactive, size: 5,
		status: "present", createdAt: idleBy(Day), expires: testNow.Add(89 * Day), sha: "sha-ref-old"}}

	sum := mustRun(t, r)
	if sum.WorldsReaped != 1 || ar.archives != 0 || len(cl.deletedPVCs) != 1 {
		t.Fatalf("summary %+v archives=%d deleted=%v: want the checked archive reused", sum, ar.archives, cl.deletedPVCs)
	}
	if st.rec.events[1] != "verify" || st.rec.events[2] != "deletePVC" {
		t.Errorf("events = %v, want the read-back before the delete", st.rec.events)
	}
	if !st.backups[0].verifiedAt.Equal(testNow) || !reflect.DeepEqual(ca.verified, []string{"ref-old"}) {
		t.Errorf("read-back not recorded: %+v", st.backups[0])
	}
}

// A reused archive that no longer reads back is marked corrupt and the world,
// still there, is archived afresh before it is deleted. The run reports it.
func TestReapReplacesCorruptArchive(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "lima", OwnerID: "user-1", LastActiveAt: idleBy(20 * Day)})
	ca := checking(r, ar)
	ca.corrupt["ref-rotten"] = true
	st.backups = []*fakeBackup{{id: "rotten", server: "lima", ref: "ref-rotten", reason: ReasonInactive, size: 5,
		status: "present", createdAt: idleBy(Day), expires: testNow.Add(89 * Day)}}

	sum := mustRun(t, r)
	if sum.WorldsReaped != 1 || sum.Corrupt != 1 || !sum.Failed() {
		t.Fatalf("summary = %+v, want reaped with one corrupt archive reported", sum)
	}
	want := []string{"hold", "verify", "corrupt", "archive", "insert", "deletePVC"}
	if !reflect.DeepEqual(st.rec.events[:len(want)], want) {
		t.Errorf("events = %v, want %v first", st.rec.events, want)
	}
	if st.backups[0].corruptAt.IsZero() || len(st.backups) != 2 || st.backups[1].sha != "sha-ref-lima-1" {
		t.Errorf("backups = %+v %+v", st.backups[0], st.backups[len(st.backups)-1])
	}
	if len(cl.deletedPVCs) != 1 {
		t.Errorf("deleted = %v", cl.deletedPVCs)
	}
}

// When the archive cannot be read back at all (the store is not mounted), the
// world is kept and nothing is marked: the error says nothing about the archive.
func TestReapKeepsWorldWhenReadBackFails(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "mike", OwnerID: "user-1", LastActiveAt: idleBy(20 * Day)})
	ca := checking(r, ar)
	ca.verifyErr = errors.New("input/output error")
	st.backups = []*fakeBackup{{id: "b", server: "mike", ref: "ref-b", reason: ReasonInactive, size: 5,
		status: "present", createdAt: idleBy(Day), expires: testNow.Add(89 * Day)}}

	sum := mustRun(t, r)
	if sum.WorldsReaped != 0 || sum.Skipped != 1 || cl.deletePVCCalls != 0 || ar.archives != 0 {
		t.Fatalf("summary %+v deletes=%d archives=%d: want the world kept", sum, cl.deletePVCCalls, ar.archives)
	}
	if !st.backups[0].corruptAt.IsZero() {
		t.Error("an unreadable store marked the archive corrupt")
	}
}

// The read-back pass takes VerifyPerRun archives, never-checked ones first, and
// records what it finds; a corrupt one is marked and fails the run once.
func TestVerifyPassSamplesArchives(t *testing.T) {
	cfg := DefaultConfig()
	cfg.VerifyPerRun = 2
	r, st, _, ar := newReaper(cfg)
	ca := checking(r, ar)
	ca.corrupt["ref-c"] = true
	st.backups = []*fakeBackup{
		{id: "a", ref: "ref-a", status: "present", createdAt: idleBy(9 * Day), verifiedAt: idleBy(8 * Day), expires: testNow.Add(30 * Day)},
		{id: "b", ref: "ref-b", status: "present", createdAt: idleBy(9 * Day), verifiedAt: idleBy(Day), expires: testNow.Add(30 * Day)},
		{id: "c", ref: "ref-c", status: "present", createdAt: idleBy(5 * Day), expires: testNow.Add(30 * Day)},
		{id: "d", ref: "ref-d", status: "deleted", createdAt: idleBy(5 * Day), expires: testNow.Add(30 * Day)},
	}

	sum := mustRun(t, r)
	if !reflect.DeepEqual(ca.verified, []string{"ref-c", "ref-a"}) {
		t.Errorf("read back %v, want [ref-c ref-a]", ca.verified)
	}
	if sum.Verified != 1 || sum.Corrupt != 1 || !sum.Failed() {
		t.Errorf("summary = %+v", sum)
	}
	if st.backups[2].corruptAt.IsZero() || !st.backups[0].verifiedAt.Equal(testNow) || st.backups[0].sha != "sha-ref-a" {
		t.Errorf("outcomes not recorded: %+v %+v", st.backups[0], st.backups[2])
	}

	// The next run skips the corrupt one and what was checked within
	// VerifyEvery; a week on, b and a are due again, b first.
	ca.verified = nil
	if sum := mustRun(t, r); sum.Corrupt != 0 || len(ca.verified) != 0 {
		t.Errorf("second run read back %v (%+v), want nothing", ca.verified, sum)
	}
	r.Now = func() time.Time { return testNow.Add(7*Day + time.Hour) }
	if mustRun(t, r); !reflect.DeepEqual(ca.verified, []string{"ref-b", "ref-a"}) {
		t.Errorf("a week later read back %v, want [ref-b ref-a]", ca.verified)
	}
}

// The sweep claims every archive a backup records, removes leftovers past
// PartialAfter and orphans past the retention, and never runs without the list
// of recorded archives.
func TestSweepPass(t *testing.T) {
	r, st, _, ar := newReaper(DefaultConfig())
	ca := checking(r, ar)
	ca.sweepRemoved = []string{"/archives/.x-1.tar.gz.partial"}
	ca.sweepOrphaned = []string{"/archives/x-2.tar.gz"}
	st.backups = []*fakeBackup{
		{id: "a", ref: "ref-a", status: "present", expires: testNow.Add(Day), verifiedAt: testNow},
		{id: "d", ref: "ref-d", status: "deleted", expires: testNow.Add(Day)},
	}
	sum := mustRun(t, r)
	if sum.Swept != 1 || sum.OrphanArchives != 1 || sum.Failed() {
		t.Errorf("summary = %+v", sum)
	}
	if !ca.sweepLive("ref-a") || ca.sweepLive("ref-d") {
		t.Error("sweep did not get the recorded archives as live")
	}
	cfg := DefaultConfig()
	if want := []time.Time{testNow.Add(-cfg.PartialAfter), testNow.Add(-cfg.Retention)}; !reflect.DeepEqual(ca.sweepCutoffs, want) {
		t.Errorf("cutoffs = %v, want %v", ca.sweepCutoffs, want)
	}

	ca.sweepLive = nil
	st.liveErr = errors.New("db down")
	if sum := mustRun(t, r); !sum.SweepFailed || ca.sweepLive != nil {
		t.Errorf("sweep ran without the recorded archives: %+v", sum)
	}
}

// data-durability-8: a server still up when its world goes idle is told to stop
// and left for the next run, never archived while it may be writing; once it is
// down the next run reaps it.
func TestReapWaitsForServerToStop(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "busy", OwnerID: "user-1", LastActiveAt: idleBy(20 * Day)})
	cl.notQuiet = map[string]bool{"busy": true}

	sum := mustRun(t, r)
	if sum.AwaitingStop != 1 || sum.Skipped != 0 || sum.Failed() || ar.archives != 0 || cl.deletePVCCalls != 0 {
		t.Fatalf("summary %+v archives=%d deletes=%d: want the world left alone for the next run", sum, ar.archives, cl.deletePVCCalls)
	}
	if st.byName["busy"].OwnerID != "user-1" || len(st.audits) != 0 {
		t.Fatalf("a server not yet down was released: %+v", st.byName["busy"])
	}

	cl.notQuiet = nil
	if sum := mustRun(t, r); sum.WorldsReaped != 1 || sum.AwaitingStop != 0 {
		t.Fatalf("second run = %+v, want reaped once the server is down", sum)
	}
}

// A hold that cannot be taken for any other reason is a failure of the run.
func TestReapHoldErrorSkips(t *testing.T) {
	r, _, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "flaky", OwnerID: "user-1", LastActiveAt: idleBy(20 * Day)})
	cl.holdErr = errors.New("apiserver timeout")

	sum := mustRun(t, r)
	if sum.Skipped != 1 || !sum.Failed() || ar.archives != 0 {
		t.Fatalf("summary %+v archives=%d: want the server skipped and the run failed", sum, ar.archives)
	}
}

// The world is held from before the archive until after the reap is recorded,
// and let go on every path, a failed archive included.
func TestReapReleasesHoldOnEveryPath(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "hotel", OwnerID: "user-1", LastActiveAt: idleBy(20 * Day)})
	ar.archiveErr = errors.New("disk full")

	mustRun(t, r)
	if len(cl.held) != 0 || st.rec.events[len(st.rec.events)-1] != "unhold" {
		t.Fatalf("hold not released after a failed archive: events=%v held=%v", st.rec.events, cl.held)
	}
}

// A lock lost while the archive is written (felis-api could then start the
// server) keeps the PVC; the archive is recorded and reused next run.
func TestReapLostHoldKeepsWorld(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "india", OwnerID: "user-1", LastActiveAt: idleBy(20 * Day)})
	ar.onArchive = func() { cl.lost(errors.New("lock rewrite failing")) }

	sum := mustRun(t, r)
	if sum.WorldsReaped != 0 || sum.Skipped != 1 || cl.deletePVCCalls != 0 {
		t.Fatalf("summary %+v deletes=%d: want the world kept once the hold was lost", sum, cl.deletePVCCalls)
	}
	if st.byName["india"].OwnerID != "user-1" {
		t.Fatal("ownership released after the hold was lost")
	}

	ar.onArchive = nil
	if sum := mustRun(t, r); sum.WorldsReaped != 1 || ar.archives != 1 {
		t.Fatalf("retry = %+v archives=%d, want the recorded archive reused", sum, ar.archives)
	}
}

// data-durability-19: an unowned server with no world (reaped before) is not
// reaped again when its clock runs out; the clock restarts, with no audit and
// no count.
func TestReapUnownedWithoutWorldRestartsClock(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "juliet", LastActiveAt: idleBy(16 * Day)})
	cl.noWorld = map[string]bool{"world-juliet-0": true}

	sum := mustRun(t, r)
	if sum.WorldsReaped != 0 || sum.Skipped != 0 || sum.Failed() || ar.archives != 0 || cl.deletePVCCalls != 0 || len(st.audits) != 0 {
		t.Fatalf("summary %+v archives=%d deletes=%d audits=%v: want only the clock restarted",
			sum, ar.archives, cl.deletePVCCalls, st.audits)
	}
	if !st.byName["juliet"].LastActiveAt.Equal(testNow) {
		t.Fatalf("last_active_at = %v, want restarted at %v", st.byName["juliet"].LastActiveAt, testNow)
	}
	if sum := mustRun(t, r); sum.WorldsReaped != 0 || len(st.audits) != 0 {
		t.Fatalf("second run = %+v audits=%v", sum, st.audits)
	}
}

// An owner who never started the server gives it up at the deadline like any
// other; there is nothing to archive, and no world was deleted to count.
func TestReapOwnedWithoutWorldReleases(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "kappa", OwnerID: "user-4", LastActiveAt: idleBy(16 * Day)})
	cl.noWorld = map[string]bool{"world-kappa-0": true}

	sum := mustRun(t, r)
	if sum.WorldsReaped != 0 || sum.Skipped != 0 || ar.archives != 0 || cl.deletePVCCalls != 0 {
		t.Fatalf("summary %+v archives=%d deletes=%d", sum, ar.archives, cl.deletePVCCalls)
	}
	if st.byName["kappa"].OwnerID != "" || len(st.audits) != 1 || st.audits[0].FormerOwner != "user-4" {
		t.Fatalf("owner %q audits %+v: want released and audited", st.byName["kappa"].OwnerID, st.audits)
	}
}

// A run that deleted the PVC and failed before releasing the world is finished
// by the next: released, audited and counted once, without a new archive.
func TestReapFinishesInterruptedReap(t *testing.T) {
	r, st, cl, ar := newReaper(DefaultConfig(),
		Candidate{Name: "lambda", OwnerID: "user-5", LastActiveAt: idleBy(20 * Day)})
	cl.noWorld = map[string]bool{"world-lambda-0": true}
	st.backups = []*fakeBackup{{id: "prev", server: "lambda", ref: "ref-prev", reason: ReasonInactive, size: 5,
		status: "present", createdAt: idleBy(Day), expires: testNow.Add(89 * Day)}}

	sum := mustRun(t, r)
	if sum.WorldsReaped != 1 || ar.archives != 0 || cl.deletePVCCalls != 0 {
		t.Fatalf("summary %+v archives=%d deletes=%d: want the reap finished", sum, ar.archives, cl.deletePVCCalls)
	}
	if st.byName["lambda"].OwnerID != "" || len(st.audits) != 1 {
		t.Fatalf("owner %q audits %+v", st.byName["lambda"].OwnerID, st.audits)
	}
}
