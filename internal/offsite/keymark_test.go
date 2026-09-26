package offsite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// putAt stores data under key as modified at t0 plus min minutes.
func putAt(b *memBucket, key string, data []byte, min int) {
	if b.modified == nil {
		b.modified = map[string]time.Time{}
	}
	b.objs[key] = data
	b.modified[key] = now.Add(time.Duration(min) * time.Minute)
}

func TestCheckKey(t *testing.T) {
	key, other := testKey(t), testKey(t)
	big := make([]byte, 3*segmentSize)
	tailDamaged := seal(t, big, key)
	tailDamaged[len(tailDamaged)-1] ^= 1
	for _, tc := range []struct {
		what   string
		bucket func(b *memBucket)
		fit    KeyFit
		err    string   // "" for none
		named  []string // what a refusal names
		skip   []string // what it must leave out
	}{
		{what: "an empty bucket", bucket: func(b *memBucket) {}, fit: KeyUnused},
		{what: "nothing Felis sealed", bucket: func(b *memBucket) { putAt(b, "db/notes.txt", []byte("hi"), 0) }, fit: KeyUnused},
		{
			what: "the recorded id", fit: KeyRecorded,
			bucket: func(b *memBucket) {
				b.objs[keyMark] = []byte(KeyID(key) + "\n")
				putAt(b, "worlds/a.tar.gz.fenc", seal(t, []byte("a"), other), 0) // the id is the judge
			},
		},
		{
			what:   "another recorded id",
			bucket: func(b *memBucket) { b.objs[keyMark] = []byte(KeyID(other) + "\n") },
			err:    "sealed with another key", named: []string{KeyID(other), KeyID(key), "offsite.env"},
		},
		{
			what:   "a marker Felis did not write",
			bucket: func(b *memBucket) { b.objs[keyMark] = []byte("hello") },
			err:    "not a key id Felis wrote",
		},
		{
			what: "an unmarked bucket the key opens", fit: KeyOpens,
			bucket: func(b *memBucket) {
				putAt(b, "db/felis-db-20260101T030000Z-daily.tar.fenc", seal(t, []byte("old"), other), 0)
				putAt(b, "registry/blobs/aa.fenc", seal(t, []byte("new"), key), 10)
			},
		},
		{
			what: "only the first segment is read", fit: KeyOpens,
			bucket: func(b *memBucket) { putAt(b, "uploads/blobs/bb.fenc", tailDamaged, 0) },
		},
		{
			what: "an unmarked bucket sealed with another key",
			bucket: func(b *memBucket) {
				for i, k := range []string{"worlds/1.fenc", "worlds/2.fenc", "db/3.fenc", "uploads/index/4.json.fenc"} {
					putAt(b, k, seal(t, []byte(k), other), i)
				}
			},
			err: "sealed with another key", named: []string{"uploads/index/4.json.fenc", "db/3.fenc", "worlds/2.fenc", KeyID(key)}, skip: []string{"worlds/1.fenc"},
		},
		{
			what: "two refusals, then an object the key opens", fit: KeyOpens,
			bucket: func(b *memBucket) {
				putAt(b, "worlds/1.fenc", seal(t, []byte("1"), key), 0)
				putAt(b, "worlds/2.fenc", seal(t, []byte("2"), other), 1)
				putAt(b, "worlds/3.fenc", seal(t, []byte("3"), other), 2)
			},
		},
		{
			what: "damaged objects are passed over", fit: KeyOpens,
			bucket: func(b *memBucket) {
				putAt(b, "worlds/1.fenc", seal(t, []byte("1"), key), 0)
				putAt(b, "worlds/2.fenc", []byte("partial"), 1)
			},
		},
		{
			what: "a refusal among damaged objects",
			bucket: func(b *memBucket) {
				putAt(b, "worlds/1.fenc", seal(t, []byte("1"), other), 0)
				putAt(b, "worlds/2.fenc", []byte("partial"), 1)
			},
			err: "sealed with another key", named: []string{"worlds/1.fenc"},
		},
		{
			what: "nothing it can judge",
			bucket: func(b *memBucket) {
				putAt(b, "worlds/1.fenc", seal(t, []byte("1"), key), 0)
				for i := range keyTries {
					putAt(b, fmt.Sprintf("worlds/junk-%d.fenc", i), []byte("partial"), 1+i)
				}
			},
			err: "cannot tell which key", named: []string{"worlds/junk-"},
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			b := newMemBucket()
			tc.bucket(b)
			fit, err := CheckKey(context.Background(), b, key)
			if b.puts != 0 {
				t.Errorf("CheckKey wrote %d objects", b.puts)
			}
			if tc.err == "" {
				if err != nil || fit != tc.fit {
					t.Fatalf("CheckKey = %v, %v; want %v", fit, err, tc.fit)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("CheckKey = %v, %v; want an error saying %q", fit, err, tc.err)
			}
			if mismatch := tc.err == "sealed with another key"; errors.Is(err, ErrKeyMismatch) != mismatch {
				t.Errorf("errors.Is(err, ErrKeyMismatch) = %v, want %v: %v", !mismatch, mismatch, err)
			}
			for _, s := range tc.named {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error lacks %q: %v", s, err)
				}
			}
			for _, s := range tc.skip {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error names %q: %v", s, err)
				}
			}
		})
	}

	// A bucket that does not answer is not a mismatch.
	b := newMemBucket()
	putAt(b, "worlds/1.fenc", seal(t, []byte("1"), other), 0)
	down := errors.New("connection reset")
	b.getErr = map[string]error{"worlds/1.fenc": down}
	if _, err := CheckKey(context.Background(), b, key); !errors.Is(err, down) || errors.Is(err, ErrKeyMismatch) {
		t.Errorf("unreachable object: err = %v, want the read error", err)
	}
	b.getErr = map[string]error{keyMark: down}
	if _, err := CheckKey(context.Background(), b, key); !errors.Is(err, down) || errors.Is(err, ErrKeyMismatch) {
		t.Errorf("unreachable marker: err = %v, want the read error", err)
	}

	// An object pruned between the listing and the read is passed over.
	b = newMemBucket()
	putAt(b, "worlds/1.fenc", seal(t, []byte("1"), key), 0)
	putAt(b, "db/2.fenc", seal(t, []byte("2"), key), 1)
	b.getErr = map[string]error{"db/2.fenc": fmt.Errorf("%w: db/2.fenc", ErrNotFound)}
	if fit, err := CheckKey(context.Background(), b, key); fit != KeyOpens || err != nil {
		t.Errorf("an object gone since the listing: CheckKey = %v, %v; want KeyOpens", fit, err)
	}
}

func TestClaimKey(t *testing.T) {
	key, other := testKey(t), testKey(t)
	marker := func(b *memBucket) string { return string(b.objs[keyMark]) }

	b := newMemBucket()
	if err := ClaimKey(context.Background(), b, key); err != nil {
		t.Fatal(err)
	}
	if marker(b) != KeyID(key)+"\n" {
		t.Fatalf("empty bucket: marker = %q, want %s", marker(b), KeyID(key))
	}
	if err := ClaimKey(context.Background(), b, key); err != nil || b.puts != 1 {
		t.Errorf("second claim: err %v, puts %d; want the marker written once", err, b.puts)
	}
	if fit, err := CheckKey(context.Background(), b, key); fit != KeyRecorded || err != nil {
		t.Errorf("after the claim: CheckKey = %v, %v", fit, err)
	}
	if err := ClaimKey(context.Background(), b, other); !errors.Is(err, ErrKeyMismatch) || marker(b) != KeyID(key)+"\n" {
		t.Errorf("another key: err %v, marker %q; want a refusal that leaves the marker", err, marker(b))
	}

	b = newMemBucket()
	putAt(b, "worlds/1.fenc", seal(t, []byte("1"), key), 0)
	if err := ClaimKey(context.Background(), b, key); err != nil || marker(b) != KeyID(key)+"\n" {
		t.Errorf("unmarked bucket the key opens: err %v, marker %q", err, marker(b))
	}

	b = newMemBucket()
	putAt(b, "worlds/1.fenc", seal(t, []byte("1"), other), 0)
	if err := ClaimKey(context.Background(), b, key); !errors.Is(err, ErrKeyMismatch) || b.puts != 0 {
		t.Errorf("unmarked bucket under another key: err %v, puts %d; want a refusal that writes nothing", err, b.puts)
	}
}

// TestSyncRefusesAnotherKeysBucket: a host whose key does not match the
// bucket's objects neither copies into it nor prunes what only the right key
// opens.
func TestSyncRefusesAnotherKeysBucket(t *testing.T) {
	for _, marked := range []bool{true, false} {
		t.Run(fmt.Sprintf("marked=%v", marked), func(t *testing.T) {
			cat := &fakeCatalog{rows: []*row{
				{WorldBackup: WorldBackup{ID: "b1", Server: "alpha", Ref: "/a/alpha-1.tar.gz"}, status: "present"},
			}}
			s, b := newSyncer(t, cat)
			other := testKey(t)
			delete(b.objs, keyMark)
			if marked {
				b.objs[keyMark] = []byte(KeyID(other) + "\n")
			}
			writeFile(t, s.ArchiveDir, "alpha-1.tar.gz", 100)
			writeFile(t, s.DBDir, "felis-db-20260924T030000Z-daily.tar", 50)
			for i, name := range []string{"felis-db-20260901T030000Z-daily.tar", "felis-db-20260902T030000Z-daily.tar", "felis-db-20260903T030000Z-daily.tar"} {
				putAt(b, DBKey(name), seal(t, []byte(name), other), i)
			}
			before := len(b.objs)

			_, err := s.Run(context.Background())
			if !errors.Is(err, ErrKeyMismatch) {
				t.Fatalf("Run error = %v, want ErrKeyMismatch", err)
			}
			if len(b.started) != 0 || len(b.removed) != 0 || len(b.objs) != before {
				t.Errorf("the refused run began puts %v and removed %v", b.started, b.removed)
			}
			if !cat.rows[0].offsite.IsZero() {
				t.Error("the refused run recorded alpha as copied")
			}
		})
	}
}

// TestSyncRecordsTheKey: the first run into a bucket records its key before
// the first upload.
func TestSyncRecordsTheKey(t *testing.T) {
	s, b := newSyncer(t, &fakeCatalog{})
	delete(b.objs, keyMark)
	writeFile(t, s.DBDir, "felis-db-20260924T030000Z-daily.tar", 50)
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.objs[keyMark], []byte(KeyID(s.Key)+"\n")) {
		t.Fatalf("marker = %q, want %s", b.objs[keyMark], KeyID(s.Key))
	}
	if len(b.started) != 2 || b.started[0] != keyMark {
		t.Errorf("puts began in the order %v, want the marker, then the bundle", b.started)
	}
}
