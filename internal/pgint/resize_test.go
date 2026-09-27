//go:build pgint

package pgint

import (
	"context"
	"errors"
	"testing"

	"felis.lolicon.best/internal/api"
)

// ---- server resize (ResizeServer) --------------------------------------------------

func seedSized(t *testing.T, name, ownerID string, size api.ResourceSpec) {
	t.Helper()
	var owner any
	if ownerID != "" {
		owner = ownerID
	}
	mustExec(t, `INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb, owner_id)
		VALUES ($1, $2, $3, $4, $5)`, name, size.CPUMilli, size.MemoryMB, size.StorageMB, owner)
}

func cachedSize(t *testing.T, name string) api.ResourceSpec {
	t.Helper()
	var s api.ResourceSpec
	if err := db.QueryRow(`SELECT cached_cpu_milli, cached_memory_mb, cached_storage_mb FROM servers WHERE name = $1`,
		name).Scan(&s.CPUMilli, &s.MemoryMB, &s.StorageMB); err != nil {
		t.Fatalf("cached size of %s: %v", name, err)
	}
	return s
}

// A resize that grows CPU or memory must fit every cap with the server's whole size
// counted, its storage and itself among the owner's servers included; one that does
// not writes nothing. A resize that grows neither is written whatever the caps say,
// and so is any resize of a server nobody owns.
func TestResizeServerQuota(t *testing.T) {
	ctx := context.Background()
	n := func(v int) *int { return &v }
	u := newUser(t, "user", "rsz")
	sfx := suffix(t)
	a, b := "rsa-"+sfx, "rsb-"+sfx
	seedSized(t, a, u.ID, api.ResourceSpec{CPUMilli: 1000, MemoryMB: 2048, StorageMB: 400})
	seedSized(t, b, u.ID, api.ResourceSpec{CPUMilli: 1000, MemoryMB: 2048, StorageMB: 400})
	if _, err := repo.SetQuotas(ctx, u.ID, api.QuotaInput{MaxServers: n(2), MaxCPUMilli: n(3000),
		MaxMemoryMB: n(6144), MaxStorageGB: n(1)}, "pgint"); err != nil {
		t.Fatalf("SetQuotas: %v", err)
	}

	for _, c := range []struct {
		name     string
		cpu, mem int
		refused  bool
		after    api.ResourceSpec
	}{
		{"cpu grows to the cap", 2000, 2048, false, api.ResourceSpec{CPUMilli: 2000, MemoryMB: 2048, StorageMB: 400}},
		{"cpu grows past the cap", 2001, 2048, true, api.ResourceSpec{CPUMilli: 2000, MemoryMB: 2048, StorageMB: 400}},
		{"memory grows past the cap", 2000, 4097, true, api.ResourceSpec{CPUMilli: 2000, MemoryMB: 2048, StorageMB: 400}},
		{"memory grows to the cap as cpu shrinks", 1500, 4096, false, api.ResourceSpec{CPUMilli: 1500, MemoryMB: 4096, StorageMB: 400}},
	} {
		prevWant := cachedSize(t, a)
		prev, err := repo.ResizeServer(ctx, a, c.cpu, c.mem)
		switch {
		case c.refused && !errors.Is(err, api.ErrQuotaExceeded):
			t.Fatalf("%s: err = %v, want ErrQuotaExceeded", c.name, err)
		case !c.refused && (err != nil || prev != prevWant):
			t.Fatalf("%s: prev = %+v, err = %v; want %+v, nil", c.name, prev, err, prevWant)
		}
		if got := cachedSize(t, a); got != c.after {
			t.Fatalf("%s: cache = %+v, want %+v", c.name, got, c.after)
		}
	}

	// Storage over its cap (an admin lowered it, or the other server grew): growth
	// is refused though it fits CPU and memory, and a shrink still goes through.
	mustExec(t, `UPDATE servers SET cached_storage_mb = 700 WHERE name = $1`, b)
	if _, err := repo.ResizeServer(ctx, a, 1600, 4096); !errors.Is(err, api.ErrQuotaExceeded) {
		t.Fatalf("growth with storage over its cap: err = %v, want ErrQuotaExceeded", err)
	}
	if _, err := repo.ResizeServer(ctx, a, 1000, 2048); err != nil {
		t.Fatalf("shrink with storage over its cap: %v", err)
	}
	if got, want := cachedSize(t, a), (api.ResourceSpec{CPUMilli: 1000, MemoryMB: 2048, StorageMB: 400}); got != want {
		t.Fatalf("cache after the shrink = %+v, want %+v", got, want)
	}

	free := "rsf-" + sfx
	seedSized(t, free, "", api.ResourceSpec{CPUMilli: 500, MemoryMB: 1024, StorageMB: 400})
	if prev, err := repo.ResizeServer(ctx, free, 8000, 16384); err != nil || prev.CPUMilli != 500 {
		t.Fatalf("resize of an unowned server = %+v, %v; want prev cpu 500, nil", prev, err)
	}
	if got, want := cachedSize(t, free), (api.ResourceSpec{CPUMilli: 8000, MemoryMB: 16384, StorageMB: 400}); got != want {
		t.Fatalf("unowned cache = %+v, want %+v", got, want)
	}

	if prev, err := repo.ResizeServer(ctx, "rsnone-"+sfx, 1000, 1024); err != nil || prev != (api.ResourceSpec{}) {
		t.Fatalf("resize of no server = %+v, %v; want zeroes, nil", prev, err)
	}
	if rowExists(t, `SELECT 1 FROM servers WHERE name = $1`, "rsnone-"+sfx) {
		t.Fatal("a resize of no server made a row")
	}
}

// A resize decides in its owner's claim lane: a server the owner claims while the
// resize waits is counted, and here tips the owner over the CPU cap.
func TestResizeServerWaitsForClaims(t *testing.T) {
	ctx := context.Background()
	n := func(v int) *int { return &v }
	u := newUser(t, "user", "rszw")
	sfx := suffix(t)
	a, claimed := "rswa-"+sfx, "rswc-"+sfx
	seedSized(t, a, u.ID, api.ResourceSpec{CPUMilli: 1000, MemoryMB: 1024, StorageMB: 1})
	seedSized(t, claimed, "", api.ResourceSpec{CPUMilli: 1000, MemoryMB: 1024, StorageMB: 1})
	if _, err := repo.SetQuotas(ctx, u.ID, api.QuotaInput{MaxCPUMilli: n(2000)}, "pgint"); err != nil {
		t.Fatalf("SetQuotas: %v", err)
	}

	hold := holdLane(t, u.ID)
	defer hold.Rollback() //nolint:errcheck // no-op after commit
	resized := make(chan error, 1)
	go func() {
		_, err := repo.ResizeServer(ctx, a, 1500, 1024)
		resized <- err
	}()
	waitForLockWait(t, "SELECT pg_advisory_xact_lock")
	if _, err := hold.Exec(`UPDATE servers SET owner_id = $2 WHERE name = $1`, claimed, u.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := hold.Commit(); err != nil {
		t.Fatalf("claim commit: %v", err)
	}
	if err := <-resized; !errors.Is(err, api.ErrQuotaExceeded) {
		t.Fatalf("resize behind a claim = %v, want ErrQuotaExceeded", err)
	}
	if got := cachedSize(t, a); got.CPUMilli != 1000 {
		t.Fatalf("cache = %+v, want cpu still 1000", got)
	}
}

// A server that changes hands while its resize waits on the old owner's lane is
// judged against the new owner's caps, under the new owner's lane.
func TestResizeServerFollowsNewOwner(t *testing.T) {
	ctx := context.Background()
	n := func(v int) *int { return &v }
	was := newUser(t, "user", "rszo-was")
	now := newUser(t, "user", "rszo-now")
	a := "rsoa-" + suffix(t)
	seedSized(t, a, was.ID, api.ResourceSpec{CPUMilli: 1000, MemoryMB: 1024, StorageMB: 1})
	if _, err := repo.SetQuotas(ctx, now.ID, api.QuotaInput{MaxCPUMilli: n(1000)}, "pgint"); err != nil {
		t.Fatalf("SetQuotas: %v", err)
	}

	hold := holdLane(t, was.ID)
	defer hold.Rollback() //nolint:errcheck // no-op after commit
	resized := make(chan error, 1)
	go func() {
		_, err := repo.ResizeServer(ctx, a, 1500, 1024)
		resized <- err
	}()
	waitForLockWait(t, "SELECT pg_advisory_xact_lock")
	mustExec(t, `UPDATE servers SET owner_id = $2 WHERE name = $1`, a, now.ID)
	if err := hold.Commit(); err != nil {
		t.Fatalf("release lane: %v", err)
	}
	if err := <-resized; !errors.Is(err, api.ErrQuotaExceeded) {
		t.Fatalf("resize of a server that changed hands = %v, want ErrQuotaExceeded from the new owner's cap", err)
	}
	if got := cachedSize(t, a); got.CPUMilli != 1000 {
		t.Fatalf("cache = %+v, want cpu still 1000", got)
	}
}

// A server nobody owns has no lane to take, so its row lock is what orders a resize
// against a claim: a resize that reaches the row while a claim holds it waits, finds
// the claimer's name on it, and is judged against the claimer's caps.
func TestResizeServerWaitsForAClaimOfAFreeServer(t *testing.T) {
	ctx := context.Background()
	n := func(v int) *int { return &v }
	claimer := newUser(t, "user", "rszf")
	free := "rsfc-" + suffix(t)
	seedSized(t, free, "", api.ResourceSpec{CPUMilli: 1000, MemoryMB: 1024, StorageMB: 1})
	if _, err := repo.SetQuotas(ctx, claimer.ID, api.QuotaInput{MaxCPUMilli: n(1000)}, "pgint"); err != nil {
		t.Fatalf("SetQuotas: %v", err)
	}

	hold, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer hold.Rollback() //nolint:errcheck // no-op after commit
	if _, err := hold.Exec(`SELECT 1 FROM servers WHERE name = $1 FOR UPDATE`, free); err != nil {
		t.Fatalf("hold server: %v", err)
	}
	if _, err := hold.Exec(`UPDATE servers SET owner_id = $2 WHERE name = $1`, free, claimer.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	resized := make(chan error, 1)
	go func() {
		_, err := repo.ResizeServer(ctx, free, 1500, 1024)
		resized <- err
	}()
	waitForLockWait(t, "SELECT owner_id, cached_cpu_milli")
	if err := hold.Commit(); err != nil {
		t.Fatalf("claim commit: %v", err)
	}
	if err := <-resized; !errors.Is(err, api.ErrQuotaExceeded) {
		t.Fatalf("resize behind a claim of a free server = %v, want ErrQuotaExceeded", err)
	}
	if got := cachedSize(t, free); got.CPUMilli != 1000 {
		t.Fatalf("cache = %+v, want cpu still 1000", got)
	}
}
