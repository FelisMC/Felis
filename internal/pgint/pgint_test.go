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

	// The approval CAS: exactly one winner, and only from pending_review.
	ok, err := s.ApproveSubmission(ctx, id, "reviewer@example.net", "registry.felis.svc:5000/user-uploads/"+id+":latest", now)
	if err != nil || !ok {
		t.Fatalf("ApproveSubmission = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := s.ApproveSubmission(ctx, id, "reviewer@example.net", "registry/x:1", now); err != nil || ok {
		t.Fatalf("second approve = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := s.RejectSubmission(ctx, id, "reviewer@example.net", "no", now); err != nil || ok {
		t.Fatalf("reject after approve = (%v, %v), want (false, nil)", ok, err)
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
}

// ---- builds --------------------------------------------------------------------

func TestBuildStoreContract(t *testing.T) {
	ctx := context.Background()
	s := build.NewPGStore(db)
	now := mustNow()

	done := "bld-done-" + suffix(t)
	if err := s.CreateBuild(ctx, &build.Build{ID: done, ImageRef: "registry.felis.svc:5000/user-uploads/" + done + ":latest",
		Status: build.StatusPending, RequestedBy: "pgint", Dockerfile: "FROM scratch\n", ContextRef: "http://api/x"}); err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	got, err := s.GetBuild(ctx, done)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got.Status != build.StatusPending || got.JobName != "" {
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
