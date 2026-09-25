//go:build pgint

package pgint

import (
	"context"
	"errors"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
)

func newSession(t *testing.T, userID, tag string, expires time.Time) string {
	t.Helper()
	hash := tag + "-" + suffix(t)
	if err := repo.CreateSession(context.Background(), api.NewSession{
		TokenHash: hash, UserID: userID, ExpiresAt: expires,
		UserAgent: "agent " + tag, ClientIP: "192.0.2.1",
	}); err != nil {
		t.Fatalf("CreateSession(%s): %v", tag, err)
	}
	return hash
}

func setLastSeen(t *testing.T, hash string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`UPDATE sessions SET last_seen_at = $2 WHERE token_hash = $1`, hash, at); err != nil {
		t.Fatal(err)
	}
}

// sameMicro compares at timestamptz's microsecond precision.
func sameMicro(a, b time.Time) bool {
	d := a.Sub(b)
	return d > -time.Microsecond && d < time.Microsecond
}

func listedHashes(t *testing.T, userID string, now time.Time) []string {
	t.Helper()
	ss, err := repo.ListUserSessions(context.Background(), userID, now)
	if err != nil {
		t.Fatalf("ListUserSessions: %v", err)
	}
	var out []string
	for _, s := range ss {
		out = append(out, s.TokenHash)
	}
	return out
}

// A staff session dies after 30 idle minutes and a player's does not; both
// SessionUser and the session list agree on which are live.
func TestStaffSessionsIdleOut(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	admin := newUser(t, "admin", "idle-admin")
	player := newUser(t, "user", "idle-player")
	fresh := newSession(t, admin.ID, "fresh", now.Add(time.Hour))
	stale := newSession(t, admin.ID, "stale", now.Add(time.Hour))
	idlePlayer := newSession(t, player.ID, "player", now.Add(time.Hour))
	setLastSeen(t, fresh, now.Add(-29*time.Minute))
	setLastSeen(t, stale, now.Add(-31*time.Minute))
	setLastSeen(t, idlePlayer, now.Add(-5*time.Hour))

	su, err := repo.SessionUser(ctx, fresh, now)
	if err != nil {
		t.Fatalf("staff session idle 29m: %v", err)
	}
	if want := now.Add(-29 * time.Minute); !sameMicro(su.LastSeenAt, want) {
		t.Errorf("LastSeenAt = %v, want %v", su.LastSeenAt, want)
	}
	if _, err := repo.SessionUser(ctx, stale, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("staff session idle 31m = %v, want ErrNotFound", err)
	}
	if _, err := repo.SessionUser(ctx, idlePlayer, now); err != nil {
		t.Fatalf("player session idle 5h: %v", err)
	}
	if got := listedHashes(t, admin.ID, now); len(got) != 1 || got[0] != fresh {
		t.Fatalf("admin's listed sessions = %v, want only %s", got, fresh)
	}
	if got := listedHashes(t, player.ID, now); len(got) != 1 || got[0] != idlePlayer {
		t.Fatalf("player's listed sessions = %v, want %s", got, idlePlayer)
	}

	// Activity keeps a staff session alive: a touch at 29m restarts the clock.
	if err := repo.TouchSession(ctx, fresh, now); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	if _, err := repo.SessionUser(ctx, fresh, now.Add(29*time.Minute)); err != nil {
		t.Fatalf("staff session 29m after a touch: %v", err)
	}
}

func TestTouchSessionNeverMovesBack(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	u := newUser(t, "user", "touch")
	hash := newSession(t, u.ID, "touch", now.Add(time.Hour))

	later := now.Add(10 * time.Minute)
	if err := repo.TouchSession(ctx, hash, later); err != nil {
		t.Fatal(err)
	}
	if err := repo.TouchSession(ctx, hash, now); err != nil {
		t.Fatal(err)
	}
	var seen time.Time
	if err := db.QueryRow(`SELECT last_seen_at FROM sessions WHERE token_hash = $1`, hash).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if !sameMicro(seen, later) {
		t.Fatalf("last_seen_at = %v after touches at +10m then +0, want %v", seen, later)
	}
	if err := repo.TouchSession(ctx, "no-such-"+suffix(t), now); err != nil {
		t.Fatalf("touching an absent session: %v", err)
	}
}

func TestSessionsRecordTheirDevice(t *testing.T) {
	now := mustNow()
	u := newUser(t, "user", "device")
	hash := newSession(t, u.ID, "device", now.Add(time.Hour))
	ss, err := repo.ListUserSessions(context.Background(), u.ID, now)
	if err != nil || len(ss) != 1 {
		t.Fatalf("ListUserSessions = %v, %v", ss, err)
	}
	s := ss[0]
	if s.TokenHash != hash || s.UserAgent != "agent device" || s.ClientIP != "192.0.2.1" {
		t.Fatalf("listed session = %+v", s)
	}
	if s.LastSeenAt.Before(s.CreatedAt) || s.LastSeenAt.IsZero() {
		t.Fatalf("a new session counts as seen at creation: created %v, last seen %v", s.CreatedAt, s.LastSeenAt)
	}
}

func TestRevokeUserSessionOnlyEndsThatUsersSession(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	steve := newUser(t, "user", "rv-steve")
	alex := newUser(t, "user", "rv-alex")
	alexs := newSession(t, alex.ID, "alex", now.Add(time.Hour))
	expired := newSession(t, alex.ID, "expired", now.Add(-time.Minute))

	if err := repo.RevokeUserSession(ctx, steve.ID, alexs); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("revoke alex's session as steve = %v, want ErrNotFound", err)
	}
	if _, err := repo.SessionUser(ctx, alexs, now); err != nil {
		t.Fatalf("alex's session after steve's attempt: %v", err)
	}
	if err := repo.RevokeUserSession(ctx, alex.ID, alexs); err != nil {
		t.Fatalf("revoke alex's session as alex: %v", err)
	}
	if _, err := repo.SessionUser(ctx, alexs, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("revoked session still resolves: %v", err)
	}
	if err := repo.RevokeUserSession(ctx, alex.ID, alexs); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("revoking it again = %v, want ErrNotFound", err)
	}
	if err := repo.RevokeUserSession(ctx, alex.ID, expired); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("revoking an expired session = %v, want ErrNotFound", err)
	}
}

func TestRevokeOtherUserSessionsKeepsOne(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	steve := newUser(t, "user", "ro-steve")
	alex := newUser(t, "user", "ro-alex")
	keep := newSession(t, steve.ID, "keep", now.Add(time.Hour))
	a := newSession(t, steve.ID, "a", now.Add(time.Hour))
	b := newSession(t, steve.ID, "b", now.Add(time.Hour))
	newSession(t, steve.ID, "gone", now.Add(-time.Minute))
	alexs := newSession(t, alex.ID, "alex", now.Add(time.Hour))

	n, err := repo.RevokeOtherUserSessions(ctx, steve.ID, keep)
	if err != nil || n != 2 {
		t.Fatalf("RevokeOtherUserSessions = %d, %v; want the 2 unexpired others", n, err)
	}
	if got := listedHashes(t, steve.ID, now); len(got) != 1 || got[0] != keep {
		t.Fatalf("steve's live sessions = %v, want only %s (a=%s b=%s ended)", got, keep, a, b)
	}
	if _, err := repo.SessionUser(ctx, alexs, now); err != nil {
		t.Fatalf("alex's session after steve's revoke-others: %v", err)
	}
	if n, err := repo.RevokeOtherUserSessions(ctx, steve.ID, ""); err != nil || n != 1 {
		t.Fatalf("revoke-others keeping nothing = %d, %v; want 1", n, err)
	}
}

// reauth_at: a proven sign-in stores the proof, a bind-code sign-in stores
// none, SessionUser reads it back, and a reauth marks only a live session.
func TestSessionReauthProof(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	u := newUser(t, "user", "reauth")

	unproven := newSession(t, u.ID, "unproven", now.Add(time.Hour))
	su, err := repo.SessionUser(ctx, unproven, now)
	if err != nil {
		t.Fatalf("SessionUser: %v", err)
	}
	if !su.ReauthAt.IsZero() {
		t.Fatalf("ReauthAt = %v on a session with no proof, want zero", su.ReauthAt)
	}

	proven := "proven-" + suffix(t)
	signedIn := now.Add(-time.Minute)
	if err := repo.CreateSession(ctx, api.NewSession{
		TokenHash: proven, UserID: u.ID, ExpiresAt: now.Add(time.Hour), ReauthAt: signedIn,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if su, err = repo.SessionUser(ctx, proven, now); err != nil || !sameMicro(su.ReauthAt, signedIn) {
		t.Fatalf("SessionUser = %+v, %v; want ReauthAt %v", su, err, signedIn)
	}

	if err := repo.MarkSessionReauth(ctx, unproven, now); err != nil {
		t.Fatalf("MarkSessionReauth: %v", err)
	}
	if su, err = repo.SessionUser(ctx, unproven, now); err != nil || !sameMicro(su.ReauthAt, now) {
		t.Fatalf("after a mark SessionUser = %+v, %v; want ReauthAt %v", su, err, now)
	}
	// The other session keeps its own proof.
	if su, _ = repo.SessionUser(ctx, proven, now); !sameMicro(su.ReauthAt, signedIn) {
		t.Fatalf("marking one session moved another's proof to %v", su.ReauthAt)
	}

	if err := repo.RevokeSession(ctx, proven); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkSessionReauth(ctx, proven, now.Add(time.Minute)); err != nil {
		t.Fatalf("marking a revoked session: %v", err)
	}
	var stored time.Time
	if err := db.QueryRow(`SELECT reauth_at FROM sessions WHERE token_hash = $1`, proven).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !sameMicro(stored, signedIn) {
		t.Fatalf("a revoked session's reauth_at moved to %v", stored)
	}
	if err := repo.MarkSessionReauth(ctx, "no-such-"+suffix(t), now); err != nil {
		t.Fatalf("marking an absent session: %v", err)
	}
}
