//go:build pgint

package pgint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/reaper"
)

// The wake allowlist: the owner reads it with each player's live account name, a
// revoked entry stops both wake checks and stays revoked through the player's next
// join, and every change of owner (claim, reaper release, account deletion) empties
// the list of that server alone. The list used to only grow: nothing could read or
// revoke it, and a reclaimed server handed the old owner's players to the new one.
func TestWakeAllowlistLifecycle(t *testing.T) {
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	seed := func(prefix string) string {
		t.Helper()
		name := prefix + "-" + suffix(t)
		if err := repo.SeedServer(ctx, name, name+"-s", 100, 128, 1024); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return name
	}
	join := func(name, id string) {
		t.Helper()
		if err := repo.RecordJoin(ctx, name, id); err != nil {
			t.Fatalf("RecordJoin(%s): %v", name, err)
		}
	}
	friend := newUser(t, "user", "alfriend")
	linked, stray := testUUID(t), testUUID(t)
	exec(`INSERT INTO account_links (user_id, mc_uuid) VALUES ($1, $2)`, friend.ID, linked)
	names := strings.NewReplacer(linked, "linked", stray, "stray", friend.Username, "friend")
	list := func(name string) string {
		t.Helper()
		es, err := repo.ServerAllowlist(ctx, name)
		if err != nil {
			t.Fatalf("ServerAllowlist(%s): %v", name, err)
		}
		parts := []string{}
		for _, e := range es {
			if time.Since(e.AddedAt) > 2*time.Hour {
				t.Fatalf("added_at %v is not the join time", e.AddedAt)
			}
			parts = append(parts, fmt.Sprintf("%s:%s:%v", e.MCUUID, e.Username, e.CanWake))
		}
		return names.Replace(strings.Join(parts, ","))
	}
	canWake := func(name string) string {
		t.Helper()
		byUUID, err := repo.UUIDInAllowlist(ctx, name, linked)
		if err != nil {
			t.Fatalf("UUIDInAllowlist: %v", err)
		}
		byUser, err := repo.UserInAllowlist(ctx, name, friend.ID)
		if err != nil {
			t.Fatalf("UserInAllowlist: %v", err)
		}
		return fmt.Sprintf("uuid=%v user=%v", byUUID, byUser)
	}

	name, bystander := seed("al"), seed("alb")
	join(name, stray)
	exec(`UPDATE server_allowlist SET added_at = now() - interval '1 hour' WHERE server_name = $1 AND mc_uuid = $2`, name, stray)
	join(name, linked)
	if got, want := list(name), "linked:friend:true,stray::true"; got != want {
		t.Fatalf("list = %s, want %s", got, want)
	}
	if got := canWake(name); got != "uuid=true user=true" {
		t.Fatalf("before revoking: %s", got)
	}

	// Revoking stops both checks, a repeat keeps the first revoked_at, and the
	// player's next join leaves the entry revoked.
	if err := repo.SetAllowlistWake(ctx, name, linked, false); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	revokedAt := func() time.Time {
		t.Helper()
		var at time.Time
		if err := db.QueryRowContext(ctx, `SELECT revoked_at FROM server_allowlist WHERE server_name = $1 AND mc_uuid = $2`,
			name, linked).Scan(&at); err != nil {
			t.Fatalf("read revoked_at: %v", err)
		}
		return at
	}
	first := revokedAt()
	time.Sleep(10 * time.Millisecond)
	if err := repo.SetAllowlistWake(ctx, name, linked, false); err != nil {
		t.Fatalf("revoke again: %v", err)
	}
	if again := revokedAt(); !again.Equal(first) {
		t.Fatalf("a repeated revoke moved revoked_at from %v to %v", first, again)
	}
	join(name, linked)
	if got := canWake(name); got != "uuid=false user=false" {
		t.Fatalf("after revoking and rejoining: %s", got)
	}
	if got, want := list(name), "linked:friend:false,stray::true"; got != want {
		t.Fatalf("list after revoking = %s, want %s", got, want)
	}
	if err := repo.SetAllowlistWake(ctx, name, linked, true); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := canWake(name); got != "uuid=true user=true" {
		t.Fatalf("after restoring: %s", got)
	}
	for _, c := range []struct{ server, id string }{{name, testUUID(t)}, {"alnone-" + suffix(t), linked}, {bystander, linked}} {
		if err := repo.SetAllowlistWake(ctx, c.server, c.id, false); !errors.Is(err, api.ErrNotFound) {
			t.Fatalf("SetAllowlistWake(%s) off the list = %v, want ErrNotFound", c.server, err)
		}
	}

	// An account that is gone no longer names its entry.
	exec(`UPDATE users SET deleted_at = now() WHERE id = $1`, friend.ID)
	if got, want := list(name), "linked::true,stray::true"; got != want {
		t.Fatalf("list after the account closed = %s, want %s", got, want)
	}

	// Each change of owner empties the list of the server that changed hands and
	// no other: the bystander, owned by someone else, keeps its entry throughout.
	owner, keeper := newUser(t, "user", "alowner"), newUser(t, "user", "alkeeper")
	if ok, err := repo.ClaimServer(ctx, bystander, keeper.ID); err != nil || !ok {
		t.Fatalf("claim the bystander = %v, %v", ok, err)
	}
	join(bystander, linked)
	claim := func() {
		t.Helper()
		if ok, err := repo.ClaimServer(ctx, name, owner.ID); err != nil || !ok {
			t.Fatalf("claim = %v, %v", ok, err)
		}
	}
	for _, step := range []struct {
		label  string
		before func()
		run    func()
	}{
		{"claim", func() {}, claim},
		{"reaper release", func() { join(name, linked) }, func() {
			if err := reaper.NewPGStore(db).ReleaseWorld(ctx, name, time.Now()); err != nil {
				t.Fatalf("ReleaseWorld: %v", err)
			}
		}},
		{"account deletion", func() { claim(); join(name, linked) }, func() {
			if err := repo.DeleteUser(ctx, owner.ID, "pgint"); err != nil {
				t.Fatalf("DeleteUser: %v", err)
			}
		}},
	} {
		step.before()
		if list(name) == "" {
			t.Fatalf("%s: the list is empty before the step", step.label)
		}
		step.run()
		if got := list(name); got != "" {
			t.Fatalf("after %s: list = %s, want empty", step.label, got)
		}
		if got, want := list(bystander), "linked::true"; got != want {
			t.Fatalf("after %s: bystander list = %s, want %s", step.label, got, want)
		}
	}
}
