//go:build pgint

package pgint

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
)

// ---- account migration (migration 0015) -------------------------------------------

func seedOwnedServer(t *testing.T, name, ownerID string, deleted bool) {
	t.Helper()
	var deletedAt any
	if deleted {
		deletedAt = mustNow()
	}
	mustExec(t, `INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb, owner_id, deleted_at)
		VALUES ($1, 100, 128, 1, $2, $3)`, name, ownerID, deletedAt)
}

func serverOwner(t *testing.T, name string) string {
	t.Helper()
	var owner string
	if err := db.QueryRow(`SELECT COALESCE(owner_id, '') FROM servers WHERE name = $1`, name).Scan(&owner); err != nil {
		t.Fatalf("owner of %s: %v", name, err)
	}
	return owner
}

func liveMigrations(t *testing.T, sourceID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM account_migrations WHERE source_user_id = $1 AND state <> 'redeemed'`,
		sourceID).Scan(&n); err != nil {
		t.Fatalf("count live migrations: %v", err)
	}
	return n
}

// startToCode drives a source from nothing to an issued code for target.
func startToCode(t *testing.T, sourceID, targetID, codeHash string, now, expiresAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := repo.StartMigration(ctx, "mig-"+suffix(t), sourceID, now); err != nil {
		t.Fatalf("StartMigration: %v", err)
	}
	if err := repo.ConfirmMigration(ctx, sourceID, "passkey", "sess-src", now); err != nil {
		t.Fatalf("ConfirmMigration: %v", err)
	}
	if err := repo.IssueMigrationCode(ctx, sourceID, targetID, "sess-src", codeHash, now, expiresAt); err != nil {
		t.Fatalf("IssueMigrationCode: %v", err)
	}
}

// Each step advances from exactly one state, a restart kills the outstanding code,
// and the redeem moves the source's live servers to the named target, retires the
// source and ends its sessions, in one go and once.
func TestMigrationStateMachine(t *testing.T) {
	ctx := context.Background()
	src := newUser(t, "user", "mig-src")
	dst := newUser(t, "user", "mig-dst")
	bystander := newUser(t, "user", "mig-by")
	sfx := suffix(t)
	liveA, liveB, gone, theirs := "ma-"+sfx, "mb-"+sfx, "mgone-"+sfx, "mtheirs-"+sfx
	seedOwnedServer(t, liveA, src.ID, false)
	seedOwnedServer(t, liveB, src.ID, false)
	seedOwnedServer(t, gone, src.ID, true)
	seedOwnedServer(t, theirs, bystander.ID, false)
	t0 := mustNow().Truncate(time.Second)
	srcSession := newSession(t, src.ID, "mig-src", t0.Add(time.Hour))
	dstSession := newSession(t, dst.ID, "mig-dst", t0.Add(time.Hour))

	if _, err := repo.MigrationForSource(ctx, src.ID); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("status before start = %v, want ErrNotFound", err)
	}
	if err := repo.ConfirmMigration(ctx, src.ID, "passkey", "sess-src", t0); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("confirm before start = %v, want ErrConflict", err)
	}
	if err := repo.StartMigration(ctx, "mig1-"+sfx, src.ID, t0); err != nil {
		t.Fatalf("StartMigration: %v", err)
	}
	m, err := repo.MigrationForSource(ctx, src.ID)
	if err != nil || m.ID != "mig1-"+sfx || m.State != "initiated" || m.TargetUserID != "" || m.ConfirmedAt != nil {
		t.Fatalf("after start = %+v, %v; want mig1 initiated, no target, unconfirmed", m, err)
	}
	if err := repo.IssueMigrationCode(ctx, src.ID, dst.ID, "sess-src", "h-skip", t0, t0.Add(10*time.Minute)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("issue before confirm = %v, want ErrConflict", err)
	}
	if err := repo.ConfirmMigration(ctx, src.ID, "email_otp", "sess-src", t0); err != nil {
		t.Fatalf("ConfirmMigration: %v", err)
	}
	if err := repo.ConfirmMigration(ctx, src.ID, "passkey", "sess-src", t0.Add(time.Minute)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("second confirm = %v, want ErrConflict", err)
	}
	m, err = repo.MigrationForSource(ctx, src.ID)
	if err != nil || m.State != "confirmed" || m.ConfirmFactor != "email_otp" || m.ConfirmedAt == nil || !m.ConfirmedAt.Equal(t0) {
		t.Fatalf("after confirm = %+v, %v; want confirmed by email_otp at %v", m, err, t0)
	}
	if err := repo.IssueMigrationCode(ctx, src.ID, dst.ID, "sess-src", "h-first", t0, t0.Add(10*time.Minute)); err != nil {
		t.Fatalf("IssueMigrationCode: %v", err)
	}
	if err := repo.IssueMigrationCode(ctx, src.ID, bystander.ID, "sess-src", "h-again", t0, t0.Add(10*time.Minute)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("second issue = %v, want ErrConflict", err)
	}
	m, err = repo.MigrationForSource(ctx, src.ID)
	if err != nil || m.State != "code_issued" || m.TargetUserID != dst.ID || m.CodeExpiresAt == nil || !m.CodeExpiresAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("after issue = %+v, %v; want code_issued for the target, expiring at +10m", m, err)
	}

	// Restarting supersedes the whole attempt: the code it issued is dead.
	if err := repo.StartMigration(ctx, "mig2-"+sfx, src.ID, t0); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if m, err := repo.MigrationForSource(ctx, src.ID); err != nil || m.ID != "mig2-"+sfx || m.State != "initiated" {
		t.Fatalf("after restart = %+v, %v; want mig2 initiated", m, err)
	}
	if n := liveMigrations(t, src.ID); n != 1 {
		t.Fatalf("live migrations after restart = %d, want 1", n)
	}
	if _, _, err := repo.RedeemMigration(ctx, dst.ID, "h-first", t0); !errors.Is(err, api.ErrLinkCodeInvalid) {
		t.Fatalf("code from the superseded attempt = %v, want ErrLinkCodeInvalid", err)
	}

	if err := repo.ConfirmMigration(ctx, src.ID, "passkey", "sess-src", t0); err != nil {
		t.Fatalf("confirm the restart: %v", err)
	}
	if err := repo.IssueMigrationCode(ctx, src.ID, dst.ID, "sess-src", "h-second", t0, t0.Add(10*time.Minute)); err != nil {
		t.Fatalf("issue the restart: %v", err)
	}
	for _, bad := range []struct {
		what, target, hash string
		at                 time.Time
	}{
		{"another user with the code", bystander.ID, "h-second", t0},
		{"the target with a wrong code", dst.ID, "h-wrong", t0},
		{"the target at code expiry", dst.ID, "h-second", t0.Add(10 * time.Minute)},
		{"the source itself", src.ID, "h-second", t0},
	} {
		if _, _, err := repo.RedeemMigration(ctx, bad.target, bad.hash, bad.at); !errors.Is(err, api.ErrLinkCodeInvalid) {
			t.Fatalf("redeem by %s = %v, want ErrLinkCodeInvalid", bad.what, err)
		}
	}
	if owner := serverOwner(t, liveA); owner != src.ID {
		t.Fatalf("a refused redeem moved %s to %s", liveA, owner)
	}

	source, moved, err := repo.RedeemMigration(ctx, dst.ID, "h-second", t0.Add(10*time.Minute-time.Second))
	if err != nil {
		t.Fatalf("RedeemMigration: %v", err)
	}
	sort.Strings(moved)
	if source != src.ID || strings.Join(moved, ",") != liveA+","+liveB {
		t.Fatalf("redeem = %s, %v; want %s, [%s %s]", source, moved, src.ID, liveA, liveB)
	}
	for name, want := range map[string]string{liveA: dst.ID, liveB: dst.ID, gone: src.ID, theirs: bystander.ID} {
		if got := serverOwner(t, name); got != want {
			t.Fatalf("owner of %s = %s, want %s", name, got, want)
		}
	}
	var disabled, deleted bool
	if err := db.QueryRow(`SELECT disabled, deleted_at IS NOT NULL FROM users WHERE id = $1`, src.ID).Scan(&disabled, &deleted); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if !disabled || !deleted {
		t.Fatalf("source after redeem: disabled=%v deleted=%v, want both", disabled, deleted)
	}
	// The session is revoked outright, beyond failing through the dead account.
	var srcRevoked bool
	if err := db.QueryRow(`SELECT revoked_at IS NOT NULL FROM sessions WHERE token_hash = $1`, srcSession).Scan(&srcRevoked); err != nil {
		t.Fatalf("read source session: %v", err)
	}
	if !srcRevoked {
		t.Fatalf("source session after redeem is not revoked")
	}
	if _, err := repo.SessionUser(ctx, dstSession, t0); err != nil {
		t.Fatalf("target session after redeem: %v", err)
	}
	var state string
	var redeemedAt sql.NullTime
	if err := db.QueryRow(`SELECT state, redeemed_at FROM account_migrations WHERE id = $1`, "mig2-"+sfx).Scan(&state, &redeemedAt); err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if state != "redeemed" || !redeemedAt.Valid || !redeemedAt.Time.Equal(t0.Add(10*time.Minute-time.Second)) {
		t.Fatalf("migration after redeem: %s at %v, want redeemed at %v", state, redeemedAt, t0.Add(10*time.Minute-time.Second))
	}

	if _, _, err := repo.RedeemMigration(ctx, dst.ID, "h-second", t0); !errors.Is(err, api.ErrLinkCodeInvalid) {
		t.Fatalf("replayed redeem = %v, want ErrLinkCodeInvalid", err)
	}
	if _, err := repo.MigrationForSource(ctx, src.ID); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("status after redeem = %v, want ErrNotFound", err)
	}
	if err := repo.StartMigration(ctx, "mig3-"+sfx, src.ID, t0); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("retired source starting again = %v, want ErrNotFound", err)
	}
}

// The step-up lets only the session that gave it issue the code, and only for
// migrateConfirmWindow (10 minutes). Another session, or the same one later, starts over
// at the step-up; so does a code that expired unspent, which the new confirmation clears.
func TestMigrationConfirmBelongsToOneSessionBriefly(t *testing.T) {
	ctx := context.Background()
	src := newUser(t, "user", "migw-src")
	dst := newUser(t, "user", "migw-dst")
	t0 := mustNow().Truncate(time.Second)
	const window = 10 * time.Minute
	if err := repo.StartMigration(ctx, "migw-"+suffix(t), src.ID, t0); err != nil {
		t.Fatalf("StartMigration: %v", err)
	}
	if err := repo.ConfirmMigration(ctx, src.ID, "passkey", "sess-a", t0); err != nil {
		t.Fatalf("confirm on a: %v", err)
	}
	if m, err := repo.MigrationForSource(ctx, src.ID); err != nil || m.ConfirmSession != "sess-a" {
		t.Fatalf("after confirm on a = %+v, %v; want it recorded against sess-a", m, err)
	}
	for _, bad := range []struct {
		what, session string
		at            time.Time
	}{
		{"another session", "sess-b", t0.Add(time.Minute)},
		{"a caller with no session", "", t0.Add(time.Minute)},
		{"the same session once the window closed", "sess-a", t0.Add(window)},
	} {
		if err := repo.IssueMigrationCode(ctx, src.ID, dst.ID, bad.session, "h-"+bad.session, bad.at, bad.at.Add(10*time.Minute)); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("issue from %s = %v, want ErrConflict", bad.what, err)
		}
	}
	if err := repo.ConfirmMigration(ctx, src.ID, "passkey", "sess-a", t0.Add(window-time.Second)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("confirm again on a while its confirmation holds = %v, want ErrConflict", err)
	}

	// Another session proves a factor of its own and takes the confirmation over.
	t1 := t0.Add(time.Minute)
	if err := repo.ConfirmMigration(ctx, src.ID, "email_otp", "sess-b", t1); err != nil {
		t.Fatalf("confirm on b: %v", err)
	}
	if err := repo.IssueMigrationCode(ctx, src.ID, dst.ID, "sess-a", "h-a", t1, t1.Add(10*time.Minute)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("issue from a after b confirmed = %v, want ErrConflict", err)
	}
	issueAt := t1.Add(window - time.Second)
	if err := repo.IssueMigrationCode(ctx, src.ID, dst.ID, "sess-b", "h-b", issueAt, issueAt.Add(10*time.Minute)); err != nil {
		t.Fatalf("issue from b inside its window: %v", err)
	}

	// A live code stands until it expires; then a new step-up clears it.
	expiry := issueAt.Add(10 * time.Minute)
	if err := repo.ConfirmMigration(ctx, src.ID, "passkey", "sess-a", expiry.Add(-time.Second)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("confirm over a live code = %v, want ErrConflict", err)
	}
	if err := repo.ConfirmMigration(ctx, src.ID, "passkey", "sess-a", expiry); err != nil {
		t.Fatalf("confirm once the code expired: %v", err)
	}
	m, err := repo.MigrationForSource(ctx, src.ID)
	if err != nil || m.State != "confirmed" || m.ConfirmSession != "sess-a" || m.TargetUserID != "" || m.CodeExpiresAt != nil {
		t.Fatalf("after confirming over the expired code = %+v, %v; want confirmed on a, no target, no code", m, err)
	}
	var hash sql.NullString
	if err := db.QueryRow(`SELECT code_hash FROM account_migrations WHERE id = $1`, m.ID).Scan(&hash); err != nil || hash.Valid {
		t.Fatalf("code hash after the new confirmation = %v, %v; want NULL", hash, err)
	}

	// The same session's own confirmation lapses too, and a fresh one replaces it.
	if err := repo.ConfirmMigration(ctx, src.ID, "passkey", "sess-a", expiry.Add(window)); err != nil {
		t.Fatalf("confirm again on a after its window: %v", err)
	}
}

// Racing redeems of one code move the servers once; racing starts for one source
// all succeed and leave one live attempt.
func TestMigrationUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	for round := 0; round < 5; round++ {
		src := newUser(t, "user", "migr-src")
		dst := newUser(t, "user", "migr-dst")
		srv := "mr-" + suffix(t)
		seedOwnedServer(t, srv, src.ID, false)
		startToCode(t, src.ID, dst.ID, "h-race", now, now.Add(10*time.Minute))

		var wg sync.WaitGroup
		errs := make([]error, 4)
		moved := make([][]string, 4)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, moved[i], errs[i] = repo.RedeemMigration(ctx, dst.ID, "h-race", now)
			}(i)
		}
		wg.Wait()
		won, lost := 0, 0
		for i, err := range errs {
			switch {
			case err == nil && len(moved[i]) == 1 && moved[i][0] == srv:
				won++
			case errors.Is(err, api.ErrLinkCodeInvalid):
				lost++
			default:
				t.Fatalf("round %d: redeem %d = %v, %v", round, i, moved[i], err)
			}
		}
		if won != 1 || lost != 3 {
			t.Fatalf("round %d: racing redeems: %d won, %d refused; want 1, 3", round, won, lost)
		}
		if owner := serverOwner(t, srv); owner != dst.ID {
			t.Fatalf("round %d: server owner = %s, want the target", round, owner)
		}
	}

	src := newUser(t, "user", "migs-src")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = repo.StartMigration(ctx, "migs-"+suffix(t), src.ID, now)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racing start %d: %v", i, err)
		}
	}
	if n := liveMigrations(t, src.ID); n != 1 {
		t.Fatalf("live migrations after 8 racing starts = %d, want 1", n)
	}
}

// waitForLockWait polls until a statement starting with prefix is waiting on a lock.
func waitForLockWait(t *testing.T, prefix string) {
	t.Helper()
	waitForLockWaiters(t, prefix, 1)
}

// waitForLockWaiters polls until want statements starting with prefix wait on a lock.
func waitForLockWaiters(t *testing.T, prefix string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND ltrim(query) LIKE $1 || '%'`,
			prefix).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if n >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fewer than %d statements %q waited on a lock within 5s", want, prefix)
}

// A restart that reaches the source while its redeem is in flight must not leave the
// retired account a live attempt: the redeem holds the migration row, the restart's
// supersede waits on it, and by the time the restart writes, the source is gone.
func TestStartMigrationBehindARedeem(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	src := newUser(t, "user", "migb-src")
	dst := newUser(t, "user", "migb-dst")
	srv := "mbr-" + suffix(t)
	seedOwnedServer(t, srv, src.ID, false)
	startToCode(t, src.ID, dst.ID, "h-behind", now, now.Add(10*time.Minute))

	// Hold the source's server so the redeem stops after locking the migration row.
	hold, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer hold.Rollback() //nolint:errcheck // no-op after commit
	if _, err := hold.Exec(`SELECT 1 FROM servers WHERE name = $1 FOR UPDATE`, srv); err != nil {
		t.Fatalf("hold server: %v", err)
	}
	redeemed := make(chan error, 1)
	go func() {
		_, _, err := repo.RedeemMigration(ctx, dst.ID, "h-behind", now)
		redeemed <- err
	}()
	waitForLockWait(t, "UPDATE servers SET owner_id")
	started := make(chan error, 1)
	go func() { started <- repo.StartMigration(ctx, "migb-"+suffix(t), src.ID, now) }()
	waitForLockWait(t, "DELETE FROM account_migrations")
	if err := hold.Commit(); err != nil {
		t.Fatalf("release server: %v", err)
	}
	if err := <-redeemed; err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if err := <-started; !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("restart behind the redeem = %v, want ErrNotFound", err)
	}
	if n := liveMigrations(t, src.ID); n != 0 {
		t.Fatalf("live migrations for the retired source = %d, want 0", n)
	}
}

// untouched checks a refused redeem left the source as it was: its servers, the
// account and its session live, and the code still issued.
func untouched(t *testing.T, what, srcID, session string, servers ...string) {
	t.Helper()
	for _, s := range servers {
		if owner := serverOwner(t, s); owner != srcID {
			t.Fatalf("%s moved %s to %s", what, s, owner)
		}
	}
	var live, sessionLive bool
	if err := db.QueryRow(`SELECT NOT disabled AND deleted_at IS NULL FROM users WHERE id = $1`, srcID).Scan(&live); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if err := db.QueryRow(`SELECT revoked_at IS NULL FROM sessions WHERE token_hash = $1`, session).Scan(&sessionLive); err != nil {
		t.Fatalf("read source session: %v", err)
	}
	if !live || !sessionLive {
		t.Fatalf("%s: source live=%v, session live=%v; want both", what, live, sessionLive)
	}
	if m, err := repo.MigrationForSource(context.Background(), srcID); err != nil || m.State != "code_issued" {
		t.Fatalf("%s: migration = %+v, %v; want still code_issued", what, m, err)
	}
}

// A redeem moves the source's servers only when the target's four caps hold with all
// of them added in. Over any cap it changes nothing, and the same code redeems once
// the quota fits. Deleted servers count on neither side.
func TestMigrationRedeemWithinTargetQuota(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	src := newUser(t, "user", "migq-src")
	dst := newUser(t, "user", "migq-dst")
	sfx := suffix(t)
	a, b := "mqa-"+sfx, "mqb-"+sfx
	seedOwnedServer(t, a, src.ID, false)
	seedOwnedServer(t, b, src.ID, false)
	seedOwnedServer(t, "mqgone-"+sfx, src.ID, true)
	seedOwnedServer(t, "mqown-"+sfx, dst.ID, false)
	mustExec(t, `UPDATE servers SET cached_storage_mb = 400 WHERE name IN ($1, $2)`, a, b)
	mustExec(t, `UPDATE servers SET cached_storage_mb = 225 WHERE name = $1`, "mqown-"+sfx)
	seedOwnedServer(t, "mqowngone-"+sfx, dst.ID, true)
	session := newSession(t, src.ID, "migq-src", now.Add(time.Hour))
	startToCode(t, src.ID, dst.ID, "h-quota", now, now.Add(10*time.Minute))

	// With both servers the target would own 3 servers, 300 millicores, 384 MB of
	// memory and 1025 MB of storage. Each cap one short of its figure refuses on its
	// own; storage is capped in whole GB, and 1 GB is 1024 MB.
	n := func(v int) *int { return &v }
	for _, c := range []struct {
		name string
		q    api.QuotaInput
	}{
		{"servers", api.QuotaInput{MaxServers: n(2)}},
		{"cpu", api.QuotaInput{MaxCPUMilli: n(299)}},
		{"memory", api.QuotaInput{MaxMemoryMB: n(383)}},
		{"storage", api.QuotaInput{MaxStorageGB: n(1)}},
	} {
		if _, err := repo.SetQuotas(ctx, dst.ID, c.q, "pgint"); err != nil {
			t.Fatalf("SetQuotas(%s): %v", c.name, err)
		}
		if _, _, err := repo.RedeemMigration(ctx, dst.ID, "h-quota", now); !errors.Is(err, api.ErrQuotaExceeded) {
			t.Fatalf("redeem over the %s cap = %v, want ErrQuotaExceeded", c.name, err)
		}
		untouched(t, "a redeem over the "+c.name+" cap", src.ID, session, a, b)
	}

	// Every cap at its figure fits, storage at the next whole GB.
	if _, err := repo.SetQuotas(ctx, dst.ID, api.QuotaInput{MaxServers: n(3), MaxCPUMilli: n(300), MaxMemoryMB: n(384), MaxStorageGB: n(2)}, "pgint"); err != nil {
		t.Fatalf("SetQuotas(fits): %v", err)
	}
	_, moved, err := repo.RedeemMigration(ctx, dst.ID, "h-quota", now)
	sort.Strings(moved)
	if err != nil || strings.Join(moved, ",") != a+","+b {
		t.Fatalf("redeem once the quota fits = %v, %v; want [%s %s]", moved, err, a, b)
	}
	if o := serverOwner(t, a); o != dst.ID {
		t.Fatalf("owner of %s after the redeem = %s, want the target", a, o)
	}

	// A source that owns nothing brings nothing, so it retires even into a target
	// already over a cap.
	empty := newUser(t, "user", "migq-empty")
	startToCode(t, empty.ID, dst.ID, "h-empty", now, now.Add(10*time.Minute))
	if _, err := repo.SetQuotas(ctx, dst.ID, api.QuotaInput{MaxServers: n(0)}, "pgint"); err != nil {
		t.Fatalf("SetQuotas(0): %v", err)
	}
	if _, moved, err := repo.RedeemMigration(ctx, dst.ID, "h-empty", now); err != nil || len(moved) != 0 {
		t.Fatalf("redeem of a source with no servers = %v, %v; want nothing moved", moved, err)
	}
	var retired bool
	if err := db.QueryRow(`SELECT disabled AND deleted_at IS NOT NULL FROM users WHERE id = $1`, empty.ID).Scan(&retired); err != nil || !retired {
		t.Fatalf("empty source after its redeem: retired=%v, %v; want retired", retired, err)
	}
}

// holdLane takes a user's claim lane (ClaimServer's advisory lock) in a transaction the
// caller ends.
func holdLane(t *testing.T, userIDs ...string) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, id := range userIDs {
		if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext($1))`, id); err != nil {
			tx.Rollback() //nolint:errcheck // the test is failing anyway
			t.Fatalf("hold lane: %v", err)
		}
	}
	return tx
}

// A redeem decides under both accounts' claim lanes: a server either account claims
// while the redeem waits is counted against the target's quota. The target holds one
// server of a two-server cap and the source one, so either claim tips it over.
func TestMigrationRedeemWaitsForClaims(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	n := func(v int) *int { return &v }
	for _, lane := range []string{"target", "source"} {
		src := newUser(t, "user", "migw-src")
		dst := newUser(t, "user", "migw-dst")
		sfx := suffix(t)
		mine, claimed := "mwa-"+sfx, "mwc-"+sfx
		seedOwnedServer(t, mine, src.ID, false)
		seedOwnedServer(t, "mwown-"+sfx, dst.ID, false)
		mustExec(t, `INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, 100, 128, 1)`, claimed)
		if _, err := repo.SetQuotas(ctx, dst.ID, api.QuotaInput{MaxServers: n(2)}, "pgint"); err != nil {
			t.Fatalf("SetQuotas: %v", err)
		}
		session := newSession(t, src.ID, "migw-src", now.Add(time.Hour))
		startToCode(t, src.ID, dst.ID, "h-wait", now, now.Add(10*time.Minute))
		claimer := dst.ID
		if lane == "source" {
			claimer = src.ID
		}

		hold := holdLane(t, claimer)
		redeemed := make(chan error, 1)
		go func() {
			_, _, err := repo.RedeemMigration(ctx, dst.ID, "h-wait", now)
			redeemed <- err
		}()
		waitForLockWait(t, "SELECT pg_advisory_xact_lock")
		if _, err := hold.Exec(`UPDATE servers SET owner_id = $2 WHERE name = $1`, claimed, claimer); err != nil {
			t.Fatalf("%s claim: %v", lane, err)
		}
		if err := hold.Commit(); err != nil {
			t.Fatalf("%s claim commit: %v", lane, err)
		}
		if err := <-redeemed; !errors.Is(err, api.ErrQuotaExceeded) {
			t.Fatalf("redeem behind a %s claim = %v, want ErrQuotaExceeded", lane, err)
		}
		untouched(t, "a redeem behind a "+lane+" claim", src.ID, session, mine)
	}
}

// Two migrations crossing between one pair of accounts take their lanes in one order,
// so neither deadlocks the other: both wait behind a holder of both lanes and, once it
// lets go, both redeem.
func TestCrossingMigrationsDoNotDeadlock(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	x := newUser(t, "user", "migx-a")
	y := newUser(t, "user", "migx-b")
	sfx := suffix(t)
	seedOwnedServer(t, "mxa-"+sfx, x.ID, false)
	seedOwnedServer(t, "mxb-"+sfx, y.ID, false)
	startToCode(t, x.ID, y.ID, "h-x", now, now.Add(10*time.Minute))
	startToCode(t, y.ID, x.ID, "h-y", now, now.Add(10*time.Minute))

	hold := holdLane(t, x.ID, y.ID)
	errs := make(chan error, 2)
	go func() { _, _, err := repo.RedeemMigration(ctx, y.ID, "h-x", now); errs <- err }()
	go func() { _, _, err := repo.RedeemMigration(ctx, x.ID, "h-y", now); errs <- err }()
	waitForLockWaiters(t, "SELECT pg_advisory_xact_lock", 2)
	if err := hold.Commit(); err != nil {
		t.Fatalf("release lanes: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("crossing redeem: %v", err)
		}
	}
}

// ---- setup tokens (migration 0012) --------------------------------------------------

// A setup token redeems once, never at or after expiry, and racing redeems of one
// token yield one session. The session is written with the spend: the token's
// user owns it, and a redemption whose session cannot be stored leaves the token
// for the next try.
func TestRedeemSetupTokenContract(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "setup")
	t0 := mustNow().Truncate(time.Second)
	create := func(hash string) {
		t.Helper()
		if err := repo.CreateSetupToken(ctx, hash, u.ID, t0.Add(10*time.Minute)); err != nil {
			t.Fatalf("CreateSetupToken: %v", err)
		}
	}
	var mu sync.Mutex
	minted := 0
	redeem := func(hash string, at time.Time) (string, string, error) {
		mu.Lock()
		minted++
		s := api.NewSession{TokenHash: "st-sess-" + strconv.Itoa(minted) + "-" + suffix(t), UserID: "ignored", CreatedAt: at, ExpiresAt: t0.Add(time.Hour)}
		mu.Unlock()
		id, err := repo.RedeemSetupToken(ctx, hash, at, s)
		return id, s.TokenHash, err
	}
	sessionOf := func(hash string) string {
		t.Helper()
		su, err := repo.SessionUser(ctx, hash, t0)
		if errors.Is(err, api.ErrNotFound) {
			return ""
		}
		if err != nil {
			t.Fatalf("SessionUser: %v", err)
		}
		return su.ID
	}

	once := "st-once-" + suffix(t)
	create(once)
	got, sess, err := redeem(once, t0)
	if err != nil || got != u.ID {
		t.Fatalf("redeem = %q, %v; want %s", got, err, u.ID)
	}
	if owner := sessionOf(sess); owner != u.ID {
		t.Fatalf("redeemed session belongs to %q, want %s", owner, u.ID)
	}
	if _, sess, err := redeem(once, t0); !errors.Is(err, api.ErrNotFound) || sessionOf(sess) != "" {
		t.Fatalf("replay = %v (session stored: %v), want ErrNotFound and no session", err, sessionOf(sess) != "")
	}
	if _, _, err := redeem("st-never-"+suffix(t), t0); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown token = %v, want ErrNotFound", err)
	}

	late := "st-late-" + suffix(t)
	create(late)
	if _, _, err := redeem(late, t0.Add(10*time.Minute)); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("redeem at expiry = %v, want ErrNotFound", err)
	}
	if got, _, err := redeem(late, t0.Add(10*time.Minute-time.Second)); err != nil || got != u.ID {
		t.Fatalf("redeem a second before expiry = %q, %v; want %s (the refused try must not spend it)", got, err, u.ID)
	}

	// The session's hash is taken, so storing it fails: the token stays unspent.
	kept := "st-kept-" + suffix(t)
	create(kept)
	taken := newSession(t, u.ID, "st-taken", t0.Add(time.Hour))
	if _, err := repo.RedeemSetupToken(ctx, kept, t0, api.NewSession{TokenHash: taken, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}); err == nil || errors.Is(err, api.ErrNotFound) {
		t.Fatalf("redeem into a taken session hash = %v, want the insert failure", err)
	}
	if got, _, err := redeem(kept, t0); err != nil || got != u.ID {
		t.Fatalf("retry after the failed session = %q, %v; want %s (the token must survive)", got, err, u.ID)
	}

	raced := "st-race-" + suffix(t)
	create(raced)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = redeem(raced, t0)
		}(i)
	}
	wg.Wait()
	won, lost := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, api.ErrNotFound):
			lost++
		default:
			t.Fatalf("racing redeem %d: %v", i, err)
		}
	}
	if won != 1 || lost != 7 {
		t.Fatalf("racing redeems: %d won, %d refused; want 1, 7", won, lost)
	}
}

// ---- remediation: end every session, drop every passkey -----------------------------

func TestRevokeAllUserSessionsIsScoped(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	u := newUser(t, "user", "rall")
	other := newUser(t, "user", "rall-other")
	a := newSession(t, u.ID, "rall-a", now.Add(time.Hour))
	b := newSession(t, u.ID, "rall-b", now.Add(time.Hour))
	old := newSession(t, u.ID, "rall-old", now.Add(time.Hour))
	theirs := newSession(t, other.ID, "rall-theirs", now.Add(time.Hour))
	earlier := now.Add(-time.Hour).Truncate(time.Microsecond)
	mustExec(t, `UPDATE sessions SET revoked_at = $2 WHERE token_hash = $1`, old, earlier)

	if err := repo.RevokeAllUserSessions(ctx, u.ID); err != nil {
		t.Fatalf("RevokeAllUserSessions: %v", err)
	}
	for _, h := range []string{a, b} {
		if _, err := repo.SessionUser(ctx, h, now); !errors.Is(err, api.ErrNotFound) {
			t.Fatalf("session %s after revoke-all = %v, want ErrNotFound", h, err)
		}
	}
	if _, err := repo.SessionUser(ctx, theirs, now); err != nil {
		t.Fatalf("another account's session after revoke-all: %v", err)
	}
	var revokedAt time.Time
	if err := db.QueryRow(`SELECT revoked_at FROM sessions WHERE token_hash = $1`, old).Scan(&revokedAt); err != nil {
		t.Fatalf("read old session: %v", err)
	}
	if !sameMicro(revokedAt, earlier) {
		t.Fatalf("an already-revoked session's revoked_at moved from %v to %v", earlier, revokedAt)
	}
	if err := repo.RevokeAllUserSessions(ctx, newUser(t, "user", "rall-none").ID); err != nil {
		t.Fatalf("revoke-all with no sessions = %v, want nil", err)
	}
}

func TestDeleteAllPasskeyCredentialsIsScoped(t *testing.T) {
	ctx := context.Background()
	seed := func(userID string) {
		t.Helper()
		mustExec(t, `INSERT INTO webauthn_credentials (id, user_id, credential_id, public_key) VALUES ($1, $2, $3, 'pk')`,
			"cred-"+suffix(t), userID, "cid-"+suffix(t))
	}
	count := func(userID string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM webauthn_credentials WHERE user_id = $1`, userID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	u := newUser(t, "user", "pkall")
	other := newUser(t, "user", "pkall-other")
	seed(u.ID)
	seed(u.ID)
	seed(other.ID)
	if err := repo.DeleteAllPasskeyCredentialsForUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteAllPasskeyCredentialsForUser: %v", err)
	}
	if got, theirs := count(u.ID), count(other.ID); got != 0 || theirs != 1 {
		t.Fatalf("after delete-all: %d left for the account, %d for the other; want 0, 1", got, theirs)
	}
	if err := repo.DeleteAllPasskeyCredentialsForUser(ctx, u.ID); err != nil {
		t.Fatalf("delete-all with none left = %v, want nil", err)
	}
}

// ---- username reclaim (migration 0006) ----------------------------------------------

// A reclaim bars the squatter's UUID (never the contested name) and stashes its data
// once: a retry keeps the first window and handle, and a failed stash leaves no bar.
func TestReclaimUsernameContract(t *testing.T) {
	ctx := context.Background()
	t0 := mustNow().Truncate(time.Second)
	squatter, genuine := testUUID(t), testUUID(t)
	name := "Notch" + suffix(t)[:6]
	holdID := "hold-" + suffix(t)

	got, err := repo.ReclaimUsername(ctx, holdID, squatter, name, "ref-1", t0.Add(30*24*time.Hour))
	if err != nil || !got.Equal(t0.Add(30*24*time.Hour)) {
		t.Fatalf("reclaim = %v, %v; want %v", got, err, t0.Add(30*24*time.Hour))
	}
	for uuid, want := range map[string]bool{squatter: true, genuine: false} {
		if barred, err := repo.IsUsernameBlacklisted(ctx, uuid); err != nil || barred != want {
			t.Fatalf("IsUsernameBlacklisted(%s) = %v, %v; want %v", uuid, barred, err, want)
		}
	}

	got, err = repo.ReclaimUsername(ctx, "hold-retry-"+suffix(t), squatter, name, "ref-2", t0.Add(60*24*time.Hour))
	if err != nil || !got.Equal(t0.Add(30*24*time.Hour)) {
		t.Fatalf("retried reclaim = %v, %v; want the first window %v", got, err, t0.Add(30*24*time.Hour))
	}
	var holds int
	var ref string
	if err := db.QueryRow(`SELECT count(*), max(data_ref) FROM player_data_holds WHERE mc_uuid = $1`, squatter).Scan(&holds, &ref); err != nil {
		t.Fatalf("read holds: %v", err)
	}
	if holds != 1 || ref != "ref-1" {
		t.Fatalf("holds after a retry: %d with ref %q, want 1 with ref-1", holds, ref)
	}

	unarchived := testUUID(t)
	if _, err := repo.ReclaimUsername(ctx, "hold-bare-"+suffix(t), unarchived, name, "", t0.Add(30*24*time.Hour)); err != nil {
		t.Fatalf("reclaim without a data ref: %v", err)
	}
	var bare sql.NullString
	if err := db.QueryRow(`SELECT data_ref FROM player_data_holds WHERE mc_uuid = $1`, unarchived).Scan(&bare); err != nil {
		t.Fatalf("read bare hold: %v", err)
	}
	if bare.Valid {
		t.Fatalf("empty data ref stored as %q, want NULL", bare.String)
	}

	// The stash fails (its id is taken), so the bar written before it rolls back.
	orphan := testUUID(t)
	if _, err := repo.ReclaimUsername(ctx, holdID, orphan, name, "ref-3", t0.Add(30*24*time.Hour)); err == nil {
		t.Fatalf("reclaim reusing a hold id = nil, want an error")
	}
	if barred, err := repo.IsUsernameBlacklisted(ctx, orphan); err != nil || barred {
		t.Fatalf("UUID barred by a reclaim whose stash failed: %v, %v; want false", barred, err)
	}
}
