package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"felis.lolicon.best/internal/store"
)

func TestLoadMigrationsOrderedAndWellFormed(t *testing.T) {
	ms, err := store.LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("expected at least one migration")
	}
	if ms[0].Version != 1 || ms[0].Name != "init" {
		t.Errorf("first migration = %d_%s, want 0001_init", ms[0].Version, ms[0].Name)
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].Version <= ms[i-1].Version {
			t.Errorf("migrations not strictly ascending at %d: %d then %d", i, ms[i-1].Version, ms[i].Version)
		}
	}
	// The init migration must define the core business tables (spec §6).
	for _, want := range []string{"CREATE TABLE users", "CREATE TABLE servers", "CREATE TABLE world_backups"} {
		if !strings.Contains(ms[0].SQL, want) {
			t.Errorf("init migration missing %q", want)
		}
	}
}

// recordingDriver captures the migration engine's calls without a database.
type recordingDriver struct {
	already            map[int]struct{}
	applied            []int
	locked             bool
	unlocked           bool
	ensured            bool
	appliedWhileUnsafe bool // true if Apply ran while not locked or already unlocked
	failOn             int  // version whose Apply should fail (0 = never)
}

func (d *recordingDriver) Lock(context.Context) error   { d.locked = true; return nil }
func (d *recordingDriver) Unlock(context.Context) error { d.unlocked = true; return nil }
func (d *recordingDriver) EnsureVersionTable(context.Context) error {
	if !d.locked || d.unlocked {
		d.appliedWhileUnsafe = true
	}
	d.ensured = true
	return nil
}
func (d *recordingDriver) AppliedVersions(context.Context) (map[int]struct{}, error) {
	if d.already == nil {
		return map[int]struct{}{}, nil
	}
	return d.already, nil
}
func (d *recordingDriver) Apply(_ context.Context, m store.Migration) error {
	if !d.locked || d.unlocked {
		d.appliedWhileUnsafe = true
	}
	if d.failOn != 0 && m.Version == d.failOn {
		return errors.New("boom")
	}
	d.applied = append(d.applied, m.Version)
	return nil
}

func TestUpAppliesAllPendingInOrder(t *testing.T) {
	d := &recordingDriver{}
	ms := []store.Migration{
		{Version: 3, Name: "c", SQL: "x"},
		{Version: 1, Name: "a", SQL: "y"},
		{Version: 2, Name: "b", SQL: "z"},
	}
	applied, err := store.Up(context.Background(), d, ms)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got := strings.Trim(strings.Join(intsToStrings(applied), ","), ""); got != "1,2,3" {
		t.Errorf("applied = %v, want [1 2 3]", applied)
	}
	if !d.locked || !d.unlocked || !d.ensured {
		t.Errorf("lifecycle flags: locked=%v unlocked=%v ensured=%v", d.locked, d.unlocked, d.ensured)
	}
	if d.appliedWhileUnsafe {
		t.Error("work ran outside the advisory lock")
	}
}

func TestUpSkipsAlreadyApplied(t *testing.T) {
	d := &recordingDriver{already: map[int]struct{}{1: {}}}
	ms := []store.Migration{
		{Version: 1, Name: "a", SQL: "y"},
		{Version: 2, Name: "b", SQL: "z"},
	}
	applied, err := store.Up(context.Background(), d, ms)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(applied) != 1 || applied[0] != 2 {
		t.Errorf("applied = %v, want [2]", applied)
	}
}

func TestUpStopsOnErrorButStillUnlocks(t *testing.T) {
	d := &recordingDriver{failOn: 2}
	ms := []store.Migration{
		{Version: 1, Name: "a", SQL: "y"},
		{Version: 2, Name: "b", SQL: "z"},
		{Version: 3, Name: "c", SQL: "x"},
	}
	applied, err := store.Up(context.Background(), d, ms)
	if err == nil {
		t.Fatal("expected an error when a migration fails")
	}
	if len(applied) != 1 || applied[0] != 1 {
		t.Errorf("applied = %v, want only [1] before the failure", applied)
	}
	if !d.unlocked {
		t.Error("advisory lock must be released even when a migration fails")
	}
}

func intsToStrings(in []int) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = itoa(v)
	}
	return out
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestUpRefusesADatabaseANewerBuildMigrated(t *testing.T) {
	d := &recordingDriver{already: map[int]struct{}{1: {}, 2: {}, 3: {}}}
	ms := []store.Migration{
		{Version: 1, Name: "a", SQL: "y"},
		{Version: 2, Name: "b", SQL: "z"},
	}
	_, err := store.Up(context.Background(), d, ms)
	if !errors.Is(err, store.ErrSchemaNewer) {
		t.Fatalf("Up err = %v, want ErrSchemaNewer", err)
	}
	if !strings.Contains(err.Error(), "migration 0003") || !strings.Contains(err.Error(), "up to 0002") {
		t.Errorf("error does not name the versions: %v", err)
	}
	if !d.unlocked {
		t.Error("the advisory lock was not released")
	}
}

func TestCompareSchemaSeparatesPendingFromUnknown(t *testing.T) {
	ms := []store.Migration{{Version: 1}, {Version: 2}, {Version: 4}}
	cases := []struct {
		name    string
		applied map[int]struct{}
		pending []int
		unknown []int
		err     error
	}{
		{"current", map[int]struct{}{1: {}, 2: {}, 4: {}}, nil, nil, nil},
		{"fresh", map[int]struct{}{}, []int{1, 2, 4}, nil, store.ErrSchemaBehind},
		{"behind", map[int]struct{}{1: {}}, []int{2, 4}, nil, store.ErrSchemaBehind},
		// Same row count as "current": a count comparison calls this up to date.
		{"newer", map[int]struct{}{1: {}, 2: {}, 5: {}}, []int{4}, []int{5}, store.ErrSchemaNewer},
	}
	for _, c := range cases {
		s := store.CompareSchema(c.applied, ms)
		if fmt.Sprint(s.Pending) != fmt.Sprint(c.pending) || fmt.Sprint(s.Unknown) != fmt.Sprint(c.unknown) {
			t.Errorf("%s: pending=%v unknown=%v, want %v %v", c.name, s.Pending, s.Unknown, c.pending, c.unknown)
		}
		if err := s.Err(); !errors.Is(err, c.err) || (c.err == nil && err != nil) {
			t.Errorf("%s: Err() = %v, want %v", c.name, err, c.err)
		}
		if s.Total != 3 || s.Latest != 4 {
			t.Errorf("%s: Total=%d Latest=%d, want 3 4", c.name, s.Total, s.Latest)
		}
	}
}
