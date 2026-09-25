//go:build pgint

package pgint

import (
	"context"
	"errors"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
)

// ---- login codes and ceremonies that coexist (migration 0029) --------------------

func liveLoginCodes(t *testing.T, userID, purpose string, now time.Time) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM email_otps
		WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL AND expires_at > $3`,
		userID, purpose, now).Scan(&n); err != nil {
		t.Fatalf("count live codes: %v", err)
	}
	return n
}

// A login start keeps the three newest live codes: a fourth drops the oldest, and
// redeeming any live one spends its siblings too.
func TestAddLoginEmailOTPKeepsNewestThree(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "otp-keep")
	purpose := "login_email"
	addr := "keep-" + suffix(t) + "@example.net"
	t0 := mustNow().Truncate(time.Second)
	add := func(hash string, at time.Time) {
		t.Helper()
		if err := repo.AddLoginEmailOTP(ctx, "keep-"+suffix(t), u.ID, addr, hash, purpose, at, at.Add(10*time.Minute)); err != nil {
			t.Fatalf("AddLoginEmailOTP(%s): %v", hash, err)
		}
	}
	add("h1", t0)
	add("h2", t0.Add(time.Minute))
	add("h3", t0.Add(2*time.Minute))
	if n := liveLoginCodes(t, u.ID, purpose, t0.Add(2*time.Minute)); n != 3 {
		t.Fatalf("live codes after three starts = %d, want 3", n)
	}
	add("h4", t0.Add(3*time.Minute))
	at := t0.Add(3 * time.Minute)
	if n := liveLoginCodes(t, u.ID, purpose, at); n != 3 {
		t.Fatalf("live codes after four starts = %d, want 3", n)
	}
	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h1", at); !errors.Is(err, api.ErrOTPInvalid) {
		t.Fatalf("oldest code after a fourth start = %v, want ErrOTPInvalid", err)
	}
	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h2", at); err != nil {
		t.Fatalf("second code = %v, want nil", err)
	}
	for _, h := range []string{"h3", "h4"} {
		if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, h, at); !errors.Is(err, api.ErrOTPInvalid) {
			t.Fatalf("sibling %s after a sign-in = %v, want ErrOTPInvalid", h, err)
		}
	}

	// Expired codes leave the allowance: a start after they lapse is the only live one.
	add("h5", t0.Add(4*time.Minute))
	add("h6", t0.Add(5*time.Minute))
	later := t0.Add(20 * time.Minute)
	add("h7", later)
	if n := liveLoginCodes(t, u.ID, purpose, later); n != 1 {
		t.Fatalf("live codes after the others expired = %d, want 1", n)
	}
	var unconsumed int
	if err := db.QueryRow(`SELECT count(*) FROM email_otps WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL`,
		u.ID, purpose).Scan(&unconsumed); err != nil {
		t.Fatalf("count unconsumed: %v", err)
	}
	if unconsumed != 1 {
		t.Fatalf("unconsumed rows = %d, want 1 (expired codes are deleted, not kept)", unconsumed)
	}
}

// A wrong guess with several live codes costs each open code one attempt and the
// account one failure; when every live code is spent the door answers ErrOTPLocked,
// and a newer code still signs in.
func TestConsumeLoginEmailOTPAcrossLiveCodes(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "otp-multi")
	purpose := "op_login"
	addr := "multi-" + suffix(t) + "@example.net"
	t0 := mustNow().Truncate(time.Second)
	add := func(hash string, at time.Time) {
		t.Helper()
		if err := repo.AddLoginEmailOTP(ctx, "multi-"+suffix(t), u.ID, addr, hash, purpose, at, at.Add(10*time.Minute)); err != nil {
			t.Fatalf("AddLoginEmailOTP(%s): %v", hash, err)
		}
	}
	add("h-a", t0)
	add("h-b", t0.Add(time.Minute))
	at := t0.Add(time.Minute)
	attempts := func() map[string]int {
		t.Helper()
		rows, err := db.Query(`SELECT code_hash, attempts FROM email_otps WHERE user_id = $1 AND purpose = $2`, u.ID, purpose)
		if err != nil {
			t.Fatalf("read attempts: %v", err)
		}
		defer rows.Close()
		got := map[string]int{}
		for rows.Next() {
			var h string
			var n int
			if err := rows.Scan(&h, &n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[h] = n
		}
		return got
	}

	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-wrong", at); !errors.Is(err, api.ErrOTPInvalid) {
		t.Fatalf("wrong guess = %v, want ErrOTPInvalid", err)
	}
	if got := attempts(); got["h-a"] != 1 || got["h-b"] != 1 {
		t.Fatalf("attempts after one wrong guess = %v, want h-a:1 h-b:1", got)
	}
	for i := 2; i <= 5; i++ {
		if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-wrong", at); !errors.Is(err, api.ErrOTPInvalid) {
			t.Fatalf("wrong guess %d = %v, want ErrOTPInvalid", i, err)
		}
	}
	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-a", at); !errors.Is(err, api.ErrOTPLocked) {
		t.Fatalf("right code once every live code is spent = %v, want ErrOTPLocked", err)
	}
	var failures int
	if err := db.QueryRow(`SELECT failures FROM otp_failure_windows WHERE user_id = $1 AND purpose = $2`,
		u.ID, purpose).Scan(&failures); err != nil {
		t.Fatalf("read budget row: %v", err)
	}
	if failures != 5 {
		t.Fatalf("account failures = %d, want 5 (one per wrong guess, not one per code)", failures)
	}

	add("h-c", t0.Add(2*time.Minute))
	at = t0.Add(2 * time.Minute)
	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-b", at); !errors.Is(err, api.ErrOTPInvalid) {
		t.Fatalf("spent code beside a fresh one = %v, want ErrOTPInvalid", err)
	}
	if got := attempts(); got["h-c"] != 1 || got["h-a"] != 5 || got["h-b"] != 5 {
		t.Fatalf("attempts after a guess of a spent code = %v, want h-c:1 and the spent codes left at 5", got)
	}
	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-c", at); err != nil {
		t.Fatalf("fresh code = %v, want nil", err)
	}
	if n := liveLoginCodes(t, u.ID, purpose, at); n != 0 {
		t.Fatalf("live codes after a sign-in = %d, want 0", n)
	}
}

// Email-first passkey ceremonies of one account coexist and are redeemed by the
// challenge the browser signed; one network holds at most 32 live ones.
func TestPasskeyLoginChallengesCoexist(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "pk-login")
	purpose := "passkey_login"
	source := "203.0.113." + suffix(t)
	now := mustNow()
	add := func(id, source, challenge, session string, expiresAt time.Time) error {
		return repo.AddPasskeyLoginChallenge(ctx, id+"-"+suffix(t), u.ID, purpose, source, challenge, []byte(session), now, expiresAt)
	}
	if err := add("c1", source, "Y2hhbGxlbmdlMQ", "s1", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("add c1: %v", err)
	}
	if err := add("c2", source, "Y2hhbGxlbmdlMg", "s2", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("add c2: %v", err)
	}
	if _, err := repo.ConsumePasskeyLoginChallenge(ctx, u.ID, purpose, "bm9ib2R5", now); !errors.Is(err, api.ErrPasskeyChallengeInvalid) {
		t.Fatalf("unissued challenge = %v, want ErrPasskeyChallengeInvalid", err)
	}
	if sd, err := repo.ConsumePasskeyLoginChallenge(ctx, u.ID, purpose, "Y2hhbGxlbmdlMQ", now); err != nil || string(sd) != "s1" {
		t.Fatalf("earlier ceremony = %q, %v; want s1 (a later begin must not cancel it)", sd, err)
	}
	if _, err := repo.ConsumePasskeyLoginChallenge(ctx, u.ID, purpose, "Y2hhbGxlbmdlMQ", now); !errors.Is(err, api.ErrPasskeyChallengeInvalid) {
		t.Fatalf("replay = %v, want ErrPasskeyChallengeInvalid", err)
	}
	if sd, err := repo.ConsumePasskeyLoginChallenge(ctx, u.ID, purpose, "Y2hhbGxlbmdlMg", now); err != nil || string(sd) != "s2" {
		t.Fatalf("later ceremony = %q, %v; want s2", sd, err)
	}
	// The same challenge under another purpose is a different door.
	if err := add("c3", source, "Y2hhbGxlbmdlMw", "s3", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("add c3: %v", err)
	}
	if _, err := repo.ConsumePasskeyLoginChallenge(ctx, u.ID, "passkey_register", "Y2hhbGxlbmdlMw", now); !errors.Is(err, api.ErrPasskeyChallengeInvalid) {
		t.Fatalf("other purpose = %v, want ErrPasskeyChallengeInvalid", err)
	}
	if _, err := repo.ConsumePasskeyLoginChallenge(ctx, u.ID, purpose, "Y2hhbGxlbmdlMw", now.Add(6*time.Minute)); !errors.Is(err, api.ErrPasskeyChallengeInvalid) {
		t.Fatalf("expired ceremony = %v, want ErrPasskeyChallengeInvalid", err)
	}

	// Other accounts' spent rows from the same source hold no slot: one expired, one
	// redeemed. Two accounts, since an account's own begin reaps its spent rows.
	other := newUser(t, "user", "pk-login-other")
	if err := repo.AddPasskeyLoginChallenge(ctx, "ox-"+suffix(t), other.ID, purpose, source, "ZXhwaXJlZA", []byte("s"), now, now.Add(-time.Second)); err != nil {
		t.Fatalf("other account's expired begin: %v", err)
	}
	third := newUser(t, "user", "pk-login-third")
	if err := repo.AddPasskeyLoginChallenge(ctx, "oc-"+suffix(t), third.ID, purpose, source, "cmVkZWVtZWQ", []byte("s"), now, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("third account's begin: %v", err)
	}
	if _, err := repo.ConsumePasskeyLoginChallenge(ctx, third.ID, purpose, "cmVkZWVtZWQ", now); err != nil {
		t.Fatalf("third account's finish: %v", err)
	}

	// Per-source bound: c3 is still live at now, so 31 more fill the allowance.
	for i := 0; i < 31; i++ {
		if err := add("cap", source, "Y2Fw", "s", now.Add(5*time.Minute)); err != nil {
			t.Fatalf("add %d from the source: %v", i+2, err)
		}
	}
	if err := add("over", source, "b3Zlcg", "s", now.Add(5*time.Minute)); !errors.Is(err, api.ErrTooManyPasskeyChallenges) {
		t.Fatalf("33rd live challenge from one source = %v, want ErrTooManyPasskeyChallenges", err)
	}
	if err := repo.AddPasskeyLoginChallenge(ctx, "x-"+suffix(t), other.ID, purpose, source, "eA", []byte("s"), now, now.Add(5*time.Minute)); !errors.Is(err, api.ErrTooManyPasskeyChallenges) {
		t.Fatalf("another account's begin from the full source = %v, want ErrTooManyPasskeyChallenges", err)
	}
	if err := add("elsewhere", "198.51.100."+suffix(t), "ZWxzZXdoZXJl", "s", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("begin from another source: %v", err)
	}
	// A redeemed ceremony frees its slot.
	if _, err := repo.ConsumePasskeyLoginChallenge(ctx, u.ID, purpose, "b3Zlcg", now); !errors.Is(err, api.ErrPasskeyChallengeInvalid) {
		t.Fatalf("the refused begin left a row: %v", err)
	}
	var capID string
	if err := db.QueryRow(`SELECT challenge FROM webauthn_challenges WHERE user_id = $1 AND source = $2 AND consumed_at IS NULL LIMIT 1`,
		u.ID, source).Scan(&capID); err != nil {
		t.Fatalf("pick a live challenge: %v", err)
	}
	if _, err := repo.ConsumePasskeyLoginChallenge(ctx, u.ID, purpose, capID, now); err != nil {
		t.Fatalf("redeem one from the full source: %v", err)
	}
	if err := add("after", source, "YWZ0ZXI", "s", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("begin after one was redeemed = %v, want nil", err)
	}
}

// The usernameless store holds each source to 32 live challenges and the whole
// table to 16384.
func TestDiscoverableChallengeBounds(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	source := "2001:db8:" + suffix(t)[:4] + "::/48"
	t.Cleanup(func() {
		db.Exec(`DELETE FROM webauthn_discoverable_challenges WHERE source LIKE 'pgint-bulk-%' OR source = $1`, source) //nolint:errcheck
	})
	var ids []string
	for i := 0; i < 32; i++ {
		id := "disc-" + suffix(t)
		if err := repo.CreateDiscoverableChallenge(ctx, id, source, []byte("s"), now, now.Add(5*time.Minute)); err != nil {
			t.Fatalf("create %d: %v", i+1, err)
		}
		ids = append(ids, id)
	}
	if err := repo.CreateDiscoverableChallenge(ctx, "disc-over-"+suffix(t), source, []byte("s"), now, now.Add(5*time.Minute)); !errors.Is(err, api.ErrTooManyPasskeyChallenges) {
		t.Fatalf("33rd from one source = %v, want ErrTooManyPasskeyChallenges", err)
	}
	if err := repo.CreateDiscoverableChallenge(ctx, "disc-else-"+suffix(t), "pgint-bulk-else", []byte("s"), now, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("another source: %v", err)
	}
	if _, err := repo.ConsumeDiscoverableChallenge(ctx, ids[0], now); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := repo.CreateDiscoverableChallenge(ctx, "disc-after-"+suffix(t), source, []byte("s"), now, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("after a redeem freed a slot = %v, want nil", err)
	}

	// Fill the table to its store-wide bound from many sources, none of them full.
	var live int
	if err := db.QueryRow(`SELECT count(*) FROM webauthn_discoverable_challenges WHERE consumed_at IS NULL AND expires_at > $1`, now).Scan(&live); err != nil {
		t.Fatalf("count: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO webauthn_discoverable_challenges (id, session_data, expires_at, source)
		SELECT 'bulk-' || $1 || '-' || i, '\x00'::bytea, $2, 'pgint-bulk-' || (i % 1000)
		FROM generate_series(1, $3) AS i`, suffix(t), now.Add(5*time.Minute), 16384-live); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	if err := repo.CreateDiscoverableChallenge(ctx, "disc-full-"+suffix(t), "pgint-bulk-fresh", []byte("s"), now, now.Add(5*time.Minute)); !errors.Is(err, api.ErrTooManyPasskeyChallenges) {
		t.Fatalf("begin with the table full = %v, want ErrTooManyPasskeyChallenges", err)
	}
}
