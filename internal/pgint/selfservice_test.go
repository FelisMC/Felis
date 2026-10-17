//go:build pgint

package pgint

import (
	"context"
	"errors"
	"sync"
	"testing"

	"felis.lolicon.best/internal/api"
)

// setDefaultQuota stores the Owner's default quota the way PUT
// /settings/self-service does, and removes it when the test ends so the rest
// of the suite keeps the unlimited claims of users without a quota.
func setDefaultQuota(t *testing.T, raw string) {
	t.Helper()
	ctx := context.Background()
	if err := repo.SetSetting(ctx, "self_service_quota", []byte(raw)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM platform_settings WHERE key = 'self_service_quota'`)
	})
}

func seedOwnerless(t *testing.T, name string, memMB int) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, 0, $2, 1024)`, name, memMB); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
}

// A user with no quota of their own creates nothing while the default quota is
// off, and within it once it is on: one server for a one-server default however
// many creates race, and claims and resizes held to the same caps.
func TestSeedOwnedServerUnderTheDefaultQuota(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "self")
	res := api.ResourceSpec{MemoryMB: 2048, StorageMB: 10240}

	if err := repo.SeedOwnedServer(ctx, "off-"+suffix(t), "off-"+suffix(t), u.ID, res); !errors.Is(err, api.ErrSelfServiceDisabled) {
		t.Fatalf("create with the default quota unset = %v, want ErrSelfServiceDisabled", err)
	}
	setDefaultQuota(t, `{"enabled":false,"max_servers":1,"max_cpu_milli":null,"max_memory_mb":4096,"max_storage_gb":null}`)
	if err := repo.SeedOwnedServer(ctx, "off-"+suffix(t), "off-"+suffix(t), u.ID, res); !errors.Is(err, api.ErrSelfServiceDisabled) {
		t.Fatalf("create with the default quota off = %v, want ErrSelfServiceDisabled", err)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM servers WHERE owner_id = $1`, u.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused creates left %d rows (%v)", rows, err)
	}

	setDefaultQuota(t, `{"enabled":true,"max_servers":1,"max_cpu_milli":null,"max_memory_mb":4096,"max_storage_gb":null}`)
	names := []string{"race-a-" + suffix(t), "race-b-" + suffix(t)}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			errs[i] = repo.SeedOwnedServer(ctx, n, n, u.ID, res)
		}(i, n)
	}
	wg.Wait()
	var won string
	for i, err := range errs {
		switch {
		case err == nil:
			won = names[i]
		case !errors.Is(err, api.ErrQuotaExceeded):
			t.Fatalf("create %s = %v", names[i], err)
		}
	}
	if won == "" || (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("concurrent creates under a one-server quota: %v, want exactly one through", errs)
	}
	rec, err := repo.ServerByName(ctx, won)
	if err != nil || rec.OwnerID != u.ID {
		t.Fatalf("the created server = %+v, %v; want it owned by the creator", rec, err)
	}
	if bySub, err := repo.ServerBySubdomain(ctx, won); err != nil || bySub.Name != won {
		t.Fatalf("subdomain %s routes to %+v, %v", won, bySub, err)
	}

	ownerless := "claim-" + suffix(t)
	seedOwnerless(t, ownerless, 1024)
	if _, err := repo.ClaimServer(ctx, ownerless, u.ID); !errors.Is(err, api.ErrQuotaExceeded) {
		t.Fatalf("claim past the default quota = %v, want ErrQuotaExceeded", err)
	}
	if _, err := repo.ResizeServer(ctx, won, 0, 8192); !errors.Is(err, api.ErrQuotaExceeded) {
		t.Fatalf("resize past the default memory cap = %v, want ErrQuotaExceeded", err)
	}

	// The cluster refused the server: discarding it gives the slot and the
	// subdomain back.
	if err := repo.DiscardSeededServer(ctx, won); err != nil {
		t.Fatal(err)
	}
	next := "next-" + suffix(t)
	if err := repo.SeedOwnedServer(ctx, next, won, u.ID, res); err != nil {
		t.Fatalf("create after the discard, on the freed subdomain = %v", err)
	}
}

// A user's own quota wins over the default, whatever the default says.
func TestSeedOwnedServerUnderTheUsersOwnQuota(t *testing.T) {
	ctx := context.Background()
	setDefaultQuota(t, `{"enabled":true,"max_servers":0,"max_cpu_milli":null,"max_memory_mb":null,"max_storage_gb":null}`)
	u := newUser(t, "user", "own")
	two := 2
	if _, err := repo.SetQuotas(ctx, u.ID, api.QuotaInput{MaxServers: &two}, "pgint"); err != nil {
		t.Fatal(err)
	}
	res := api.ResourceSpec{MemoryMB: 1024}
	for i := 0; i < 2; i++ {
		n := "own-" + suffix(t)
		if err := repo.SeedOwnedServer(ctx, n, n, u.ID, res); err != nil {
			t.Fatalf("create %d under the user's own two-server quota = %v", i, err)
		}
	}
	n := "own-" + suffix(t)
	if err := repo.SeedOwnedServer(ctx, n, n, u.ID, res); !errors.Is(err, api.ErrQuotaExceeded) {
		t.Fatalf("third create = %v, want ErrQuotaExceeded", err)
	}

	st, err := repo.SelfServiceState(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Source != "user" || st.Limits == nil || st.Limits.MaxServers == nil || *st.Limits.MaxServers != 2 ||
		st.Usage.Servers != 2 || st.Usage.MemoryMB != 2048 {
		t.Errorf("state = %+v (limits %+v), want the user's own two-server quota with both servers counted", st, st.Limits)
	}

	// A user without a row reads the default (zero servers granted here).
	other := newUser(t, "user", "default")
	if st, err := repo.SelfServiceState(ctx, other.ID); err != nil || st.Source != "default" ||
		st.Limits == nil || st.Limits.MaxServers == nil || *st.Limits.MaxServers != 0 {
		t.Errorf("state without a row = %+v, %v; want the default quota", st, err)
	}

	if v, err := repo.GetQuotas(ctx, u.ID); err != nil || !v.Custom || v.MaxServers == nil || *v.MaxServers != 2 {
		t.Fatalf("GetQuotas with an own row = %+v, %v; want custom with two servers", v, err)
	}

	// Removing the user's own row hands them back to the default, which grants
	// no servers: the next create is refused, and the read reports no own row.
	if v, err := repo.ClearQuotas(ctx, u.ID); err != nil || v.Custom {
		t.Fatalf("ClearQuotas = %+v, %v", v, err)
	}
	if v, err := repo.GetQuotas(ctx, u.ID); err != nil || v.Custom || v.MaxServers != nil {
		t.Fatalf("GetQuotas after clearing = %+v, %v; want no own row", v, err)
	}
	n = "own-" + suffix(t)
	if err := repo.SeedOwnedServer(ctx, n, n, u.ID, res); !errors.Is(err, api.ErrQuotaExceeded) {
		t.Fatalf("create under the default after clearing = %v, want ErrQuotaExceeded", err)
	}
	if _, err := repo.ClearQuotas(ctx, "no-such-user"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("ClearQuotas for an unknown user = %v, want ErrNotFound", err)
	}
}
