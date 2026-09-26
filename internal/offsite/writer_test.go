package offsite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	myID    = "aaaaaaaaaaaaaaaa"
	otherID = "bbbbbbbbbbbbbbbb"
)

func putWriter(t *testing.T, b *memBucket, id, host string, at time.Time) {
	t.Helper()
	raw, err := json.Marshal(Writer{HostID: id, Host: host, At: at})
	if err != nil {
		t.Fatal(err)
	}
	b.objs[writerMark] = raw
}

// testLease is a lease whose id file is in a temp dir, holding id unless it
// is "".
func testLease(t *testing.T, id string) Lease {
	t.Helper()
	l := Lease{IDFile: filepath.Join(t.TempDir(), HostIDFile), Host: "spare-1"}
	if id != "" {
		if err := os.WriteFile(l.IDFile, []byte(id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func TestLeasePlan(t *testing.T) {
	at := now.Add(-20 * time.Minute)
	for _, tc := range []struct {
		what      string
		mine      string
		inherited bool
		writer    string // the id felis-writer names, "" for none
		empty     bool
		want      Role
	}{
		{"an empty bucket, a new host", "", false, "", true, RoleClaims},
		{"another host's copies, no writer named, a new host", "", false, "", false, RoleStandby},
		{"another host's copies, no writer named, a host that copied before writers were recorded", "", true, "", false, RoleClaims},
		{"no writer named, a host that wrote before", myID, false, "", false, RoleClaims},
		{"this host named", myID, false, myID, false, RoleWrites},
		{"another host named, a host that wrote before", myID, false, otherID, false, RoleDisplaced},
		{"another host named, a new host", "", false, otherID, false, RoleStandby},
		{"another host named, a host that copied before writers were recorded", "", true, otherID, false, RoleStandby},
		{"another host named over an empty bucket", "", false, otherID, true, RoleStandby},
	} {
		t.Run(tc.what, func(t *testing.T) {
			b := newMemBucket()
			if tc.writer != "" {
				putWriter(t, b, tc.writer, "prod-1", at)
			}
			l := testLease(t, tc.mine)
			l.Inherited = tc.inherited
			role, w, err := l.Plan(context.Background(), b, tc.empty)
			if err != nil || role != tc.want {
				t.Fatalf("Plan = %v, %v; want %v", role, err, tc.want)
			}
			if (w != nil) != (tc.writer != "") || (w != nil && (w.HostID != tc.writer || w.Host != "prod-1" || !w.At.Equal(at))) {
				t.Errorf("Plan named %+v, want the bucket's writer %q", w, tc.writer)
			}
			if b.puts != 0 {
				t.Errorf("Plan wrote %d objects", b.puts)
			}
		})
	}

	b := newMemBucket()
	b.objs[writerMark] = []byte("hello")
	if _, _, err := testLease(t, "").Plan(context.Background(), b, true); err == nil || !strings.Contains(err.Error(), "not a record Felis wrote") {
		t.Errorf("a record Felis did not write: err = %v", err)
	}
	putWriter(t, b, "../../etc", "prod-1", at)
	if _, _, err := testLease(t, "").Plan(context.Background(), b, true); err == nil {
		t.Error("a record with an id Felis does not make was taken")
	}
	b = newMemBucket()
	b.getErr = map[string]error{writerMark: errors.New("connection reset")}
	if _, _, err := testLease(t, "").Plan(context.Background(), b, true); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("an unreadable record: err = %v, want the read error", err)
	}
	b = newMemBucket()
	if _, _, err := testLease(t, "not-an-id").Plan(context.Background(), b, true); err == nil || !strings.Contains(err.Error(), "not a host id Felis wrote") {
		t.Errorf("a damaged id file: err = %v", err)
	}
}

func TestLeaseAcquire(t *testing.T) {
	ctx := context.Background()

	// A new host claims an empty bucket: its id is created and recorded.
	b := newMemBucket()
	l := testLease(t, "")
	if err := l.Acquire(ctx, b, true, now); err != nil {
		t.Fatal(err)
	}
	id, err := l.ID()
	if err != nil || !keyIDPattern.MatchString(id) {
		t.Fatalf("id after the claim = %q, %v", id, err)
	}
	if fi, err := os.Stat(l.IDFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("id file: %v, %v", fi, err)
	}
	w, err := BucketWriter(ctx, b)
	if err != nil || w == nil || w.HostID != id || w.Host != "spare-1" || !w.At.Equal(now) {
		t.Fatalf("record after the claim = %+v, %v", w, err)
	}

	// Its next run keeps the id and moves the time on.
	later := now.Add(time.Hour)
	if err := l.Acquire(ctx, b, false, later); err != nil {
		t.Fatal(err)
	}
	if w, _ := BucketWriter(ctx, b); w.HostID != id || !w.At.Equal(later) {
		t.Errorf("record after the next run = %+v, want %s at %s", w, id, later)
	}

	// A host restored from its backup stands by: nothing written, no id made.
	spare := testLease(t, "")
	puts := b.puts
	err = spare.Acquire(ctx, b, false, later)
	var we *WriterError
	if !errors.Is(err, ErrStandby) || !errors.As(err, &we) || we.Writer == nil || we.Writer.HostID != id {
		t.Fatalf("spare: err = %v, want ErrStandby naming %s", err, id)
	}
	if b.puts != puts {
		t.Error("the standby host wrote to the bucket")
	}
	if _, err := os.Stat(spare.IDFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the standby host made an id: %v", err)
	}

	// The spare takes over; the first host is displaced.
	prev, err := spare.TakeOver(ctx, b, later)
	if err != nil || prev == nil || prev.HostID != id {
		t.Fatalf("TakeOver = %+v, %v; want the first host replaced", prev, err)
	}
	spareID, _ := spare.ID()
	if spareID == "" || spareID == id {
		t.Fatalf("spare id after the take-over = %q", spareID)
	}
	puts = b.puts
	err = l.Acquire(ctx, b, false, later)
	if !errors.Is(err, ErrDisplaced) || !errors.As(err, &we) || we.Writer.HostID != spareID {
		t.Fatalf("first host after the take-over: err = %v, want ErrDisplaced naming %s", err, spareID)
	}
	if b.puts != puts {
		t.Error("the displaced host wrote to the bucket")
	}

	// A host name is kept printable and short for the messages that show it.
	b = newMemBucket()
	l = testLease(t, "")
	l.Host = "evil\x1b[2J\n" + strings.Repeat("x", 80)
	if err := l.Acquire(ctx, b, true, now); err != nil {
		t.Fatal(err)
	}
	if w, _ := BucketWriter(ctx, b); w.Host != "evil[2J"+strings.Repeat("x", 57) {
		t.Errorf("recorded host = %q", w.Host)
	}

	// A record that cannot be written fails the run.
	b = newMemBucket()
	b.putErr[writerMark] = errors.New("access denied")
	if err := testLease(t, "").Acquire(ctx, b, true, now); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("unwritable record: err = %v", err)
	}
}

func TestWriterErrorSays(t *testing.T) {
	w := &Writer{HostID: otherID, Host: "prod-1", At: now}
	for _, tc := range []struct {
		err  *WriterError
		kind error
		says []string
	}{
		{&WriterError{Kind: ErrStandby, Writer: w}, ErrStandby, []string{"host prod-1 (id " + otherID + ")", "2026-09-24T12:00:00Z", "built from its backup", "take-over -yes"}},
		{&WriterError{Kind: ErrStandby}, ErrStandby, []string{"names no host writing it", "take-over -yes"}},
		{&WriterError{Kind: ErrDisplaced, Writer: w}, ErrDisplaced, []string{"host prod-1 (id " + otherID + ") writes it now", "rehearsal machine", "take-over -yes"}},
	} {
		msg := tc.err.Error()
		if !errors.Is(tc.err, tc.kind) {
			t.Errorf("%q is not %v", msg, tc.kind)
		}
		for _, s := range tc.says {
			if !strings.Contains(msg, s) {
				t.Errorf("%q lacks %q", msg, s)
			}
		}
	}
}

func TestStandsBy(t *testing.T) {
	w := &Writer{HostID: otherID, Host: "prod-1", At: now.Add(-2 * time.Hour)}
	for _, tc := range []struct {
		what string
		st   *Status
		at   time.Time
		want bool
	}{
		{"a standby run, the writer seen two hours ago", &Status{Standby: true, Writer: w}, now, true},
		{"a standby run, the writer gone quiet", &Status{Standby: true, Writer: w}, now.Add(WriterLive), false},
		{"a standby run, no writer named", &Status{Standby: true}, now, false},
		{"a displaced run", &Status{Displaced: true, Writer: w}, now, false},
		{"a run that copied", &Status{Writer: w}, now, false},
		{"no run yet", nil, now, false},
	} {
		if got := tc.st.StandsBy(tc.at); (got != nil) != tc.want {
			t.Errorf("%s: StandsBy = %+v, want %v", tc.what, got, tc.want)
		}
	}
}

func TestHostLease(t *testing.T) {
	dir := t.TempDir()
	status := filepath.Join(dir, "status.json")
	if l := HostLease(status); l.IDFile != filepath.Join(dir, HostIDFile) || l.Inherited || l.Host == "" {
		t.Errorf("no status yet: %+v", l)
	}
	for _, tc := range []struct {
		what string
		st   Status
		want bool
	}{
		{"a status an older release wrote", Status{LastAttempt: now}, true},
		{"a status this release wrote", Status{LastAttempt: now, Format: StatusFormat}, false},
		{"a status that carries the older release's claim", Status{LastAttempt: now, Format: StatusFormat, Inherited: true}, true},
	} {
		if err := WriteStatus(status, tc.st); err != nil {
			t.Fatal(err)
		}
		if got := HostLease(status).Inherited; got != tc.want {
			t.Errorf("%s: Inherited = %v, want %v", tc.what, got, tc.want)
		}
	}
	if err := os.WriteFile(status, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if HostLease(status).Inherited {
		t.Error("an unreadable status counted as an older release's")
	}
}

// TestSyncStandsBy: a host restored from the writer's backup copies nothing
// into the bucket, prunes nothing, and records nothing as copied, whether the
// bucket names that host or holds its copies without naming anyone.
func TestSyncStandsBy(t *testing.T) {
	for _, named := range []bool{true, false} {
		t.Run(fmt.Sprintf("named=%v", named), func(t *testing.T) {
			cat := &fakeCatalog{rows: []*row{
				{WorldBackup: WorldBackup{ID: "b1", Server: "alpha", Ref: "/a/alpha-1.tar.gz"}, status: "present"},
			}}
			s, b := newSyncer(t, cat)
			delete(b.objs, keyMark)
			if named {
				putWriter(t, b, otherID, "prod-1", now.Add(-30*time.Minute))
			}
			writeFile(t, s.ArchiveDir, "alpha-1.tar.gz", 100)
			writeFile(t, s.DBDir, "felis-db-20260924T030000Z-daily.tar", 50)
			for i, name := range []string{"felis-db-20260901T030000Z-daily.tar", "felis-db-20260902T030000Z-daily.tar", "felis-db-20260903T030000Z-daily.tar"} {
				putAt(b, DBKey(name), seal(t, []byte(name), s.Key), i)
			}
			l := testLease(t, "")
			s.Lease = &l
			before := len(b.objs)

			_, err := s.Run(context.Background())
			if !errors.Is(err, ErrStandby) {
				t.Fatalf("Run error = %v, want ErrStandby", err)
			}
			if len(b.started) != 0 || len(b.removed) != 0 || len(b.objs) != before {
				t.Errorf("the standby run began puts %v and removed %v", b.started, b.removed)
			}
			if !cat.rows[0].offsite.IsZero() {
				t.Error("the standby run recorded alpha as copied")
			}
		})
	}
}

// TestSyncRecordsTheWriter: the first run records this host before the key
// id and the first upload.
func TestSyncRecordsTheWriter(t *testing.T) {
	s, b := newSyncer(t, &fakeCatalog{})
	delete(b.objs, keyMark)
	writeFile(t, s.DBDir, "felis-db-20260924T030000Z-daily.tar", 50)
	l := testLease(t, "")
	s.Lease = &l
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	id, _ := l.ID()
	if w, err := BucketWriter(context.Background(), b); err != nil || w == nil || w.HostID != id {
		t.Fatalf("record = %+v, %v; want %s", w, err, id)
	}
	if len(b.started) != 3 || b.started[0] != writerMark || b.started[1] != keyMark {
		t.Errorf("puts began in the order %v, want the writer, the key id, then the bundle", b.started)
	}
}
