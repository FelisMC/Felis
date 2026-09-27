package offsite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/dbbackup"
)

// snapshotter stands in for `felis db backup -label offsite`: each call writes
// a bundle stamped with the syncer's clock into its DBDir.
type snapshotter struct {
	t     *testing.T
	s     *Syncer
	calls int
	err   error
}

func (sn *snapshotter) take(context.Context) error {
	sn.calls++
	if sn.err != nil {
		return sn.err
	}
	name := dbbackup.BundleName(sn.s.now(), dbbackup.LabelOffsite)
	data := dbBundle(sn.t, sn.s.now(), &dbbackup.Counts{Users: 3, Servers: 2}, 64)
	return os.WriteFile(filepath.Join(sn.s.DBDir, name), data, 0o600)
}

func withSnapshot(t *testing.T, s *Syncer) *snapshotter {
	sn := &snapshotter{t: t, s: s}
	s.Snapshot = sn.take
	return sn
}

func dbKeys(b *memBucket) []string {
	var keys []string
	for k := range b.objs {
		if strings.HasPrefix(k, dbDir) {
			keys = append(keys, strings.TrimSuffix(strings.TrimPrefix(k, dbDir), objExt))
		}
	}
	sort.Strings(keys)
	return keys
}

// TestSyncSnapshotsAfterCopyingWorlds: a pass that copies an archive leaves a
// bundle in the bucket taken after the copy, which a restore takes as the
// latest and whose rows list the archive; the pass counts the bucket's bundles
// once, with that one newest. A pass that copies nothing takes none, and the
// next daily bundle replaces it.
func TestSyncSnapshotsAfterCopyingWorlds(t *testing.T) {
	cat := &fakeCatalog{rows: []*row{
		{WorldBackup: WorldBackup{ID: "b1", Server: "alpha", Ref: "/a/alpha-1.tar.gz"}, status: "present"},
	}}
	s, b := newSyncer(t, cat)
	sn := withSnapshot(t, s)
	writeFile(t, s.ArchiveDir, "alpha-1.tar.gz", 100)
	daily := "felis-db-20260924T030000Z-daily.tar"
	writeFile(t, s.DBDir, daily, 50)

	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v (%+v)", err, res)
	}
	snap := dbbackup.BundleName(now, dbbackup.LabelOffsite)
	if sn.calls != 1 {
		t.Fatalf("snapshots taken = %d, want 1 after copying an archive", sn.calls)
	}
	if got := dbKeys(b); !slices.Equal(got, []string{daily, snap}) {
		t.Fatalf("bucket bundles = %v, want the daily and the snapshot", got)
	}
	if res.NewestDB != snap || res.RemoteDB != 2 || res.DBUploaded != 2 || res.WorldsUploaded != 1 {
		t.Fatalf("result = %+v, want the snapshot newest of 2 bundles, 2 bundles and 1 archive copied", res)
	}
	name, _, err := ChooseDB(context.Background(), b, s.Key)
	if err != nil || name != snap {
		t.Fatalf("fetch-db latest = %q, %v; want the snapshot", name, err)
	}

	if res, err := s.Run(context.Background()); err != nil || sn.calls != 1 || res.NewestDB != snap || res.RemoteDB != 2 {
		t.Fatalf("a pass that copied nothing: snapshots %d, result %+v, err %v; want no new snapshot", sn.calls, res, err)
	}

	next := now.Add(15 * time.Hour)
	s.Now = func() time.Time { return next }
	nextDaily := dbbackup.BundleName(next.Add(-time.Minute), dbbackup.LabelDaily)
	writeFile(t, s.DBDir, nextDaily, 50)
	res, err = s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := dbKeys(b); !slices.Equal(got, []string{daily, nextDaily}) || sn.calls != 1 || res.DBPruned != 1 {
		t.Fatalf("after the next daily: bucket %v, snapshots %d, result %+v; want the snapshot pruned, both dailies kept", got, sn.calls, res)
	}
}

// TestSyncSnapshotOnlyWhenACopyIsNewer: no snapshot while the newest bundle in
// the bucket was taken after the last copy, in the same second included (bundle
// names have one-second resolution); one when a copy is newer, or when the
// bucket has no bundle at all; none on an install that never copied an archive.
func TestSyncSnapshotOnlyWhenACopyIsNewer(t *testing.T) {
	stamped := func(at time.Time) string { return dbbackup.BundleName(at, dbbackup.LabelDaily) }
	cases := []struct {
		name    string
		copied  time.Time
		bundle  string // in the bucket before the pass; "" for none
		want    int
		wantNew string
	}{
		{name: "never copied", want: 0},
		{name: "bundle after the copy", copied: now.Add(-time.Hour), bundle: stamped(now.Add(-time.Minute)), want: 0},
		{name: "same second", copied: now.Add(400 * time.Millisecond), bundle: stamped(now), want: 0},
		{name: "copy after the bundle", copied: now.Add(-time.Hour), bundle: stamped(now.Add(-2 * time.Hour)), want: 1},
		{name: "no bundle", copied: now.Add(-time.Hour), want: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cat := &fakeCatalog{rows: []*row{
				{WorldBackup: WorldBackup{ID: "b1", Ref: "/a/alpha-1.tar.gz"}, status: "present", offsite: c.copied},
			}}
			if c.copied.IsZero() {
				cat.rows[0].status = "deleted"
			}
			s, b := newSyncer(t, cat)
			sn := withSnapshot(t, s)
			if c.bundle != "" {
				b.objs[DBKey(c.bundle)] = seal(t, []byte("bundle"), s.Key)
			}
			res, err := s.Run(context.Background())
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if sn.calls != c.want {
				t.Fatalf("snapshots taken = %d, want %d (newest %s)", sn.calls, c.want, res.NewestDB)
			}
			if c.want == 1 && res.NewestDB != dbbackup.BundleName(now, dbbackup.LabelOffsite) {
				t.Fatalf("newest bundle = %s, want the snapshot", res.NewestDB)
			}
		})
	}

	// A pass that copies no bundles (-db-dir "") takes none either.
	cat := &fakeCatalog{rows: []*row{
		{WorldBackup: WorldBackup{ID: "b1", Ref: "/a/alpha-1.tar.gz"}, status: "present", offsite: now.Add(-time.Hour)},
	}}
	s, _ := newSyncer(t, cat)
	s.DBDir = ""
	calls := 0
	s.Snapshot = func(context.Context) error { calls++; return errors.New("no bundle directory") }
	if _, err := s.Run(context.Background()); err != nil || calls != 0 {
		t.Fatalf("no bundle directory: snapshots %d, err %v; want none", calls, err)
	}
}

// TestSyncSnapshotFailureIsReported: a snapshot that fails fails the pass, which
// the watchdog reports, and the bundles already there still count.
func TestSyncSnapshotFailureIsReported(t *testing.T) {
	cat := &fakeCatalog{rows: []*row{
		{WorldBackup: WorldBackup{ID: "b1", Server: "alpha", Ref: "/a/alpha-1.tar.gz"}, status: "present"},
	}}
	s, _ := newSyncer(t, cat)
	sn := withSnapshot(t, s)
	sn.err = errors.New("pg_dump: connection refused")
	writeFile(t, s.ArchiveDir, "alpha-1.tar.gz", 100)
	daily := "felis-db-20260924T030000Z-daily.tar"
	writeFile(t, s.DBDir, daily, 50)

	res, err := s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pg_dump: connection refused") {
		t.Fatalf("run error = %v, want the snapshot's failure", err)
	}
	if res.NewestDB != daily || res.RemoteDB != 1 || res.WorldsUploaded != 1 {
		t.Fatalf("result = %+v, want the daily still counted and the archive copied", res)
	}
}

// TestSyncBucketKeepsDailiesPastSnapshots: snapshots do not count against
// DBKeep, so hourly ones leave the daily restore points alone, and only the
// newest snapshot stays, while it is the newest bundle of all.
func TestSyncBucketKeepsDailiesPastSnapshots(t *testing.T) {
	s, b := newSyncer(t, &fakeCatalog{})
	for _, name := range []string{
		"felis-db-20260922T030000Z-daily.tar",
		"felis-db-20260923T030000Z-daily.tar",
		"felis-db-20260924T030000Z-daily.tar",
	} {
		b.objs[DBKey(name)] = []byte("daily")
	}
	for h := 4; h <= 9; h++ {
		name := dbbackup.BundleName(time.Date(2026, 9, 24, h, 0, 0, 0, time.UTC), dbbackup.LabelOffsite)
		b.objs[DBKey(name)] = []byte("snapshot")
	}
	b.objs[DBKey("felis-db-20260923T120000Z-offsite.tar")] = []byte("older than a daily")

	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"felis-db-20260923T030000Z-daily.tar", "felis-db-20260924T030000Z-daily.tar", "felis-db-20260924T090000Z-offsite.tar"}
	if got := dbKeys(b); !slices.Equal(got, want) {
		t.Fatalf("bucket bundles = %v, want %v", got, want)
	}
	if res.RemoteDB != 3 || res.DBPruned != 7 || res.NewestDB != want[2] {
		t.Fatalf("result = %+v, want 3 held, 7 pruned, the snapshot newest", res)
	}

	got := keptBundles([]string{"felis-db-20260925T030000Z-daily.tar", "felis-db-20260924T090000Z-offsite.tar", "felis-db-20260924T030000Z-daily.tar"}, 2)
	if len(got) != 2 || got["felis-db-20260924T090000Z-offsite.tar"] {
		t.Fatalf("kept = %v, want the two dailies: a snapshot older than a daily lists nothing it does not", got)
	}
}

// TestSyncSweepsUnrecordedWorlds: a world object no row keeps goes once it is
// older than OrphanAfter; a younger one, one whose age the bucket does not
// report, one a row keeps however old, and anything that is not an archive
// object stay.
func TestSyncSweepsUnrecordedWorlds(t *testing.T) {
	day := 24 * time.Hour
	rows := func() []*row {
		return []*row{
			{WorldBackup: WorldBackup{ID: "kept", Ref: "/a/kept.tar.gz"}, status: "present", offsite: now.Add(-200 * day)},
			{WorldBackup: WorldBackup{ID: "evicted", Ref: "/a/evicted.tar.gz"}, status: "deleted", expires: now.Add(day), offsite: now.Add(-100 * day)},
			// Copied, but the run died before MarkOffsite, and the row has
			// since expired: ExpiredRefs never lists it.
			{WorldBackup: WorldBackup{ID: "unmarked", Ref: "/a/unmarked.tar.gz"}, status: "deleted", expires: now.Add(-day)},
		}
	}
	ages := map[string]time.Duration{
		"worlds/kept.tar.gz.fenc":         200 * day,
		"worlds/evicted.tar.gz.fenc":      100 * day,
		"worlds/unmarked.tar.gz.fenc":     95 * day,
		"worlds/restored-old.tar.gz.fenc": 91 * day,
		"worlds/restored-new.tar.gz.fenc": 10 * day,
		"worlds/no-age.tar.gz.fenc":       0,
		"worlds/README":                   300 * day,
	}
	setup := func(t *testing.T, cat *fakeCatalog) (*Syncer, *memBucket, *bytes.Buffer) {
		s, b := newSyncer(t, cat)
		s.OrphanAfter = 90 * day
		var log bytes.Buffer
		s.Log = &log
		b.modified = map[string]time.Time{}
		for k, age := range ages {
			b.objs[k] = []byte("x")
			if age > 0 {
				b.modified[k] = now.Add(-age)
			}
		}
		return s, b, &log
	}

	s, b, log := setup(t, &fakeCatalog{rows: rows()})
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	removed := slices.Sorted(slices.Values(b.removed))
	if want := []string{"worlds/restored-old.tar.gz.fenc", "worlds/unmarked.tar.gz.fenc"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}
	if res.WorldsExpired != 2 || res.RemoteWorlds != len(ages)-2 {
		t.Fatalf("result = %+v, want 2 expired, %d left", res, len(ages)-2)
	}
	if !strings.Contains(log.String(), "2 world archives in the bucket have no backup record") {
		t.Errorf("the young unrecorded archives were not reported:\n%s", log.String())
	}

	// A database not restored yet keeps nothing: nothing is swept on it.
	s, b, _ = setup(t, &fakeCatalog{})
	if _, err := s.Run(context.Background()); err != nil || len(b.removed) != 0 {
		t.Fatalf("an empty catalog: removed %v, err %v; want nothing", b.removed, err)
	}
	s, b, _ = setup(t, &fakeCatalog{rows: rows()})
	s.OrphanAfter = 0
	if _, err := s.Run(context.Background()); err != nil || len(b.removed) != 0 {
		t.Fatalf("no OrphanAfter: removed %v, err %v; want nothing", b.removed, err)
	}
}
