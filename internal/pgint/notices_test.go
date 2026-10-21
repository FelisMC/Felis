//go:build pgint

package pgint

import (
	"context"
	"slices"
	"testing"

	"felis.lolicon.best/internal/api"
)

func TestServerNoticeRecipients(t *testing.T) {
	ctx := context.Background()
	sfx := suffix(t)
	user := func(name, role string, verified, disabled bool) *api.UserView {
		t.Helper()
		u, err := repo.CreateUser(ctx, api.CreateUserInput{Username: name + "-" + sfx, Email: name + "-" + sfx + "@example.test", Role: role}, "pgint")
		if err != nil {
			t.Fatalf("CreateUser(%s): %v", name, err)
		}
		mustExec(t, `UPDATE users SET email_verified = $2, disabled = $3 WHERE id = $1`, u.ID, verified, disabled)
		return u
	}
	owner := user("nr-owner", "user", true, false)
	unverified := user("nr-unverified", "user", false, false)
	gone := user("nr-disabled", "user", true, true)
	platform := user("nr-platform", "owner", true, false)
	silentPlatform := user("nr-platform-off", "owner", true, true)
	seed := func(name string, ownerID any) {
		t.Helper()
		mustExec(t, `INSERT INTO servers (name, owner_id, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, $2, 100, 128, 1)`, name, ownerID)
	}
	seed("nr-mine-"+sfx, owner.ID)
	seed("nr-unverified-"+sfx, unverified.ID)
	seed("nr-disabled-"+sfx, gone.ID)
	seed("nr-free-"+sfx, nil)

	got, err := repo.ServerNoticeRecipients(ctx, "nr-mine-"+sfx)
	if err != nil || !slices.Equal(got, []string{owner.Email}) {
		t.Fatalf("owned server: %q, %v; want only its owner", got, err)
	}
	for _, name := range []string{"nr-unverified-" + sfx, "nr-disabled-" + sfx} {
		if got, err := repo.ServerNoticeRecipients(ctx, name); err != nil || len(got) != 0 {
			t.Fatalf("%s: %q, %v; want nobody, and no fallback to the platform owners", name, got, err)
		}
	}
	got, err = repo.ServerNoticeRecipients(ctx, "nr-free-"+sfx)
	if err != nil || !slices.Contains(got, platform.Email) || slices.Contains(got, silentPlatform.Email) || slices.Contains(got, owner.Email) {
		t.Fatalf("unclaimed server: %q, %v; want the active Owner-role accounts", got, err)
	}
}
