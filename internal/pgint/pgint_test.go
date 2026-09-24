//go:build pgint

// Package pgint holds the Postgres-level contract tests for the business stores
// (api.PGRepo, submit.PGStore, build.PGStore). They exist because pure unit suites
// run against fakes that encode the CONTRACT — and PGRepo drifted behind that
// contract three times (attempt accounting, a missing JOIN, a missing FOR UPDATE)
// while every unit test stayed green.
//
// They run ONLY against a throwaway database whose name contains "pgint": the
// harness drops and recreates the public schema and replays the real embedded
// migrations, so the schema under test is exactly what `felis migrate up`
// produces. Point it at the test database, never at a live one:
//
//	FELIS_TEST_PG_URL='postgres://felis:***@127.0.0.1:15432/felis_pgint?sslmode=disable' \
//	  go test -tags pgint ./internal/pgint/ -v
package pgint

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/store"
	"felis.lolicon.best/internal/submit"
)

var (
	db   *sql.DB
	repo *api.PGRepo
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("FELIS_TEST_PG_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, `pgint: FELIS_TEST_PG_URL must name a throwaway test database (its name must contain "pgint")`)
		os.Exit(1)
	}
	// The harness drops the public schema; refuse anything that does not look
	// like a dedicated test database, so a copy-paste of the production DSN
	// cannot wipe a live install.
	u, err := url.Parse(dsn)
	if err != nil || !strings.Contains(strings.TrimPrefix(u.Path, "/"), "pgint") {
		fmt.Fprintf(os.Stderr, "pgint: refusing %q: the database name must contain \"pgint\"\n", dsn)
		os.Exit(1)
	}
	ctx := context.Background()
	drv, err := store.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgint: open: %v\n", err)
		os.Exit(1)
	}
	defer drv.Close()
	// Fresh schema + the real migrations: violations of the schema contract
	// (bad enum, missing column) fail here, not mid-suite.
	for _, stmt := range []string{"DROP SCHEMA IF EXISTS public CASCADE", "CREATE SCHEMA public"} {
		if _, err := drv.DB().ExecContext(ctx, stmt); err != nil {
			fmt.Fprintf(os.Stderr, "pgint: reset schema: %v\n", err)
			os.Exit(1)
		}
	}
	ms, err := store.LoadMigrations()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgint: load migrations: %v\n", err)
		os.Exit(1)
	}
	if _, err := store.Up(ctx, drv, ms); err != nil {
		fmt.Fprintf(os.Stderr, "pgint: migrate: %v\n", err)
		os.Exit(1)
	}
	db = drv.DB()
	repo = api.NewPGRepo(db)
	os.Exit(m.Run())
}

// suffix returns a short unique hex string for per-test identifiers, so the
// suites can share one schema without interfering.
func suffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// testUUID formats random bytes as a UUID string (the version nibble does not
// matter to Postgres's uuid type).
func testUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func newUser(t *testing.T, role, prefix string) *api.UserView {
	t.Helper()
	u, err := repo.CreateUser(context.Background(),
		api.CreateUserInput{Username: prefix + "-" + suffix(t), Role: role}, "pgint")
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", role, err)
	}
	return u
}

func mustNow() time.Time { return time.Now().UTC() }

// ---- sessions -----------------------------------------------------------------

func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "sess")
	hash := "tok-" + suffix(t)
	expires := mustNow().Add(time.Hour)

	if err := repo.CreateSession(ctx, hash, u.ID, expires); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	su, err := repo.SessionUser(ctx, hash, mustNow())
	if err != nil {
		t.Fatalf("SessionUser live: %v", err)
	}
	if su.ID != u.ID || su.Role != "user" || su.EmailVerified {
		t.Fatalf("SessionedUser = %+v, want id=%s role=user verified=false", su, u.ID)
	}

	// Expiry is evaluated against the supplied clock, not wall time.
	if _, err := repo.SessionUser(ctx, hash, expires.Add(time.Second)); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("expired session = %v, want ErrNotFound", err)
	}

	// Revoke is idempotent and kills the session even before expiry.
	if err := repo.RevokeSession(ctx, hash); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if err := repo.RevokeSession(ctx, hash); err != nil {
		t.Fatalf("RevokeSession twice must be idempotent: %v", err)
	}
	if _, err := repo.SessionUser(ctx, hash, mustNow()); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("revoked session = %v, want ErrNotFound", err)
	}
	if _, err := repo.SessionUser(ctx, "no-such-"+suffix(t), mustNow()); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown session = %v, want ErrNotFound", err)
	}
}

// ---- email OTP: onboarding primitive ------------------------------------------

func TestOnboardingEmailOTPContract(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "otp")
	purpose := "onboard_email"
	addr := "otp-" + suffix(t) + "@example.net"
	now := mustNow()

	// No live code at all.
	if _, err := repo.VerifyEmailOTP(ctx, u.ID, purpose, "h1", now); !errors.Is(err, api.ErrOTPInvalid) {
		t.Fatalf("verify without code = %v, want ErrOTPInvalid", err)
	}

	if err := repo.CreateEmailOTP(ctx, "otp-"+suffix(t), u.ID, addr, "h-good", purpose, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateEmailOTP: %v", err)
	}
	// A mismatch charges one attempt and does NOT consume the code.
	if _, err := repo.VerifyEmailOTP(ctx, u.ID, purpose, "h-bad", now); !errors.Is(err, api.ErrOTPInvalid) {
		t.Fatalf("wrong code = %v, want ErrOTPInvalid", err)
	}
	assertAttempts(t, u.ID, purpose, 1)
	// The right code still redeems after a typo, and proves the address.
	got, err := repo.VerifyEmailOTP(ctx, u.ID, purpose, "h-good", now)
	if err != nil || got != addr {
		t.Fatalf("verify = (%q, %v), want (%q, nil)", got, err, addr)
	}
	assertEmailProven(t, u.ID, addr, true)
	// Replay: consumed codes stay dead.
	if _, err := repo.VerifyEmailOTP(ctx, u.ID, purpose, "h-good", now); !errors.Is(err, api.ErrOTPInvalid) {
		t.Fatalf("replay = %v, want ErrOTPInvalid", err)
	}

	// A new code supersedes the previous live one (at most one outstanding).
	if err := repo.CreateEmailOTP(ctx, "otp2-"+suffix(t), u.ID, addr, "h-new", purpose, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateEmailOTP (second): %v", err)
	}
	if _, err := repo.VerifyEmailOTP(ctx, u.ID, purpose, "h-old", now); !errors.Is(err, api.ErrOTPInvalid) {
		t.Fatalf("superseded code = %v, want ErrOTPInvalid", err)
	}
	if _, err := repo.VerifyEmailOTP(ctx, u.ID, purpose, "h-new", now); err != nil {
		t.Fatalf("newest code must redeem: %v", err)
	}

	// Expiry: a past expires_at is invalid, never a success.
	if err := repo.CreateEmailOTP(ctx, "otp3-"+suffix(t), u.ID, addr, "h-exp", purpose, now.Add(-time.Minute)); err != nil {
		t.Fatalf("CreateEmailOTP (expired): %v", err)
	}
	if _, err := repo.VerifyEmailOTP(ctx, u.ID, purpose, "h-exp", now); !errors.Is(err, api.ErrOTPInvalid) {
		t.Fatalf("expired code = %v, want ErrOTPInvalid", err)
	}

	// Lockout: otpMaxAttempts wrong guesses retire the code (correct hash or not).
	if err := repo.CreateEmailOTP(ctx, "otp4-"+suffix(t), u.ID, addr, "h-lock", purpose, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateEmailOTP (lock): %v", err)
	}
	locked := false
	for i := 0; i < 10 && !locked; i++ {
		_, err := repo.VerifyEmailOTP(ctx, u.ID, purpose, "h-wrong", now)
		switch {
		case errors.Is(err, api.ErrOTPInvalid):
		case errors.Is(err, api.ErrOTPLocked):
			locked = true
		default:
			t.Fatalf("wrong guess %d = %v, want invalid/locked", i+1, err)
		}
	}
	if !locked {
		t.Fatal("attempt budget never exhausted after 10 wrong guesses")
	}
	if _, err := repo.VerifyEmailOTP(ctx, u.ID, purpose, "h-lock", now); !errors.Is(err, api.ErrOTPLocked) {
		t.Fatalf("correct code after lock = %v, want ErrOTPLocked", err)
	}
	assertConsumed(t, u.ID, purpose, false)
}

// The verified-email uniqueness guard: two accounts cannot prove the same
// address (the login door resolves accounts BY verified email, so a duplicate
// would make identity ambiguous). This is the guard ConsumeLoginEmailOTP
// deliberately skips.
func TestOnboardingEmailOTPRejectsTakenEmail(t *testing.T) {
	ctx := context.Background()
	a, b := newUser(t, "user", "take-a"), newUser(t, "user", "take-b")
	addr := "shared-" + suffix(t) + "@example.net"
	now := mustNow()
	purpose := "onboard_email"

	if err := repo.CreateEmailOTP(ctx, "tka-"+suffix(t), a.ID, addr, "h-a", purpose, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateEmailOTP(a): %v", err)
	}
	if _, err := repo.VerifyEmailOTP(ctx, a.ID, purpose, "h-a", now); err != nil {
		t.Fatalf("verify a: %v", err)
	}
	if err := repo.CreateEmailOTP(ctx, "tkb-"+suffix(t), b.ID, addr, "h-b", purpose, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateEmailOTP(b): %v", err)
	}
	if _, err := repo.VerifyEmailOTP(ctx, b.ID, purpose, "h-b", now); !errors.Is(err, api.ErrEmailTaken) {
		t.Fatalf("second account proving a taken email = %v, want ErrEmailTaken", err)
	}
	// The address, not the code, was the problem: b's code stays live.
	assertConsumed(t, b.ID, purpose, false)
	// The invariant is also enforced by the database, not only the app guard: a
	// direct write that bypasses VerifyEmailOTP still loses (uppercased to prove
	// the index keys on lower(email)).
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET email = $2, email_verified = true WHERE id = $1`, b.ID, strings.ToUpper(addr)); err == nil {
		t.Fatal("a direct duplicate verified-email write succeeded; users_verified_email_unique is missing")
	}
	// And the refusal does not wedge b: its OWN address still verifies fine.
	own := "own-" + suffix(t) + "@example.net"
	if err := repo.CreateEmailOTP(ctx, "tkb2-"+suffix(t), b.ID, own, "h-b2", purpose, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateEmailOTP(b, own): %v", err)
	}
	if _, err := repo.VerifyEmailOTP(ctx, b.ID, purpose, "h-b2", now); err != nil {
		t.Fatalf("b must still be able to prove its own address: %v", err)
	}
}

// ---- email OTP: pre-session login primitive (regression: the #16 drift) --------

func TestConsumeLoginEmailOTPContract(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "login-otp")
	purpose := "login_email"
	addr := "login-" + suffix(t) + "@example.net"
	now := mustNow()

	if err := repo.CreateEmailOTP(ctx, "lotp-"+suffix(t), u.ID, addr, "h-good", purpose, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateEmailOTP: %v", err)
	}
	// Wrong guesses must ACCUMULATE (the drift left attempts at zero) and must
	// not consume the code.
	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-bad", now); !errors.Is(err, api.ErrOTPInvalid) {
		t.Fatalf("wrong code = %v, want ErrOTPInvalid", err)
	}
	assertAttempts(t, u.ID, purpose, 1)
	// Redemption succeeds and is single-use.
	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-good", now); err != nil {
		t.Fatalf("correct code = %v, want nil", err)
	}
	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-good", now); !errors.Is(err, api.ErrOTPInvalid) {
		t.Fatalf("replay = %v, want ErrOTPInvalid", err)
	}
	// The login primitive writes NO identity state: the user's email stays empty.
	assertEmailProven(t, u.ID, "", false)

	// Lockout parity with the onboarding primitive.
	if err := repo.CreateEmailOTP(ctx, "lotp2-"+suffix(t), u.ID, addr, "h-lock", purpose, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateEmailOTP (lock): %v", err)
	}
	locked := false
	for i := 0; i < 10 && !locked; i++ {
		switch err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-wrong", now); {
		case errors.Is(err, api.ErrOTPInvalid):
		case errors.Is(err, api.ErrOTPLocked):
			locked = true
		default:
			t.Fatalf("wrong guess %d = %v, want invalid/locked", i+1, err)
		}
	}
	if !locked {
		t.Fatal("attempt budget never exhausted after 10 wrong guesses")
	}
	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-lock", now); !errors.Is(err, api.ErrOTPLocked) {
		t.Fatalf("correct code after lock = %v, want ErrOTPLocked", err)
	}
}

// The account-level wrong-code budget must survive supersede: minting a fresh
// code resets the per-code attempts, and the public login door can mint one a
// minute, so only a counter outside email_otps bounds guessing per account.
// TestAuditAttributionContract pins migration 0023: actor_user_id is filled
// from a real account id and falls to NULL (never a failed insert) for an id
// with no users row; client_ip and user_agent land when given.
func TestAuditAttributionContract(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "audit")
	action := "pgint.audit." + suffix(t)
	for _, e := range []api.AuditEntry{
		{Actor: u.Username, ActorUserID: u.ID, Action: action, ClientIP: "2001:db8::7", UserAgent: "pgint/1"},
		{Actor: "sso-subject", ActorUserID: "not-a-user-" + suffix(t), Action: action},
		{Actor: "anonymous", Action: action, ClientIP: "203.0.113.9"},
	} {
		e.Source = "external"
		if err := repo.Audit(ctx, e); err != nil {
			t.Fatalf("Audit(%s): %v", e.Actor, err)
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT actor, COALESCE(actor_user_id, ''), COALESCE(host(client_ip), ''), COALESCE(user_agent, '')
		FROM audit_logs WHERE action = $1 ORDER BY id`, action)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var actor, uid, ip, ua string
		if err := rows.Scan(&actor, &uid, &ip, &ua); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, strings.Join([]string{actor, uid, ip, ua}, "|"))
	}
	want := []string{
		u.Username + "|" + u.ID + "|2001:db8::7|pgint/1",
		"sso-subject|||",
		"anonymous||203.0.113.9|",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The session principal carries the username the rows are signed with.
	hash := "audit-sess-" + suffix(t)
	if err := repo.CreateSession(ctx, hash, u.ID, mustNow().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	su, err := repo.SessionUser(ctx, hash, mustNow())
	if err != nil || su.Username != u.Username {
		t.Fatalf("SessionUser = %+v, %v; want username %q", su, err, u.Username)
	}
}

func TestOTPFailureBudgetContract(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "otp-budget")
	purpose := "login_email"
	addr := "budget-" + suffix(t) + "@example.net"
	start := mustNow().Truncate(time.Second)
	mint := func(hash string, at time.Time) {
		t.Helper()
		if err := repo.CreateEmailOTP(ctx, "bud-"+suffix(t), u.ID, addr, hash, purpose, at.Add(5*time.Minute)); err != nil {
			t.Fatalf("CreateEmailOTP: %v", err)
		}
	}

	// Ten wrong guesses spread over three codes; the per-code cap (5) is never hit.
	var tripped *api.OTPAccountLockedError
	for i := 0; i < 10; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		if i%4 == 0 {
			mint("h-good", at)
		}
		err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-wrong", at)
		if i < 9 {
			if !errors.Is(err, api.ErrOTPInvalid) {
				t.Fatalf("wrong guess %d = %v, want ErrOTPInvalid", i+1, err)
			}
			continue
		}
		if !errors.As(err, &tripped) || !tripped.JustLocked {
			t.Fatalf("10th wrong guess = %v, want a fresh *OTPAccountLockedError", err)
		}
	}
	if want := start.Add(24 * time.Hour); !tripped.Until.Equal(want) {
		t.Fatalf("lock until %v, want %v (window opened by the first wrong guess)", tripped.Until, want)
	}

	// The right code on a live, barely-used code is refused while locked.
	at := start.Add(10 * time.Minute)
	var lock *api.OTPAccountLockedError
	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-good", at); !errors.As(err, &lock) || lock.JustLocked {
		t.Fatalf("right code while locked = %v, want a standing *OTPAccountLockedError", err)
	}
	if until, err := repo.OTPLockedUntil(ctx, u.ID, purpose, at); err != nil || until.IsZero() {
		t.Fatalf("OTPLockedUntil during lock = %v, %v", until, err)
	}
	// Other purposes of the same account are separate doors.
	if until, err := repo.OTPLockedUntil(ctx, u.ID, "onboard_email", at); err != nil || !until.IsZero() {
		t.Fatalf("onboard door locked too: %v, %v", until, err)
	}

	// After the window the door reopens and the count starts over.
	later := start.Add(25 * time.Hour)
	if until, err := repo.OTPLockedUntil(ctx, u.ID, purpose, later); err != nil || !until.IsZero() {
		t.Fatalf("OTPLockedUntil after window = %v, %v", until, err)
	}
	mint("h-good", later)
	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-wrong", later); !errors.Is(err, api.ErrOTPInvalid) {
		t.Fatalf("first wrong guess of a new window = %v, want ErrOTPInvalid", err)
	}
	var failures int
	if err := db.QueryRowContext(ctx,
		`SELECT failures FROM otp_failure_windows WHERE user_id = $1 AND purpose = $2`, u.ID, purpose).Scan(&failures); err != nil {
		t.Fatalf("read budget row: %v", err)
	}
	if failures != 1 {
		t.Fatalf("failures after window reset = %d, want 1", failures)
	}
	if err := repo.ConsumeLoginEmailOTP(ctx, u.ID, purpose, "h-good", later); err != nil {
		t.Fatalf("right code after the window = %v, want nil", err)
	}

	// The onboarding primitive shares the rule.
	onboard := "onboard_email"
	for i := 0; i < 10; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		if i%4 == 0 {
			if err := repo.CreateEmailOTP(ctx, "bud-on-"+suffix(t), u.ID, addr, "h-good", onboard, at.Add(5*time.Minute)); err != nil {
				t.Fatalf("CreateEmailOTP onboard: %v", err)
			}
		}
		_, err := repo.VerifyEmailOTP(ctx, u.ID, onboard, "h-wrong", at)
		if i == 9 && !errors.Is(err, api.ErrOTPAccountLocked) {
			t.Fatalf("10th wrong onboarding guess = %v, want ErrOTPAccountLocked", err)
		}
	}
	if _, err := repo.VerifyEmailOTP(ctx, u.ID, onboard, "h-good", start.Add(10*time.Minute)); !errors.Is(err, api.ErrOTPAccountLocked) {
		t.Fatalf("right onboarding code while locked = %v, want ErrOTPAccountLocked", err)
	}
	assertEmailProven(t, u.ID, "", false)
}

// ---- user admin (spec §7) -------------------------------------------------------

// The claim gate must hold under concurrency (audit #4): two simultaneous claims
// by one user for two different ownerless servers must not both pass a
// max_servers=1 cap. ClaimServer now owns the gate (advisory lock + four-dimension
// re-check in the same transaction as the ownership write), so this drives real
// goroutines against real Postgres.
func TestClaimServerQuotaAtomicGate(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "quota")
	one := 1
	if _, err := repo.SetQuotas(ctx, u.ID, api.QuotaInput{MaxServers: &one}, "pgint"); err != nil {
		t.Fatalf("SetQuotas: %v", err)
	}
	seed := func(name string) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, 100, 128, 1)`,
			name); err != nil {
			t.Fatalf("seed server %s: %v", name, err)
		}
	}
	s1, s2 := "qa-"+suffix(t), "qb-"+suffix(t)
	seed(s1)
	seed(s2)

	var wg sync.WaitGroup
	results := make([]error, 2)
	claimed := make([]bool, 2)
	for i, name := range []string{s1, s2} {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			claimed[i], results[i] = repo.ClaimServer(ctx, name, u.ID)
		}(i, name)
	}
	wg.Wait()

	var wins, gated, other int
	for i := range results {
		switch {
		case results[i] == nil && claimed[i]:
			wins++
		case errors.Is(results[i], api.ErrQuotaExceeded):
			gated++
		default:
			other++
			t.Logf("unexpected outcome %d: claimed=%v err=%v", i, claimed[i], results[i])
		}
	}
	if wins != 1 || gated != 1 || other != 0 {
		t.Fatalf("concurrent claims: wins=%d gated=%d other=%d, want 1/1/0", wins, gated, other)
	}
	var owned int
	if err := db.QueryRow(`SELECT count(*) FROM servers WHERE owner_id = $1 AND deleted_at IS NULL`, u.ID).Scan(&owned); err != nil {
		t.Fatalf("count owned: %v", err)
	}
	if owned != 1 {
		t.Fatalf("owned servers = %d, want exactly 1 (no over-provision)", owned)
	}

	// Sequential, the gate answers identically: a third claim meets the same 403.
	s3 := "qc-" + suffix(t)
	seed(s3)
	if _, err := repo.ClaimServer(ctx, s3, u.ID); !errors.Is(err, api.ErrQuotaExceeded) {
		t.Fatalf("third claim = %v, want ErrQuotaExceeded", err)
	}

	// No quota row → unlimited: a fresh user claims both remaining ownerless rows.
	otherUser := newUser(t, "user", "quota-free")
	var free1 string
	if err := db.QueryRow(
		`SELECT name FROM servers WHERE name IN ($1, $2) AND owner_id IS NULL ORDER BY name LIMIT 1`,
		s1, s2).Scan(&free1); err != nil {
		t.Fatalf("find the race's unclaimed row: %v", err)
	}
	for _, name := range []string{free1, s3} {
		if ok, err := repo.ClaimServer(ctx, name, otherUser.ID); err != nil || !ok {
			t.Fatalf("quota-free claim %s = (%v, %v), want (true, nil)", name, ok, err)
		}
	}
}

// An admin email edit must not carry a verification over to an address nobody
// proved: the verified flag is exactly what the pre-session login resolves on
// (UserByEmail), and only VerifyEmailOTP may assert it — the same rationale as
// SetUserEmail. A no-op edit that passes the same value keeps the proof.
func TestUserAdminEmailEditClearsVerification(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "admin-edit")
	purpose := "onboard_email"
	addr := "edit-" + suffix(t) + "@example.net"
	now := mustNow()

	if err := repo.CreateEmailOTP(ctx, "ae-"+suffix(t), u.ID, addr, "h", purpose, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateEmailOTP: %v", err)
	}
	if _, err := repo.VerifyEmailOTP(ctx, u.ID, purpose, "h", now); err != nil {
		t.Fatalf("verify: %v", err)
	}
	assertEmailProven(t, u.ID, addr, true)

	same := addr
	if _, err := repo.UpdateUser(ctx, u.ID, api.UpdateUserInput{Email: &same}, "pgint"); err != nil {
		t.Fatalf("UpdateUser (same email): %v", err)
	}
	assertEmailProven(t, u.ID, addr, true)

	next := "edit2-" + suffix(t) + "@example.net"
	if _, err := repo.UpdateUser(ctx, u.ID, api.UpdateUserInput{Email: &next}, "pgint"); err != nil {
		t.Fatalf("UpdateUser (new email): %v", err)
	}
	assertEmailProven(t, u.ID, next, false)
}

// The user-scoped admin sub-resources must refuse an id that has no live users
// row with ErrNotFound (→ the API's 404). The quota upsert and the account-link
// insert touch user_id foreign keys, so before the requireLiveUser guard the
// live drill returned a 500 on both (audit #30); the quotas read answered a
// zero-value "unlimited" view for an id that never existed.
func TestAdminSubresourcesRequireLiveUser(t *testing.T) {
	ctx := context.Background()
	ghost := "usr-ghost-" + suffix(t)

	if _, err := repo.GetQuotas(ctx, ghost); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("GetQuotas(ghost) = %v, want ErrNotFound", err)
	}
	three := 3
	if _, err := repo.SetQuotas(ctx, ghost, api.QuotaInput{MaxServers: &three}, "pgint"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("SetQuotas(ghost) = %v, want ErrNotFound", err)
	}
	if err := repo.LinkAccount(ctx, ghost, testUUID(t), "mojang"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("LinkAccount(ghost) = %v, want ErrNotFound", err)
	}

	// Control: the same calls land for a live user.
	u := newUser(t, "user", "subres")
	if _, err := repo.SetQuotas(ctx, u.ID, api.QuotaInput{MaxServers: &three}, "pgint"); err != nil {
		t.Fatalf("SetQuotas(live): %v", err)
	}
	got, err := repo.GetQuotas(ctx, u.ID)
	if err != nil || got.MaxServers == nil || *got.MaxServers != 3 {
		t.Fatalf("GetQuotas(live) = %+v, %v; want max_servers=3", got, err)
	}
	if err := repo.LinkAccount(ctx, u.ID, testUUID(t), "mojang"); err != nil {
		t.Fatalf("LinkAccount(live): %v", err)
	}

	// A soft-deleted user is no longer a live target either.
	if err := repo.DeleteUser(ctx, u.ID, "pgint"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := repo.SetQuotas(ctx, u.ID, api.QuotaInput{MaxServers: &three}, "pgint"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("SetQuotas(deleted) = %v, want ErrNotFound", err)
	}
}

// A disabled or soft-deleted account must be dead in the real database: login
// resolution (UserByEmail) refuses it, session validation (SessionUser) refuses
// even a freshly inserted session, and DeleteUser severs the account's passkeys
// and Minecraft links so the closed account keeps neither a standing credential
// nor the UNIQUE(mc_uuid) claim. Live audit #33: a deleted user re-logged-in via
// the email door and held a working session.
func TestDeadAccountsAreLockedOutInPG(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "dead")
	addr := "dead-" + suffix(t) + "@example.net"
	now := mustNow()

	if err := repo.CreateEmailOTP(ctx, "de-"+suffix(t), u.ID, addr, "h", "onboard_email", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateEmailOTP: %v", err)
	}
	if _, err := repo.VerifyEmailOTP(ctx, u.ID, "onboard_email", "h", now); err != nil {
		t.Fatalf("VerifyEmailOTP: %v", err)
	}

	// Alive: the login door resolves the account and a session validates.
	if _, err := repo.UserByEmail(ctx, addr); err != nil {
		t.Fatalf("alive UserByEmail: %v", err)
	}
	hash := "h-" + suffix(t)
	if err := repo.CreateSession(ctx, hash, u.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := repo.SessionUser(ctx, hash, now); err != nil {
		t.Fatalf("alive SessionUser: %v", err)
	}

	// Disabled: invisible to the door, session stops authenticating.
	if err := repo.SetUserDisabled(ctx, u.ID, true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if _, err := repo.UserByEmail(ctx, addr); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("disabled UserByEmail = %v, want ErrNotFound", err)
	}
	if _, err := repo.SessionUser(ctx, hash, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("disabled SessionUser = %v, want ErrNotFound", err)
	}
	if err := repo.SetUserDisabled(ctx, u.ID, false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}

	// Deleted: same refusals, a fresh session cannot authenticate, and the
	// identity assets are gone from the tables.
	mc := testUUID(t)
	if err := repo.LinkAccount(ctx, u.ID, mc, "mojang"); err != nil {
		t.Fatalf("LinkAccount: %v", err)
	}
	// A disabled account's intact link carries no in-game standing (the doors read
	// it like an unlinked UUID), and resolves again once re-enabled.
	if err := repo.SetUserDisabled(ctx, u.ID, true); err != nil {
		t.Fatalf("disable with link: %v", err)
	}
	if _, err := repo.UserByMCUUID(ctx, mc); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("UserByMCUUID(disabled) = %v, want ErrNotFound", err)
	}
	if err := repo.SetUserDisabled(ctx, u.ID, false); err != nil {
		t.Fatalf("re-enable with link: %v", err)
	}
	if id, err := repo.UserByMCUUID(ctx, mc); err != nil || id != u.ID {
		t.Fatalf("UserByMCUUID(re-enabled) = %q, %v; want %q", id, err, u.ID)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO webauthn_credentials (id, user_id, credential_id, public_key) VALUES ($1,$2,$3,'pk')`,
		"cred-"+suffix(t), u.ID, "cid-"+suffix(t)); err != nil {
		t.Fatalf("seed passkey: %v", err)
	}
	if err := repo.DeleteUser(ctx, u.ID, "pgint"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := repo.UserByEmail(ctx, addr); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("deleted UserByEmail = %v, want ErrNotFound", err)
	}
	hash2 := "h2-" + suffix(t)
	if err := repo.CreateSession(ctx, hash2, u.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession (deleted): %v", err)
	}
	if _, err := repo.SessionUser(ctx, hash2, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("deleted SessionUser = %v, want ErrNotFound", err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM account_links WHERE user_id = $1`,
		`SELECT count(*) FROM webauthn_credentials WHERE user_id = $1`,
	} {
		var n int
		if err := db.QueryRowContext(ctx, q, u.ID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("DeleteUser left %d rows matching %q; the closed account must keep no asset", n, q)
		}
	}
}

// VerifyLinkCode's takeover rule: a fresh in-game code (proof the caller holds the
// UUID) lets a live account take over a link whose account was SOFT-DELETED — the
// migrated-source case, whose retire keeps the link but kills the account — while a
// merely DISABLED holder keeps its identity (takeover there would bypass the
// lockout) and the refused code survives.
func TestVerifyLinkCodeTakesOverDeletedLink(t *testing.T) {
	ctx := context.Background()
	now := mustNow()

	// The retired source: linked, then retired the way RedeemMigration retires one.
	src := newUser(t, "user", "retire-src")
	mc := testUUID(t)
	if err := repo.LinkAccount(ctx, src.ID, mc, "mojang"); err != nil {
		t.Fatalf("LinkAccount(src): %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET disabled = true, deleted_at = now() WHERE id = $1`, src.ID); err != nil {
		t.Fatalf("retire src: %v", err)
	}

	taker := newUser(t, "user", "taker")
	code := "tk-" + suffix(t)
	if err := repo.CreateLinkCode(ctx, code, mc, "mojang", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateLinkCode: %v", err)
	}
	if _, _, err := repo.VerifyLinkCode(ctx, taker.ID, code, now); err != nil {
		t.Fatalf("takeover verify: %v", err)
	}
	if id, err := repo.UserByMCUUID(ctx, mc); err != nil || id != taker.ID {
		t.Fatalf("after takeover UserByMCUUID = %q, %v; want %q", id, err, taker.ID)
	}

	// A disabled (not deleted) holder keeps the identity; the conflict arm must not
	// consume the code and must leave the link where it was.
	locked := newUser(t, "user", "locked")
	mc2 := testUUID(t)
	if err := repo.LinkAccount(ctx, locked.ID, mc2, "mojang"); err != nil {
		t.Fatalf("LinkAccount(locked): %v", err)
	}
	if err := repo.SetUserDisabled(ctx, locked.ID, true); err != nil {
		t.Fatalf("disable locked: %v", err)
	}
	code2 := "lk-" + suffix(t)
	if err := repo.CreateLinkCode(ctx, code2, mc2, "mojang", now.Add(5*time.Minute)); err != nil {
		t.Fatalf("CreateLinkCode(code2): %v", err)
	}
	if _, _, err := repo.VerifyLinkCode(ctx, taker.ID, code2, now); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("takeover of a disabled holder = %v, want ErrConflict", err)
	}
	var stillLinked string
	if err := db.QueryRowContext(ctx, `SELECT user_id FROM account_links WHERE mc_uuid = $1`, mc2).Scan(&stillLinked); err != nil || stillLinked != locked.ID {
		t.Fatalf("disabled holder's link moved to %q, %v; want %q", stillLinked, err, locked.ID)
	}
	var codeRows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM account_link_codes WHERE code = $1`, code2).Scan(&codeRows); err != nil || codeRows != 1 {
		t.Fatalf("refused code rows = %d, %v; want 1 (not consumed)", codeRows, err)
	}
}

// The owner tier the panel gates on must actually be WRITTEN: until this
// contract had a test, every provisioning path wrote 'admin', so the whole
// owner surface (user administration) was unreachable in a fresh install.
func TestOwnerProvisioningWritesOwnerRole(t *testing.T) {
	ctx := context.Background()
	name := "owner-" + suffix(t)
	id := "usr-" + suffix(t)
	if err := repo.UpsertOwner(ctx, id, name, "owner-"+suffix(t)+"@example.net"); err != nil {
		t.Fatalf("UpsertOwner: %v", err)
	}
	if role := userRole(t, id); role != "owner" {
		t.Fatalf("UpsertOwner role = %q, want owner", role)
	}
	// Re-running the break-glass path resets (email) and PROMOTES (role) in
	// place — the documented upgrade for a pre-0011 'admin' Owner row.
	if err := repo.UpsertOwner(ctx, "usr-other-"+suffix(t), name, "reset@example.net"); err != nil {
		t.Fatalf("UpsertOwner (reset): %v", err)
	}
	var gotID, email, role string
	if err := db.QueryRow(`SELECT id, COALESCE(email, ''), role::text FROM users WHERE username = $1`, name).
		Scan(&gotID, &email, &role); err != nil {
		t.Fatalf("read owner row: %v", err)
	}
	if gotID != id || email != "reset@example.net" || role != "owner" {
		t.Fatalf("reset row = (%s, %s, %s), want id preserved + email reset + owner", gotID, email, role)
	}
	if ok, err := repo.AdminExists(ctx); err != nil || !ok {
		t.Fatalf("AdminExists = (%v, %v), want true (the owner counts as staff)", ok, err)
	}
	// OwnerUsername names the seat the console guard protects. The shared test
	// database may hold owner rows from earlier tests, so assert the returned name
	// IS an active owner rather than one specific row.
	seat, err := repo.OwnerUsername(ctx)
	if err != nil {
		t.Fatalf("OwnerUsername: %v", err)
	}
	if seat == "" {
		t.Fatal("OwnerUsername = empty, want an active owner seat")
	}
	var active int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE username = $1 AND role = 'owner' AND deleted_at IS NULL`, seat).
		Scan(&active); err != nil || active != 1 {
		t.Fatalf("OwnerUsername returned %q, not an active owner row (count=%d err=%v)", seat, active, err)
	}
	// Operators stay plain admins: the owner tier stays singular.
	opID := "usr-op-" + suffix(t)
	opName := "op-" + suffix(t)
	if err := repo.InsertOperator(ctx, opID, opName, ""); err != nil {
		t.Fatalf("InsertOperator: %v", err)
	}
	if role := userRole(t, opID); role != "admin" {
		t.Fatalf("InsertOperator role = %q, want admin", role)
	}
	// A taken username must surface as api.ErrConflict: the console routes its
	// rename prompt off that sentinel (the cmd fake encoded the contract; PGRepo
	// returned the raw driver error until this arm was mapped).
	if err := repo.InsertOperator(ctx, "usr-op2-"+suffix(t), opName, ""); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("InsertOperator on a taken username = %v, want ErrConflict", err)
	}
}

// The setup wizard's MC-bind path establishes THE Owner, so it writes the same
// role as break-glass rather than a plain admin.
func TestCompleteOwnerSetupWritesOwnerRole(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	mc := testUUID(t)
	code := "osc-" + suffix(t)
	if err := repo.CreateLinkCode(ctx, code, mc, "mojang", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("CreateLinkCode: %v", err)
	}
	newID := "usr-setup-" + suffix(t)
	userID, gotUUID, src, err := repo.CompleteOwnerSetup(ctx, newID, code, now,
		"tok-"+suffix(t), now.Add(time.Hour))
	if err != nil || userID != newID || gotUUID != mc || src != "mojang" {
		t.Fatalf("CompleteOwnerSetup = (%s, %s, %s, %v), want (%s, %s, mojang, nil)",
			userID, gotUUID, src, err, newID, mc)
	}
	if role := userRole(t, newID); role != "owner" {
		t.Fatalf("CompleteOwnerSetup role = %q, want owner", role)
	}
}

// IsProtectedAdminLink is one of the staff predicates the owner role must flow
// through: the Owner logging in via the third-party Yggdrasil must never be
// barred by a Mojang-priority reclaim, exactly like an Operator.
func TestIsProtectedAdminLinkStaffRoles(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	link := func(userID, source string) string {
		t.Helper()
		mc := testUUID(t)
		code := "pro-" + suffix(t)
		if err := repo.CreateLinkCode(ctx, code, mc, source, now.Add(10*time.Minute)); err != nil {
			t.Fatalf("CreateLinkCode(%s): %v", source, err)
		}
		if _, _, err := repo.VerifyLinkCode(ctx, userID, code, now); err != nil {
			t.Fatalf("VerifyLinkCode(%s): %v", source, err)
		}
		return mc
	}

	ownerID := "usr-" + suffix(t)
	if err := repo.UpsertOwner(ctx, ownerID, "prot-owner-"+suffix(t), ""); err != nil {
		t.Fatalf("UpsertOwner: %v", err)
	}
	admin := newUser(t, "admin", "prot-admin")
	player := newUser(t, "user", "prot-player")

	for _, tc := range []struct {
		name string
		mc   string
		want bool
	}{
		{"owner via thirdparty", link(ownerID, "thirdparty"), true},
		{"admin via thirdparty", link(admin.ID, "thirdparty"), true},
		{"player via thirdparty", link(player.ID, "thirdparty"), false},
		{"admin via mojang", link(admin.ID, "mojang"), false},
	} {
		got, err := repo.IsProtectedAdminLink(ctx, tc.mc)
		if err != nil || got != tc.want {
			t.Fatalf("%s = (%v, %v), want %v", tc.name, got, err, tc.want)
		}
	}
}

// ---- op.console staff login state machine --------------------------------------

func TestOpLoginStateMachine(t *testing.T) {
	ctx := context.Background()
	staff := newUser(t, "admin", "staff")
	approver := newUser(t, "admin", "approver")
	now := mustNow()
	id := "opl-" + suffix(t)

	if err := repo.CreateOpLoginRequest(ctx, id, staff.ID, "staff@example.net", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("CreateOpLoginRequest: %v", err)
	}
	req, err := repo.OpLoginRequestByID(ctx, id)
	if err != nil {
		t.Fatalf("OpLoginRequestByID: %v", err)
	}
	if req.Status != "pending" || req.Consumed {
		t.Fatalf("fresh request = %+v, want pending+unconsumed", req)
	}
	if req.Username != "" {
		t.Errorf("ByID must not join a username, got %q", req.Username)
	}

	// The pending list is what the in-game admin sees: it must name the staff
	// user and carry a real created_at (the #17 drift left both empty).
	pending, err := repo.ListPendingOpLogins(ctx, now)
	if err != nil {
		t.Fatalf("ListPendingOpLogins: %v", err)
	}
	found := false
	for _, p := range pending {
		if p.ID != id {
			continue
		}
		found = true
		if p.Username != staff.Username {
			t.Errorf("pending username = %q, want %q", p.Username, staff.Username)
		}
		if p.CreatedAt.IsZero() {
			t.Error("pending created_at is zero")
		}
	}
	if !found {
		t.Fatal("fresh pending request missing from the list")
	}

	// Approve -> consume -> single use; second approve/consume are ErrNotFound.
	if err := repo.ApproveOpLogin(ctx, id, approver.ID, now); err != nil {
		t.Fatalf("ApproveOpLogin: %v", err)
	}
	if err := repo.ApproveOpLogin(ctx, id, approver.ID, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("double approve = %v, want ErrNotFound", err)
	}
	if err := repo.ConsumeOpLoginRequest(ctx, id, now); err != nil {
		t.Fatalf("ConsumeOpLoginRequest: %v", err)
	}
	if err := repo.ConsumeOpLoginRequest(ctx, id, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("double consume = %v, want ErrNotFound", err)
	}

	// An expired request is dead on every path.
	oldID := "opl-old-" + suffix(t)
	if err := repo.CreateOpLoginRequest(ctx, oldID, staff.ID, "staff@example.net", now.Add(-time.Minute)); err != nil {
		t.Fatalf("CreateOpLoginRequest (expired): %v", err)
	}
	pending, err = repo.ListPendingOpLogins(ctx, now)
	if err != nil {
		t.Fatalf("ListPendingOpLogins (after expiry): %v", err)
	}
	for _, p := range pending {
		if p.ID == oldID {
			t.Fatal("expired request must not be listed as actionable")
		}
	}
	if err := repo.ApproveOpLogin(ctx, oldID, approver.ID, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("approve expired = %v, want ErrNotFound", err)
	}
	if err := repo.ConsumeOpLoginRequest(ctx, oldID, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("consume expired = %v, want ErrNotFound", err)
	}
}

// ---- account links -------------------------------------------------------------

func TestVerifyLinkCodeContract(t *testing.T) {
	ctx := context.Background()
	u1, u2 := newUser(t, "user", "link-a"), newUser(t, "user", "link-b")
	mc := testUUID(t)
	now := mustNow()

	code := "lc-" + suffix(t)
	if err := repo.CreateLinkCode(ctx, code, mc, "mojang", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("CreateLinkCode: %v", err)
	}
	gotUUID, src, err := repo.VerifyLinkCode(ctx, u1.ID, code, now)
	if err != nil || gotUUID != mc || src != "mojang" {
		t.Fatalf("VerifyLinkCode = (%s, %s, %v), want (%s, mojang, nil)", gotUUID, src, err, mc)
	}
	if ok, err := repo.IsLinked(ctx, u1.ID); err != nil || !ok {
		t.Fatalf("IsLinked after verify = (%v, %v), want (true, nil)", ok, err)
	}
	// Single use.
	if _, _, err := repo.VerifyLinkCode(ctx, u1.ID, code, now); !errors.Is(err, api.ErrLinkCodeInvalid) {
		t.Fatalf("replay = %v, want ErrLinkCodeInvalid", err)
	}

	// A uuid already linked to ANOTHER user is a conflict, not a silent takeover.
	code2 := "lc2-" + suffix(t)
	if err := repo.CreateLinkCode(ctx, code2, mc, "mojang", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("CreateLinkCode (2): %v", err)
	}
	if _, _, err := repo.VerifyLinkCode(ctx, u2.ID, code2, now); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("cross-user link = %v, want ErrConflict", err)
	}

	// Re-verifying the same (user, uuid) is idempotent and refreshes authSource.
	code3 := "lc3-" + suffix(t)
	if err := repo.CreateLinkCode(ctx, code3, mc, "thirdparty", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("CreateLinkCode (3): %v", err)
	}
	if _, src, err := repo.VerifyLinkCode(ctx, u1.ID, code3, now); err != nil || src != "thirdparty" {
		t.Fatalf("re-verify = (%s, %v), want (thirdparty, nil)", src, err)
	}
	assertLinkAuthSource(t, u1.ID, mc, "thirdparty")

	// Expired code: invalid, never usable.
	expired := "lc4-" + suffix(t)
	if err := repo.CreateLinkCode(ctx, expired, testUUID(t), "mojang", now.Add(-time.Minute)); err != nil {
		t.Fatalf("CreateLinkCode (expired): %v", err)
	}
	if _, _, err := repo.VerifyLinkCode(ctx, u2.ID, expired, now); !errors.Is(err, api.ErrLinkCodeInvalid) {
		t.Fatalf("expired code = %v, want ErrLinkCodeInvalid", err)
	}
}

// RedeemPlayerBindCode is the account-less console bootstrap (regression: the #18
// concurrency drift). Its documented outcomes are exercised here, including the
// one that must NOT consume the code.
func TestRedeemPlayerBindCodeContract(t *testing.T) {
	ctx := context.Background()
	now := mustNow()

	// New uuid: creates a user + link, consumes the code.
	mcNew := testUUID(t)
	codeNew := "bc-" + suffix(t)
	if err := repo.CreateLinkCode(ctx, codeNew, mcNew, "mojang", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("CreateLinkCode: %v", err)
	}
	newID := "player-" + suffix(t)
	userID, gotUUID, src, err := repo.RedeemPlayerBindCode(ctx, newID, codeNew, now)
	if err != nil || userID != newID || gotUUID != mcNew || src != "mojang" {
		t.Fatalf("first redeem = (%s, %s, %s, %v), want (%s, %s, mojang, nil)", userID, gotUUID, src, err, newID, mcNew)
	}
	if ok, _ := repo.IsLinked(ctx, newID); !ok {
		t.Fatal("redeem must write the account link")
	}
	if _, _, _, err := repo.RedeemPlayerBindCode(ctx, "player-x-"+suffix(t), codeNew, now); !errors.Is(err, api.ErrLinkCodeInvalid) {
		t.Fatalf("consumed code = %v, want ErrLinkCodeInvalid", err)
	}

	// Same uuid again: returns the EXISTING player, even when asked for a new id.
	codeAgain := "bc2-" + suffix(t)
	if err := repo.CreateLinkCode(ctx, codeAgain, mcNew, "mojang", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("CreateLinkCode (again): %v", err)
	}
	if userID, _, _, err := repo.RedeemPlayerBindCode(ctx, "player-y-"+suffix(t), codeAgain, now); err != nil || userID != newID {
		t.Fatalf("second redeem = (%s, %v), want the same user %s", userID, err, newID)
	}

	// Staff uuid: forbidden, and the code is deliberately NOT consumed.
	staff := newUser(t, "admin", "bound-staff")
	mcStaff := testUUID(t)
	staffCode := "bc3-" + suffix(t)
	if err := repo.CreateLinkCode(ctx, staffCode, mcStaff, "mojang", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("CreateLinkCode (staff): %v", err)
	}
	if _, _, err := repo.VerifyLinkCode(ctx, staff.ID, staffCode, now); err != nil {
		t.Fatalf("link staff uuid: %v", err)
	}
	staffRedeem := "bc4-" + suffix(t)
	if err := repo.CreateLinkCode(ctx, staffRedeem, mcStaff, "mojang", now.Add(10*time.Minute)); err != nil {
		t.Fatalf("CreateLinkCode (staff redeem): %v", err)
	}
	if _, _, _, err := repo.RedeemPlayerBindCode(ctx, "player-z-"+suffix(t), staffRedeem, now); !errors.Is(err, api.ErrPlayerBindForbidden) {
		t.Fatalf("staff redeem = %v, want ErrPlayerBindForbidden", err)
	}
	assertLinkCodeUnconsumed(t, staffRedeem)

	// Expired: invalid, unconsumed.
	expCode := "bc5-" + suffix(t)
	if err := repo.CreateLinkCode(ctx, expCode, testUUID(t), "mojang", now.Add(-time.Minute)); err != nil {
		t.Fatalf("CreateLinkCode (expired): %v", err)
	}
	if _, _, _, err := repo.RedeemPlayerBindCode(ctx, "player-w-"+suffix(t), expCode, now); !errors.Is(err, api.ErrLinkCodeInvalid) {
		t.Fatalf("expired redeem = %v, want ErrLinkCodeInvalid", err)
	}
	assertLinkCodeUnconsumed(t, expCode)
}

// ---- submissions ---------------------------------------------------------------

func TestSubmitStoreContract(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "sub")
	s := submit.NewPGStore(db)
	now := mustNow()
	id := "sub-" + suffix(t)

	sub := &submit.Submission{
		ID: id, SubmittedBy: u.ID, DisplayName: "pgint pack",
		ContextRef: "s3://bucket/" + id + "/context.tar.gz",
		Status:     submit.StatusPendingReview, CreatedAt: now,
	}
	if err := s.CreateSubmission(ctx, sub); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	// The pending-queue count is the read behind the per-user pending cap: a
	// fresh user starts at zero and a pending row counts.
	if n, err := s.CountPendingSubmissionsBy(ctx, u.ID); err != nil || n != 1 {
		t.Fatalf("CountPendingSubmissionsBy after create = (%d, %v), want (1, nil)", n, err)
	}
	if n, err := s.CountPendingSubmissionsBy(ctx, u.ID+"-nobody"); err != nil || n != 0 {
		t.Fatalf("CountPendingSubmissionsBy for an unknown user = (%d, %v), want (0, nil)", n, err)
	}
	got, err := s.GetSubmission(ctx, id)
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if got.SubmittedBy != u.ID || got.DisplayName != "pgint pack" || got.Status != submit.StatusPendingReview {
		t.Fatalf("round-trip = %+v", got)
	}
	if _, err := s.GetSubmission(ctx, "missing-"+suffix(t)); !errors.Is(err, submit.ErrNotFound) {
		t.Fatalf("missing submission = %v, want ErrNotFound", err)
	}
	byUser, err := s.ListSubmissionsBy(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListSubmissionsBy: %v", err)
	}
	if !containsSubmission(byUser, id) {
		t.Fatal("ListSubmissionsBy must return the caller's submission")
	}
	if all, err := s.ListSubmissions(ctx); err != nil || !containsSubmission(all, id) {
		t.Fatalf("ListSubmissions: (%v, %v), want the submission present", all, err)
	}

	// The upload records its digest; the approval CAS names it, so an approval of
	// a replaced context loses while the row stays pending.
	reviewed, swapped := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if ok, err := s.SetContextDigest(ctx, id, reviewed); err != nil || !ok {
		t.Fatalf("SetContextDigest = (%v, %v), want (true, nil)", ok, err)
	}
	if got, _ := s.GetSubmission(ctx, id); got.ContextSHA256 != reviewed {
		t.Fatalf("context_sha256 = %q, want %q", got.ContextSHA256, reviewed)
	}
	if ok, err := s.ApproveSubmission(ctx, id, "reviewer@example.net", "registry/x:1", swapped, now); err != nil || ok {
		t.Fatalf("approve naming another digest = (%v, %v), want (false, nil)", ok, err)
	}

	// The approval CAS: exactly one winner, and only from pending_review.
	ok, err := s.ApproveSubmission(ctx, id, "reviewer@example.net", "registry.felis.svc:5000/user-uploads/"+id+":latest", reviewed, now)
	if err != nil || !ok {
		t.Fatalf("ApproveSubmission = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := s.ApproveSubmission(ctx, id, "reviewer@example.net", "registry/x:1", reviewed, now); err != nil || ok {
		t.Fatalf("second approve = (%v, %v), want (false, nil)", ok, err)
	}
	// A reviewed row's digest is frozen: a late upload cannot rewrite it.
	if ok, err := s.SetContextDigest(ctx, id, swapped); err != nil || ok {
		t.Fatalf("SetContextDigest after approve = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := s.RejectSubmission(ctx, id, "reviewer@example.net", "no", now); err != nil || ok {
		t.Fatalf("reject after approve = (%v, %v), want (false, nil)", ok, err)
	}
	// A reviewed row leaves the pending queue.
	if n, err := s.CountPendingSubmissionsBy(ctx, u.ID); err != nil || n != 0 {
		t.Fatalf("CountPendingSubmissionsBy after approve = (%d, %v), want (0, nil)", n, err)
	}
	got, _ = s.GetSubmission(ctx, id)
	if got.Status != submit.StatusApproved || got.ImageRef == "" || got.ReviewedBy != "reviewer@example.net" || got.ReviewedAt == nil {
		t.Fatalf("approved row = %+v", got)
	}
	if err := s.LinkBuild(ctx, id, "bld-"+suffix(t)); err != nil {
		t.Fatalf("LinkBuild: %v", err)
	}
	if got, _ = s.GetSubmission(ctx, id); got.BuildID == "" {
		t.Fatal("LinkBuild must persist build_id")
	}

	// Lifecycle: the withdraw CAS deletes ONLY the owner's still-pending row — a
	// wrong owner or a reviewed row can never delete through it — and the admin
	// path deletes any status, exactly once.
	id2 := "sub-w-" + suffix(t)
	if err := s.CreateSubmission(ctx, &submit.Submission{
		ID: id2, SubmittedBy: u.ID, DisplayName: "withdraw me",
		ContextRef: "s3://bucket/" + id2 + "/context.tar.gz",
		Status:     submit.StatusPendingReview, CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSubmission(2): %v", err)
	}
	if ok, err := s.DeletePendingSubmission(ctx, id2, "someone-else"); err != nil || ok {
		t.Fatalf("withdraw by a non-owner = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := s.DeletePendingSubmission(ctx, id, u.ID); err != nil || ok {
		t.Fatalf("withdraw of a reviewed row = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := s.DeletePendingSubmission(ctx, id2, u.ID); err != nil || !ok {
		t.Fatalf("withdraw by the owner = (%v, %v), want (true, nil)", ok, err)
	}
	if _, err := s.GetSubmission(ctx, id2); !errors.Is(err, submit.ErrNotFound) {
		t.Fatalf("withdrawn row still readable: %v", err)
	}
	if ok, err := s.DeleteSubmission(ctx, id); err != nil || !ok {
		t.Fatalf("admin delete = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := s.DeleteSubmission(ctx, id); err != nil || ok {
		t.Fatalf("second admin delete = (%v, %v), want (false, nil)", ok, err)
	}
}

// ---- builds --------------------------------------------------------------------

func TestBuildStoreContract(t *testing.T) {
	ctx := context.Background()
	s := build.NewPGStore(db)
	now := mustNow()

	done := "bld-done-" + suffix(t)
	digest := strings.Repeat("c", 64)
	if err := s.CreateBuild(ctx, &build.Build{ID: done, ImageRef: "registry.felis.svc:5000/user-uploads/" + done + ":latest",
		Status: build.StatusPending, RequestedBy: "pgint", Dockerfile: "FROM scratch\n", ContextRef: "http://api/x",
		ContextDigest: digest}); err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	got, err := s.GetBuild(ctx, done)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != build.StatusPending || got.JobName != "" || got.ContextDigest != digest {
		t.Fatalf("fresh build = %+v", got)
	}
	if err := s.SetBuildJob(ctx, done, "job-"+suffix(t)); err != nil {
		t.Fatalf("SetBuildJob: %v", err)
	}
	got, _ = s.GetBuild(ctx, done)
	if got.JobName == "" || got.Status != build.StatusBuilding {
		t.Fatalf("SetBuildJob must record the job and advance to building, got %+v", got)
	}
	if err := s.FinishBuild(ctx, done, build.StatusSucceeded, "", now); err != nil {
		t.Fatalf("FinishBuild: %v", err)
	}
	got, _ = s.GetBuild(ctx, done)
	if got.Status != build.StatusSucceeded || got.FinishedAt == nil {
		t.Fatalf("finished build = %+v", got)
	}
	if _, err := s.GetBuild(ctx, "missing-"+suffix(t)); !errors.Is(err, build.ErrNotFound) {
		t.Fatalf("missing build = %v, want ErrNotFound", err)
	}

	running := "bld-run-" + suffix(t)
	if err := s.CreateBuild(ctx, &build.Build{ID: running, ImageRef: "registry.felis.svc:5000/user-uploads/" + running + ":latest",
		Status: build.StatusPending, RequestedBy: "pgint"}); err != nil {
		t.Fatalf("CreateBuild (running): %v", err)
	}
	unfinished, err := s.ListUnfinishedBuilds(ctx)
	if err != nil {
		t.Fatalf("ListUnfinishedBuilds: %v", err)
	}
	var sawRunning, sawDone bool
	for _, b := range unfinished {
		sawRunning = sawRunning || b.ID == running
		sawDone = sawDone || b.ID == done
	}
	if !sawRunning || sawDone {
		t.Fatalf("ListUnfinishedBuilds: running=%v finished=%v, want true/false", sawRunning, sawDone)
	}

	// Image admission round trip (the scan-gate success path + external admit).
	builtRef := "registry.felis.svc:5000/user-uploads/" + running + ":latest"
	if err := s.AdmitBuiltImage(ctx, build.Image{ImageRef: builtRef, BuildID: running, AddedBy: "felis-api"}); err != nil {
		t.Fatalf("AdmitBuiltImage: %v", err)
	}
	images, err := s.ListImages(ctx)
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if img, ok := findImage(images, builtRef); !ok || !img.Enabled || img.Source != "built" {
		t.Fatalf("admitted image = %+v (found=%v), want enabled built", img, ok)
	}
	if err := s.RemoveImage(ctx, builtRef); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	if images, _ = s.ListImages(ctx); hasImage(images, builtRef) {
		t.Fatal("RemoveImage must drop the row")
	}
	if err := s.RemoveImage(ctx, builtRef); !errors.Is(err, build.ErrNotFound) {
		t.Fatalf("remove twice = %v, want ErrNotFound", err)
	}
	extRef := "registry.felis.svc:5000/mirror/ext:1"
	if err := s.AddExternalImage(ctx, build.Image{ImageRef: extRef, AddedBy: "admin@example.net"}); err != nil {
		t.Fatalf("AddExternalImage: %v", err)
	}
	if images, _ = s.ListImages(ctx); !hasImage(images, extRef) {
		t.Fatal("AddExternalImage must record the image")
	}
}

// ---- helpers -------------------------------------------------------------------

func assertAttempts(t *testing.T, userID, purpose string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT attempts FROM email_otps WHERE user_id=$1 AND purpose=$2 AND consumed_at IS NULL ORDER BY created_at DESC LIMIT 1`,
		userID, purpose).Scan(&got); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if got != want {
		t.Fatalf("attempts = %d, want %d", got, want)
	}
}

func assertConsumed(t *testing.T, userID, purpose string, want bool) {
	t.Helper()
	var consumed bool
	if err := db.QueryRow(`SELECT consumed_at IS NOT NULL FROM email_otps WHERE user_id=$1 AND purpose=$2 ORDER BY created_at DESC LIMIT 1`,
		userID, purpose).Scan(&consumed); err != nil {
		t.Fatalf("read consumed: %v", err)
	}
	if consumed != want {
		t.Fatalf("consumed = %v, want %v", consumed, want)
	}
}

func assertEmailProven(t *testing.T, userID, wantEmail string, wantVerified bool) {
	t.Helper()
	var email string
	var verified bool
	if err := db.QueryRow(`SELECT COALESCE(email,''), COALESCE(email_verified,false) FROM users WHERE id=$1`, userID).
		Scan(&email, &verified); err != nil {
		t.Fatalf("read user email: %v", err)
	}
	if email != wantEmail || verified != wantVerified {
		t.Fatalf("user email = (%q, %v), want (%q, %v)", email, verified, wantEmail, wantVerified)
	}
}

func userRole(t *testing.T, userID string) string {
	t.Helper()
	var role string
	if err := db.QueryRow(`SELECT role::text FROM users WHERE id = $1`, userID).Scan(&role); err != nil {
		t.Fatalf("read user role: %v", err)
	}
	return role
}

func assertLinkAuthSource(t *testing.T, userID, mcUUID, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(`SELECT auth_source::text FROM account_links WHERE user_id=$1 AND mc_uuid=$2`,
		userID, mcUUID).Scan(&got); err != nil {
		t.Fatalf("read link auth_source: %v", err)
	}
	if got != want {
		t.Fatalf("auth_source = %q, want %q", got, want)
	}
}

// assertLinkCodeUnconsumed fails when the code row is gone. Consumption is a
// DELETE of the row (there is no consumed flag on account_link_codes), so mere
// row presence IS the "still redeemable" observable — the staff-redeem path
// must leave a pending code untouched.
func assertLinkCodeUnconsumed(t *testing.T, code string) {
	t.Helper()
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM account_link_codes WHERE code=$1)`, code).Scan(&exists); err != nil {
		t.Fatalf("read link code: %v", err)
	}
	if !exists {
		t.Fatal("link code row is gone but the contract says it must survive")
	}
}

func containsSubmission(subs []submit.Submission, id string) bool {
	for _, s := range subs {
		if s.ID == id {
			return true
		}
	}
	return false
}

func findImage(images []build.Image, ref string) (build.Image, bool) {
	for _, img := range images {
		if img.ImageRef == ref {
			return img, true
		}
	}
	return build.Image{}, false
}

func hasImage(images []build.Image, ref string) bool {
	_, ok := findImage(images, ref)
	return ok
}
