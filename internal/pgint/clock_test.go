//go:build pgint

package pgint

import (
	"context"
	"errors"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
)

// A method handed the API clock writes only that clock (PGRepo's doc). Each case
// runs the API clock an hour behind the database's, so a time taken from now()
// instead lands an hour away from the one the method was given.

func skewedClock() time.Time { return mustNow().Add(-time.Hour).Truncate(time.Microsecond) }

func stampAt(t *testing.T, q string, args ...any) time.Time {
	t.Helper()
	var at time.Time
	if err := db.QueryRow(q, args...).Scan(&at); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return at
}

func wantStamp(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !sameMicro(got, want) {
		t.Errorf("%s = %v, want the API clock's %v", what, got, want)
	}
}

// A session is created and last seen at its sign-in on the API clock, which is
// also the clock the staff idle cutoff reads.
func TestSessionTimesComeFromTheSignIn(t *testing.T) {
	ctx := context.Background()
	now := skewedClock()
	create := func(userID, tag string, signedIn time.Time) string {
		t.Helper()
		hash := tag + "-" + suffix(t)
		if err := repo.CreateSession(ctx, api.NewSession{
			TokenHash: hash, UserID: userID, CreatedAt: signedIn, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatalf("CreateSession(%s): %v", tag, err)
		}
		return hash
	}

	admin := newUser(t, "admin", "clk-admin")
	stale := create(admin.ID, "clk-stale", now.Add(-31*time.Minute))
	fresh := create(admin.ID, "clk-fresh", now.Add(-29*time.Minute))
	if _, err := repo.SessionUser(ctx, stale, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("staff session signed in 31m ago = %v, want ErrNotFound (idled out)", err)
	}
	if _, err := repo.SessionUser(ctx, fresh, now); err != nil {
		t.Fatalf("staff session signed in 29m ago: %v", err)
	}

	player := newUser(t, "user", "clk-player")
	signedIn := now.Add(-2 * time.Hour)
	hash := create(player.ID, "clk-player", signedIn)
	ss, err := repo.ListUserSessions(ctx, player.ID, now)
	if err != nil || len(ss) != 1 || ss[0].TokenHash != hash {
		t.Fatalf("ListUserSessions = %+v, %v; want only %s", ss, err, hash)
	}
	wantStamp(t, "created_at", ss[0].CreatedAt, signedIn)
	wantStamp(t, "last_seen_at", ss[0].LastSeenAt, signedIn)
}

// Every door that writes a link stamps it, and whatever else its transaction
// creates, at the time it was given.
func TestLinkWritesStampTheAPIClock(t *testing.T) {
	ctx := context.Background()
	now := skewedClock()
	code := func(mc string) string {
		t.Helper()
		c := "clk-" + suffix(t)
		if err := repo.CreateLinkCode(ctx, c, mc, "mojang", now.Add(10*time.Minute)); err != nil {
			t.Fatalf("CreateLinkCode: %v", err)
		}
		return c
	}
	linkedAt := func(mc string) time.Time {
		t.Helper()
		return stampAt(t, `SELECT verified_at FROM account_links WHERE mc_uuid = $1`, mc)
	}
	createdAt := func(userID string) time.Time {
		t.Helper()
		return stampAt(t, `SELECT created_at FROM users WHERE id = $1`, userID)
	}

	t.Run("verify", func(t *testing.T) {
		u := newUser(t, "user", "clk-verify")
		mc := testUUID(t)
		if _, _, err := repo.VerifyLinkCode(ctx, u.ID, code(mc), now); err != nil {
			t.Fatalf("VerifyLinkCode: %v", err)
		}
		wantStamp(t, "verified_at", linkedAt(mc), now)
	})

	t.Run("takeover of a retired link", func(t *testing.T) {
		retired := newUser(t, "user", "clk-retired")
		mc := testUUID(t)
		if err := repo.LinkAccount(ctx, retired.ID, mc, "mojang"); err != nil {
			t.Fatalf("LinkAccount: %v", err)
		}
		mustExec(t, `UPDATE users SET disabled = true, deleted_at = now() WHERE id = $1`, retired.ID)
		taker := newUser(t, "user", "clk-taker")
		if _, _, err := repo.VerifyLinkCode(ctx, taker.ID, code(mc), now); err != nil {
			t.Fatalf("takeover VerifyLinkCode: %v", err)
		}
		wantStamp(t, "verified_at", linkedAt(mc), now)
	})

	t.Run("player bind", func(t *testing.T) {
		mc := testUUID(t)
		id := "usr-clk-player-" + suffix(t)
		if got, _, _, err := repo.RedeemPlayerBindCode(ctx, id, code(mc), now); err != nil || got != id {
			t.Fatalf("RedeemPlayerBindCode = %q, %v; want %s", got, err, id)
		}
		wantStamp(t, "user created_at", createdAt(id), now)
		wantStamp(t, "verified_at", linkedAt(mc), now)
	})

	t.Run("owner setup", func(t *testing.T) {
		r, setupDB := setupRepository(t)
		id := "usr-clk-owner-" + suffix(t)
		tok := "tok-clk-" + suffix(t)
		if got, _, err := r.CompleteOwnerSetup(ctx, id, now, tok, now.Add(time.Hour)); err != nil || got != id {
			t.Fatalf("setup = %q, %v", got, err)
		}
		for _, query := range []string{
			`SELECT created_at FROM users WHERE id = '` + id + `'`,
			`SELECT created_at FROM setup_tokens WHERE token_hash = '` + tok + `'`,
			`SELECT updated_at FROM platform_settings WHERE key = '` + api.LocalAuthEnabledKey + `'`,
		} {
			var stamp time.Time
			if err := setupDB.QueryRow(query).Scan(&stamp); err != nil {
				t.Fatal(err)
			}
			wantStamp(t, "owner setup clock", stamp, now)
		}
	})
}

// The redeem moves the servers, retires the source and ends its sessions at the
// instant it records the migration redeemed.
func TestMigrationRedeemStampsTheAPIClock(t *testing.T) {
	ctx := context.Background()
	now := skewedClock()
	src, dst := newUser(t, "user", "clk-src"), newUser(t, "user", "clk-dst")
	server := "clk-srv-" + suffix(t)
	seedOwnedServer(t, server, src.ID, false)
	sess := newSession(t, src.ID, "clk-src", now.Add(time.Hour))
	hash := "h-clk-" + suffix(t)
	startToCode(t, src.ID, dst.ID, hash, now, now.Add(10*time.Minute))

	if _, moved, err := repo.RedeemMigration(ctx, dst.ID, hash, now); err != nil || len(moved) != 1 {
		t.Fatalf("RedeemMigration = %v, %v; want [%s]", moved, err, server)
	}
	wantStamp(t, "redeemed_at",
		stampAt(t, `SELECT redeemed_at FROM account_migrations WHERE source_user_id = $1 AND state = 'redeemed'`, src.ID), now)
	wantStamp(t, "server claimed_at", stampAt(t, `SELECT claimed_at FROM servers WHERE name = $1`, server), now)
	wantStamp(t, "source deleted_at", stampAt(t, `SELECT deleted_at FROM users WHERE id = $1`, src.ID), now)
	wantStamp(t, "source session revoked_at", stampAt(t, `SELECT revoked_at FROM sessions WHERE token_hash = $1`, sess), now)
}
