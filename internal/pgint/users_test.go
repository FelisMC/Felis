//go:build pgint

package pgint

import (
	"context"
	"net/url"
	"os"
	"testing"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/store"
)

// ---- user admin ----------------------------------------------------------------

// A user's detail whose linked accounts cannot be read is an error. It used to come
// back with an empty list, which tells the admin the user has linked nothing.
func TestUserDetailLinkReadFailure(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "udl")
	mc := testUUID(t)
	mustExec(t, `INSERT INTO account_links (user_id, mc_uuid) VALUES ($1, $2)`, u.ID, mc)

	// A role that reads users and servers and nothing else.
	const role = "pgint_nolinks"
	drop := func() {
		for _, stmt := range []string{"DROP OWNED BY " + role, "DROP ROLE " + role} {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Errorf("%s: %v", stmt, err)
			}
		}
	}
	mustExec(t, "CREATE ROLE "+role+" LOGIN PASSWORD 'pgint'")
	t.Cleanup(drop)
	mustExec(t, "GRANT USAGE ON SCHEMA public TO "+role)
	mustExec(t, "GRANT SELECT ON users, servers TO "+role)
	dsn, err := url.Parse(os.Getenv("FELIS_TEST_PG_URL"))
	if err != nil {
		t.Fatal(err)
	}
	dsn.User = url.UserPassword(role, "pgint")
	drv, err := store.Open(ctx, dsn.String())
	if err != nil {
		t.Fatalf("open as %s: %v", role, err)
	}
	defer drv.Close()
	limited := api.NewPGRepo(drv.DB())

	if d, err := limited.UserDetail(ctx, u.ID); err == nil {
		t.Fatalf("UserDetail with account_links unreadable = %+v, nil; want its error", d)
	}
	mustExec(t, "GRANT SELECT ON account_links TO "+role)
	d, err := limited.UserDetail(ctx, u.ID)
	if err != nil || d.Username != u.Username || len(d.LinkedAccounts) != 1 || d.LinkedAccounts[0].MCUUID != mc {
		t.Fatalf("UserDetail with account_links readable = %+v, %v; want %s with %s linked", d, err, u.Username, mc)
	}
}
