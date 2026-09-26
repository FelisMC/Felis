package offsite

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/dbbackup"
)

var dbT0 = time.Date(2026, 9, 20, 3, 30, 0, 0, time.UTC)

// dbBundle is a bundle as dbbackup.Backup lays it out: MANIFEST.json, then a
// dump of dumpSize random bytes.
func dbBundle(t *testing.T, created time.Time, counts *dbbackup.Counts, dumpSize int) []byte {
	t.Helper()
	dump := make([]byte, dumpSize)
	rand.Read(dump)
	sum := sha256.Sum256(dump)
	m := dbbackup.Manifest{
		Format: 1, CreatedAt: created, Label: dbbackup.LabelDaily, FelisVersion: "v1.2.3", SchemaVersion: 21, Counts: counts,
		Files: []dbbackup.ManifestEntry{{Name: "db.dump", Size: int64(dumpSize), SHA256: hex.EncodeToString(sum[:]), Mode: 0o600}},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name string
		data []byte
	}{{"MANIFEST.json", raw}, {"db.dump", dump}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.data)), ModTime: created, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// putDB seals a bundle taken daysAgo days before dbT0 into b and returns its name.
func putDB(t *testing.T, b *memBucket, key []byte, daysAgo int, counts *dbbackup.Counts) string {
	t.Helper()
	created := dbT0.AddDate(0, 0, -daysAgo)
	name := dbbackup.BundleName(created, dbbackup.LabelDaily)
	b.objs[DBKey(name)] = seal(t, dbBundle(t, created, counts, 100), key)
	return name
}

func TestPeekDBReadsOnlyTheManifest(t *testing.T) {
	key := testKey(t)
	b := newMemBucket()
	name := dbbackup.BundleName(dbT0, dbbackup.LabelDaily)
	sealed := seal(t, dbBundle(t, dbT0, &dbbackup.Counts{Users: 4, Servers: 2}, 5*segmentSize), key)

	// The last segment is damaged: a whole download fails, the peek never gets there.
	tail := bytes.Clone(sealed)
	tail[len(tail)-1] ^= 1
	b.objs[DBKey(name)] = tail
	m, err := PeekDB(context.Background(), b, key, name)
	if err != nil {
		t.Fatalf("PeekDB read past the manifest: %v", err)
	}
	if !m.CreatedAt.Equal(dbT0) || m.FelisVersion != "v1.2.3" || m.Counts == nil || *m.Counts != (dbbackup.Counts{Users: 4, Servers: 2}) {
		t.Errorf("manifest = %+v", m)
	}

	// The segment holding the manifest is authenticated like any other.
	head := bytes.Clone(sealed)
	head[headerSize+10] ^= 1
	b.objs[DBKey(name)] = head
	if _, err := PeekDB(context.Background(), b, key, name); !errors.Is(err, ErrAuth) {
		t.Errorf("damaged manifest segment: err = %v, want ErrAuth", err)
	}
	b.objs[DBKey(name)] = sealed
	if _, err := PeekDB(context.Background(), b, testKey(t), name); !errors.Is(err, ErrAuth) {
		t.Errorf("another key: err = %v, want ErrAuth", err)
	}
	b.objs[DBKey(name)] = seal(t, []byte("not a bundle"), key)
	if _, err := PeekDB(context.Background(), b, key, name); err == nil || !strings.Contains(err.Error(), "not a felis database bundle") {
		t.Errorf("sealed junk: err = %v", err)
	}
	if _, err := PeekDB(context.Background(), b, key, "felis-db-20260101T000000Z-daily.tar"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing bundle: err = %v, want ErrNotFound", err)
	}
}

func TestChooseDB(t *testing.T) {
	c := func(users, servers int) *dbbackup.Counts { return &dbbackup.Counts{Users: users, Servers: servers} }
	type put struct {
		daysAgo int
		counts  *dbbackup.Counts
	}
	for _, tc := range []struct {
		what    string
		bundles []put
		pick    int      // index into bundles, or -1 for a refusal
		named   []int    // bundles the refusal names, in order
		unnamed []int    // bundles it must leave out
		says    []string // what else the refusal says
	}{
		{what: "the newest holds data", bundles: []put{{1, c(5, 3)}, {0, c(5, 2)}}, pick: 1},
		{what: "the newest predates counts", bundles: []put{{1, c(5, 3)}, {0, nil}}, pick: 1},
		{what: "only the owner and a server", bundles: []put{{1, c(5, 3)}, {0, c(1, 1)}}, pick: 1},
		{what: "a new install with nothing older", bundles: []put{{0, c(0, 0)}}, pick: 0},
		{what: "a new install over older empty ones", bundles: []put{{2, c(0, 0)}, {1, c(1, 0)}, {0, c(1, 0)}}, pick: 2},
		{
			what: "a rebuilt host's first backup", bundles: []put{{3, c(5, 3)}, {2, c(0, 0)}, {1, c(6, 3)}, {0, c(0, 0)}},
			pick: -1, named: []int{2, 0}, unnamed: []int{1}, says: []string{"0 accounts, 0 servers", "6 accounts, 3 servers", "5 accounts, 3 servers"},
		},
		{
			what: "the rebuilt host's owner already set up", bundles: []put{{1, c(2, 0)}, {0, c(1, 0)}},
			pick: -1, named: []int{0}, says: []string{"1 account, 0 servers", "2 accounts, 0 servers"},
		},
		{
			what: "servers the owner made before the loss", bundles: []put{{1, c(1, 2)}, {0, c(1, 0)}},
			pick: -1, named: []int{0}, says: []string{"1 account, 2 servers"},
		},
		{
			what: "older bundles from before counts", bundles: []put{{1, nil}, {0, c(0, 0)}},
			pick: -1, named: []int{0}, says: []string{"(not recorded)"},
		},
		{
			what: "at most three named", bundles: []put{{5, c(9, 9)}, {4, c(8, 8)}, {3, c(7, 7)}, {2, c(6, 6)}, {1, c(0, 0)}},
			pick: -1, named: []int{3, 2, 1}, unnamed: []int{0}, says: []string{"felis offsite list"},
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			key := testKey(t)
			b := newMemBucket()
			var names []string
			for _, p := range tc.bundles {
				names = append(names, putDB(t, b, key, p.daysAgo, p.counts))
			}
			name, m, err := ChooseDB(context.Background(), b, key)
			if tc.pick >= 0 {
				if err != nil || name != names[tc.pick] {
					t.Fatalf("ChooseDB = %q, %v; want %s", name, err, names[tc.pick])
				}
				if want := tc.bundles[tc.pick].counts; (m.Counts == nil) != (want == nil) || (want != nil && *m.Counts != *want) {
					t.Errorf("manifest counts = %v, want %v", m.Counts, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ChooseDB picked %s, want a refusal", name)
			}
			msg := err.Error()
			last := -1
			for _, i := range tc.named {
				at := strings.Index(msg, names[i])
				if at < 0 {
					t.Fatalf("refusal does not name %s: %s", names[i], msg)
				}
				if at < last {
					t.Errorf("refusal names %s out of order (newest first): %s", names[i], msg)
				}
				last = at
			}
			for _, i := range tc.unnamed {
				if strings.Contains(msg, names[i]) {
					t.Errorf("refusal names %s: %s", names[i], msg)
				}
			}
			for _, s := range tc.says {
				if !strings.Contains(msg, s) {
					t.Errorf("refusal lacks %q: %s", s, msg)
				}
			}
		})
	}

	if _, _, err := ChooseDB(context.Background(), newMemBucket(), testKey(t)); err == nil || !strings.Contains(err.Error(), "no database bundle") {
		t.Errorf("empty bucket: err = %v", err)
	}

	// A bundle it cannot read stops the choice, named.
	key := testKey(t)
	b := newMemBucket()
	putDB(t, b, key, 2, &dbbackup.Counts{Users: 5, Servers: 3})
	other := putDB(t, b, testKey(t), 1, &dbbackup.Counts{Users: 5, Servers: 3})
	newest := putDB(t, b, key, 0, &dbbackup.Counts{})
	if _, _, err := ChooseDB(context.Background(), b, key); !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), other) {
		t.Errorf("an older bundle under another key: err = %v, want ErrAuth naming %s", err, other)
	}
	if _, _, err := ChooseDB(context.Background(), b, testKey(t)); !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), newest) {
		t.Errorf("the wrong key: err = %v, want ErrAuth naming %s", err, newest)
	}
}
