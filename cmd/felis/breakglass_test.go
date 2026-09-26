package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
)

// fakeOwnerStore records what break-glass / setup provisioning writes and answers
// the identity lookups, so the core logic (admin resolution, provisioning,
// accountability audit, setup-token mint) is exercised without a database or a
// terminal. The design is passwordless: accounts carry no credential, and the
// Owner completes first-login through the setup-token web flow.
type fakeOwnerStore struct {
	upserts   []upsertCall
	inserts   []upsertCall
	settings  map[string][]byte
	audits    []api.AuditEntry
	tokens    []setupTokenCall
	redeems   []redeemCall
	users     map[string]*api.StaffUser // keyed by username
	admins    bool                      // AdminExists answer
	ownerSeat string                    // OwnerUsername answer: the occupied seat, "" when none

	// CompleteOwnerSetup's success result. redeemUserID defaults to the fresh id
	// the caller passes (the unlinked-UUID case) when left empty.
	redeemUserID     string
	redeemMCUUID     string
	redeemAuthSource string

	upsertErr      error
	insertErr      error
	setErr         error
	auditErr       error
	userErr        error // non-not-found error from UserByUsername
	adminErr       error
	seatErr        error
	redeemErr      error
	createTokenErr error
}

// upsertCall is a recorded owner/operator provision. Passwordless: the row is pure
// identity (id, username, email) with an implied role=admin.
type upsertCall struct {
	id, username, email string
}

// setupTokenCall is a recorded setup-token write. Only the hash is persisted.
type setupTokenCall struct {
	tokenHash string
	userID    string
	expiresAt time.Time
}

// redeemCall records the inputs CompleteOwnerSetup was called with.
type redeemCall struct {
	newUserID string
	code      string
}

func (f *fakeOwnerStore) AdminExists(_ context.Context) (bool, error) {
	if f.adminErr != nil {
		return false, f.adminErr
	}
	return f.admins, nil
}

func (f *fakeOwnerStore) UserByUsername(_ context.Context, username string) (*api.StaffUser, error) {
	if f.userErr != nil {
		return nil, f.userErr
	}
	if u, ok := f.users[username]; ok {
		return u, nil
	}
	return nil, api.ErrNotFound
}

// OwnerUsername reports the single active Owner seat. Tests set ownerSeat; the
// zero value models a fresh install where bootstrap is free to mint.
func (f *fakeOwnerStore) OwnerUsername(_ context.Context) (string, error) {
	if f.seatErr != nil {
		return "", f.seatErr
	}
	return f.ownerSeat, nil
}

func (f *fakeOwnerStore) UpsertOwner(_ context.Context, id, username, email string) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, upsertCall{id, username, email})
	return nil
}

// InsertOperator records an insert-only Operator provision. A username already in
// the users map is a conflict (api.ErrConflict), mirroring the PGRepo insert-only
// contract; a fresh one is recorded and reflected into users so a later lookup — or
// a second insert of the same name — sees it. The row is passwordless (role=admin).
func (f *fakeOwnerStore) InsertOperator(_ context.Context, id, username, email string) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	if _, taken := f.users[username]; taken {
		return api.ErrConflict
	}
	f.inserts = append(f.inserts, upsertCall{id, username, email})
	if f.users == nil {
		f.users = map[string]*api.StaffUser{}
	}
	f.users[username] = &api.StaffUser{ID: id, Username: username, Email: email, Role: "admin"}
	return nil
}

// CompleteOwnerSetup models the real all-or-nothing transaction: injected failures
// record none of the redeem, auth-toggle, or setup-token writes.
func (f *fakeOwnerStore) CompleteOwnerSetup(_ context.Context, newUserID, code string, _ time.Time,
	tokenHash string, expiresAt time.Time) (string, string, string, error) {
	if f.redeemErr != nil {
		return "", "", "", f.redeemErr
	}
	if f.setErr != nil {
		return "", "", "", f.setErr
	}
	if f.createTokenErr != nil {
		return "", "", "", f.createTokenErr
	}
	f.redeems = append(f.redeems, redeemCall{newUserID, code})
	userID := f.redeemUserID
	if userID == "" {
		userID = newUserID // unlinked UUID → the fresh id becomes the Owner
	}
	if f.settings == nil {
		f.settings = map[string][]byte{}
	}
	f.settings[api.LocalAuthEnabledKey] = []byte("true")
	f.tokens = append(f.tokens, setupTokenCall{tokenHash, userID, expiresAt})
	return userID, f.redeemMCUUID, f.redeemAuthSource, nil
}

func (f *fakeOwnerStore) SetSetting(_ context.Context, key string, value []byte) error {
	if f.setErr != nil {
		return f.setErr
	}
	if f.settings == nil {
		f.settings = map[string][]byte{}
	}
	f.settings[key] = value
	return nil
}

func (f *fakeOwnerStore) Audit(_ context.Context, e api.AuditEntry) error {
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audits = append(f.audits, e)
	return nil
}

// mkAdmin builds a resolvable staff row (role=admin). The design is passwordless,
// so a staff account is identity + role — there is no credential to attach.
func mkAdmin(username string) *api.StaffUser {
	return &api.StaffUser{ID: "usr-admin", Username: username, Role: "admin"}
}

func TestProvisionOwner(t *testing.T) {
	ctx := context.Background()

	t.Run("mints a passwordless owner row", func(t *testing.T) {
		f := &fakeOwnerStore{}
		if err := provisionOwner(ctx, f, "owner", "me@example.com"); err != nil {
			t.Fatalf("provisionOwner: %v", err)
		}
		if len(f.upserts) != 1 {
			t.Fatalf("want 1 upsert, got %d", len(f.upserts))
		}
		got := f.upserts[0]
		if got.username != "owner" {
			t.Errorf("username = %q, want owner", got.username)
		}
		if got.email != "me@example.com" {
			t.Errorf("email = %q, want me@example.com", got.email)
		}
		if !strings.HasPrefix(got.id, "usr-") {
			t.Errorf("id = %q, want usr- prefix", got.id)
		}
	})

	t.Run("trims surrounding whitespace", func(t *testing.T) {
		f := &fakeOwnerStore{}
		if err := provisionOwner(ctx, f, "  owner  ", "  e@x.io  "); err != nil {
			t.Fatalf("provisionOwner: %v", err)
		}
		if f.upserts[0].username != "owner" || f.upserts[0].email != "e@x.io" {
			t.Errorf("got username=%q email=%q, want trimmed", f.upserts[0].username, f.upserts[0].email)
		}
	})

	t.Run("rejects an empty username before any write", func(t *testing.T) {
		f := &fakeOwnerStore{}
		if err := provisionOwner(ctx, f, "   ", ""); err == nil {
			t.Fatal("want error for empty username")
		}
		if len(f.upserts) != 0 {
			t.Errorf("want no upsert on validation failure, got %d", len(f.upserts))
		}
	})

	t.Run("an occupied seat refuses any other username", func(t *testing.T) {
		// The seat is the single owner row: upserting a fresh name would take the
		// insert arm and mint a SECOND owner, while the existing seat — possibly the
		// compromised account this reset was meant to replace — stays live, and no
		// supported path can delete an owner row.
		f := &fakeOwnerStore{ownerSeat: "seat-holder"}
		err := provisionOwner(ctx, f, "someone-else", "")
		if !errors.Is(err, api.ErrConflict) {
			t.Fatalf("error = %v, want it to wrap api.ErrConflict so the TUI routes back to the form", err)
		}
		if !strings.Contains(err.Error(), `"seat-holder"`) {
			t.Errorf("error = %q, want it to name the occupied seat", err)
		}
		if len(f.upserts) != 0 {
			t.Errorf("want no write against an occupied seat, got %d", len(f.upserts))
		}
	})

	t.Run("the occupied seat's own username still resets", func(t *testing.T) {
		f := &fakeOwnerStore{ownerSeat: "seat-holder"}
		if err := provisionOwner(ctx, f, "seat-holder", "new@example.net"); err != nil {
			t.Fatalf("provisionOwner(reset): %v", err)
		}
		if len(f.upserts) != 1 || f.upserts[0].username != "seat-holder" || f.upserts[0].email != "new@example.net" {
			t.Fatalf("want 1 reset upsert for the seat, got %+v", f.upserts)
		}
	})

	t.Run("propagates a store error", func(t *testing.T) {
		f := &fakeOwnerStore{upsertErr: errors.New("boom")}
		if err := provisionOwner(ctx, f, "owner", ""); err == nil {
			t.Fatal("want error when the store fails")
		}
	})
}

func TestEnableLocalAuth(t *testing.T) {
	f := &fakeOwnerStore{}
	if err := enableLocalAuth(context.Background(), f); err != nil {
		t.Fatalf("enableLocalAuth: %v", err)
	}
	raw, ok := f.settings[api.LocalAuthEnabledKey]
	if !ok {
		t.Fatalf("setting %q not written", api.LocalAuthEnabledKey)
	}
	// Mirror how the live API parses the toggle (session.go localAuthEnabled): the
	// stored jsonb must round-trip to the bool true, or the gate fails closed and the
	// Owner can never log in.
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		t.Fatalf("setting value %q is not valid JSON: %v", raw, err)
	}
	if !enabled {
		t.Errorf("local_auth_enabled = false, want true")
	}
}

func TestResolveAdmin(t *testing.T) {
	ctx := context.Background()

	// resolveAdmin only finds the staff account a typed name points at; proving the
	// operator holds it is the mailed code's job (beginRecovery).

	t.Run("resolves an existing admin", func(t *testing.T) {
		f := &fakeOwnerStore{users: map[string]*api.StaffUser{"root": mkAdmin("root")}}
		u, err := resolveAdmin(ctx, f, "  root ")
		if err != nil {
			t.Fatalf("resolveAdmin: %v", err)
		}
		if u == nil || u.Username != "root" {
			t.Fatalf("resolveAdmin = %+v, want the root admin", u)
		}
	})

	t.Run("a non-admin role is no staff account", func(t *testing.T) {
		player := mkAdmin("alice")
		player.Role = "user" // a player row is not staff
		f := &fakeOwnerStore{users: map[string]*api.StaffUser{"alice": player}}
		if u, err := resolveAdmin(ctx, f, "alice"); u != nil || err != nil {
			t.Errorf("resolveAdmin(player) = (%+v, %v), want (nil, nil)", u, err)
		}
	})

	t.Run("the owner role counts as staff", func(t *testing.T) {
		owner := mkAdmin("root")
		owner.Role = "owner" // the platform owner is staff too (migration 0011)
		f := &fakeOwnerStore{users: map[string]*api.StaffUser{"root": owner}}
		if u, err := resolveAdmin(ctx, f, "root"); err != nil || u == nil {
			t.Fatalf("resolveAdmin(owner) = (%+v, %v), want the owner", u, err)
		}
	})

	t.Run("an unknown user is nil, not an error", func(t *testing.T) {
		if u, err := resolveAdmin(ctx, &fakeOwnerStore{}, "nobody"); u != nil || err != nil {
			t.Errorf("resolveAdmin(unknown) = (%+v, %v), want (nil, nil)", u, err)
		}
	})

	t.Run("an empty username makes no store call", func(t *testing.T) {
		f := &fakeOwnerStore{userErr: errors.New("must not be called")}
		if u, err := resolveAdmin(ctx, f, " "); u != nil || err != nil {
			t.Errorf("empty username: (%+v, %v), want (nil, nil)", u, err)
		}
	})

	t.Run("a datastore fault is surfaced", func(t *testing.T) {
		f := &fakeOwnerStore{userErr: errors.New("db down")}
		if _, err := resolveAdmin(ctx, f, "root"); err == nil {
			t.Fatal("want error when the store fails")
		}
	})
}

// auditOf decodes the single recorded audit row's payload for assertions.
func auditOf(t *testing.T, f *fakeOwnerStore) (api.AuditEntry, map[string]any) {
	t.Helper()
	if len(f.audits) != 1 {
		t.Fatalf("want exactly 1 audit row, got %d", len(f.audits))
	}
	e := f.audits[0]
	var payload map[string]any
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("audit payload is not valid JSON: %v", err)
	}
	return e, payload
}

func TestPerformBreakGlass(t *testing.T) {
	ctx := context.Background()

	t.Run("bootstrap provisions the owner, enables local auth, and audits", func(t *testing.T) {
		f := &fakeOwnerStore{}
		op := breakGlassOp{
			mode:          "bootstrap",
			accountable:   "deploybot",
			osUser:        "deploybot",
			ownerUsername: "owner",
			ownerEmail:    "owner@example.com",
		}
		out, err := performBreakGlass(ctx, f, op)
		if err != nil {
			t.Fatalf("performBreakGlass: %v", err)
		}
		if out.auditErr != nil {
			t.Errorf("auditErr = %v, want nil", out.auditErr)
		}
		if len(f.upserts) != 1 || f.upserts[0].username != "owner" {
			t.Errorf("owner was not provisioned: %+v", f.upserts)
		}
		if _, ok := f.settings[api.LocalAuthEnabledKey]; !ok {
			t.Error("local auth was not enabled — login would 403")
		}
		e, payload := auditOf(t, f)
		if e.Actor != "deploybot" || e.Source != "break-glass" || e.Action != "break_glass.bootstrap" {
			t.Errorf("audit envelope = %+v, want actor=deploybot source=break-glass action=break_glass.bootstrap", e)
		}
		if payload["verified"] != false {
			t.Errorf("payload.verified = %v, want false for bootstrap", payload["verified"])
		}
		if _, present := payload["admin_account"]; present {
			t.Error("payload.admin_account present, want omitted when no admin was attempted")
		}
		if payload["os_user"] != "deploybot" || payload["owner"] != "owner" {
			t.Errorf("payload = %v, want os_user=deploybot owner=owner", payload)
		}
	})

	t.Run("recovery provisions the owner and records a verified row", func(t *testing.T) {
		f := &fakeOwnerStore{}
		op := breakGlassOp{
			mode:           "recovery",
			accountable:    "root",
			osUser:         "alice",
			ownerUsername:  "owner",
			attemptedAdmin: "root",
			verifiedBy:     verifiedByEmailOTP,
			codeSentTo:     "root@example.com",
		}
		out, err := performBreakGlass(ctx, f, op)
		if err != nil {
			t.Fatalf("performBreakGlass: %v", err)
		}
		if out.auditErr != nil {
			t.Errorf("auditErr = %v, want nil", out.auditErr)
		}
		if len(f.upserts) != 1 {
			t.Fatalf("want 1 upsert, got %d", len(f.upserts))
		}
		if _, ok := f.settings[api.LocalAuthEnabledKey]; !ok {
			t.Error("local auth was not enabled")
		}
		e, payload := auditOf(t, f)
		if e.Actor != "root" || e.Action != "break_glass.recovery" {
			t.Errorf("audit envelope = %+v, want actor=root action=break_glass.recovery", e)
		}
		if payload["verified"] != true {
			t.Errorf("payload.verified = %v, want true for recovery", payload["verified"])
		}
		if payload["admin_account"] != "root" {
			t.Errorf("payload.admin_account = %v, want root", payload["admin_account"])
		}
		if payload["verified_by"] != verifiedByEmailOTP || payload["code_sent_to"] != "root@example.com" {
			t.Errorf("payload = %v, want verified_by=email_otp code_sent_to=root@example.com", payload)
		}
		if _, present := payload["otp_skipped"]; present {
			t.Error("a proven recovery carries no otp_skipped")
		}
	})

	t.Run("recovery without a proof is recorded unverified", func(t *testing.T) {
		f := &fakeOwnerStore{}
		op := breakGlassOp{mode: "recovery", accountable: "root", osUser: "alice", ownerUsername: "owner", attemptedAdmin: "root"}
		if _, err := performBreakGlass(ctx, f, op); err != nil {
			t.Fatalf("performBreakGlass: %v", err)
		}
		if _, payload := auditOf(t, f); payload["verified"] != false {
			t.Errorf("payload.verified = %v, want false: only a mailed code verifies", payload["verified"])
		}
	})

	t.Run("root override records an unverified row attributed to the OS user", func(t *testing.T) {
		f := &fakeOwnerStore{}
		op := breakGlassOp{
			mode:           "root_override",
			accountable:    "alice",
			osUser:         "alice",
			ownerUsername:  "owner",
			attemptedAdmin: "typo-admin",
			otpSkipped:     otpSkipSendFailed,
			otpSkipDetail:  "dial tcp: connection refused",
		}
		if _, err := performBreakGlass(ctx, f, op); err != nil {
			t.Fatalf("performBreakGlass: %v", err)
		}
		e, payload := auditOf(t, f)
		if e.Actor != "alice" || e.Action != "break_glass.root_override" {
			t.Errorf("audit envelope = %+v, want actor=alice action=break_glass.root_override", e)
		}
		if payload["verified"] != false {
			t.Errorf("payload.verified = %v, want false for root override", payload["verified"])
		}
		// The attempted (failed) admin is preserved so the trail shows what was tried.
		if payload["admin_account"] != "typo-admin" {
			t.Errorf("payload.admin_account = %v, want typo-admin", payload["admin_account"])
		}
		// Why no code proved anyone is part of the record.
		if payload["otp_skipped"] != otpSkipSendFailed || payload["otp_skip_detail"] != "dial tcp: connection refused" {
			t.Errorf("payload = %v, want otp_skipped=send_failed with its detail", payload)
		}
		if _, present := payload["verified_by"]; present {
			t.Error("an override carries no verified_by")
		}
	})

	t.Run("an audit failure does not fail the recovery", func(t *testing.T) {
		f := &fakeOwnerStore{auditErr: errors.New("audit sink down")}
		op := breakGlassOp{mode: "recovery", accountable: "root", osUser: "alice", ownerUsername: "owner", attemptedAdmin: "root"}
		out, err := performBreakGlass(ctx, f, op)
		if err != nil {
			t.Fatalf("performBreakGlass returned %v, want nil — break-glass must survive a dead audit sink", err)
		}
		if out.auditErr == nil {
			t.Error("auditErr = nil, want the surfaced audit failure")
		}
		// The load-bearing writes must still have happened.
		if len(f.upserts) != 1 {
			t.Error("owner was not provisioned despite a recoverable audit failure")
		}
		if _, ok := f.settings[api.LocalAuthEnabledKey]; !ok {
			t.Error("local auth was not enabled despite a recoverable audit failure")
		}
	})

	t.Run("does not enable local auth or audit if the owner write fails", func(t *testing.T) {
		f := &fakeOwnerStore{upsertErr: errors.New("boom")}
		op := breakGlassOp{mode: "bootstrap", accountable: "root", osUser: "root", ownerUsername: "owner"}
		if _, err := performBreakGlass(ctx, f, op); err == nil {
			t.Fatal("want error when the owner write fails")
		}
		if len(f.settings) != 0 {
			t.Error("local auth should not be enabled when provisioning failed")
		}
		if len(f.audits) != 0 {
			t.Error("no audit row should be written when provisioning failed")
		}
	})

	t.Run("records accountability before enabling local auth, surviving an enableLocalAuth failure", func(t *testing.T) {
		// The Owner is written by provisionOwner; if the audit were written only after
		// enableLocalAuth, a failed toggle write would leave that write with no "who did
		// it" row. Order guarantees the accountability row lands first.
		f := &fakeOwnerStore{setErr: errors.New("settings write down")}
		op := breakGlassOp{mode: "recovery", accountable: "root", osUser: "alice", ownerUsername: "owner", attemptedAdmin: "root"}
		if _, err := performBreakGlass(ctx, f, op); err == nil {
			t.Fatal("want error when enableLocalAuth fails")
		}
		if len(f.upserts) != 1 {
			t.Error("owner should have been provisioned before the toggle write failed")
		}
		if len(f.audits) != 1 {
			t.Fatalf("accountability row count = %d, want 1 — the audit must precede enableLocalAuth", len(f.audits))
		}
		if f.audits[0].Actor != "root" || f.audits[0].Action != "break_glass.recovery" {
			t.Errorf("audit = %+v, want actor=root action=break_glass.recovery", f.audits[0])
		}
	})
}

func TestAccountableOSUser(t *testing.T) {
	t.Run("prefers SUDO_USER", func(t *testing.T) {
		t.Setenv("SUDO_USER", "alice")
		if got := accountableOSUser(); got != "alice" {
			t.Errorf("accountableOSUser() = %q, want alice", got)
		}
	})
	t.Run("falls back to root when SUDO_USER is unset", func(t *testing.T) {
		t.Setenv("SUDO_USER", "")
		if got := accountableOSUser(); got != "root" {
			t.Errorf("accountableOSUser() = %q, want root", got)
		}
	})
}

func TestProvisionOperator(t *testing.T) {
	ctx := context.Background()

	t.Run("mints a passwordless operator row (insert-only)", func(t *testing.T) {
		f := &fakeOwnerStore{}
		if err := provisionOperator(ctx, f, "ops-jordan", "jordan@example.com"); err != nil {
			t.Fatalf("provisionOperator: %v", err)
		}
		// Insert-only: it records an insert and never touches the Owner upsert path.
		if len(f.upserts) != 0 {
			t.Errorf("want 0 owner upserts, got %d — operator-add must not use the Owner path", len(f.upserts))
		}
		if len(f.inserts) != 1 {
			t.Fatalf("want 1 insert, got %d", len(f.inserts))
		}
		got := f.inserts[0]
		if got.username != "ops-jordan" {
			t.Errorf("username = %q, want ops-jordan", got.username)
		}
		if got.email != "jordan@example.com" {
			t.Errorf("email = %q, want jordan@example.com", got.email)
		}
		if !strings.HasPrefix(got.id, "usr-") {
			t.Errorf("id = %q, want usr- prefix", got.id)
		}
	})

	t.Run("a taken username is a conflict, not a silent reset", func(t *testing.T) {
		// The Owner already holds this username. Operator-add must refuse rather than
		// overwrite it the way UpsertOwner would.
		f := &fakeOwnerStore{users: map[string]*api.StaffUser{
			"owner": {ID: "usr-owner", Username: "owner", Role: "admin"},
		}}
		err := provisionOperator(ctx, f, "owner", "")
		if err == nil {
			t.Fatal("want error when the username is already taken")
		}
		// The sentinel must remain matchable so the TUI can render "name already taken".
		if !errors.Is(err, api.ErrConflict) {
			t.Errorf("error = %v, want it to wrap api.ErrConflict", err)
		}
		if len(f.inserts) != 0 {
			t.Errorf("want no insert on conflict, got %d", len(f.inserts))
		}
		// The pre-existing account must be untouched.
		if f.users["owner"].ID != "usr-owner" {
			t.Error("conflicting insert clobbered the existing account")
		}
	})

	t.Run("trims surrounding whitespace", func(t *testing.T) {
		f := &fakeOwnerStore{}
		if err := provisionOperator(ctx, f, "  ops  ", "  e@x.io  "); err != nil {
			t.Fatalf("provisionOperator: %v", err)
		}
		if f.inserts[0].username != "ops" || f.inserts[0].email != "e@x.io" {
			t.Errorf("got username=%q email=%q, want trimmed", f.inserts[0].username, f.inserts[0].email)
		}
	})

	t.Run("rejects an empty username before any write", func(t *testing.T) {
		f := &fakeOwnerStore{}
		if err := provisionOperator(ctx, f, "   ", ""); err == nil {
			t.Fatal("want error for empty username")
		}
		if len(f.inserts) != 0 {
			t.Errorf("want no insert on validation failure, got %d", len(f.inserts))
		}
	})

	t.Run("propagates a non-conflict store error without mislabeling it", func(t *testing.T) {
		f := &fakeOwnerStore{insertErr: errors.New("boom")}
		err := provisionOperator(ctx, f, "ops", "")
		if err == nil {
			t.Fatal("want error when the store fails")
		}
		// A generic store fault must NOT be mistaken for a username conflict.
		if errors.Is(err, api.ErrConflict) {
			t.Error("a generic store error was misreported as a conflict")
		}
	})
}

func TestPerformAddOperator(t *testing.T) {
	ctx := context.Background()

	t.Run("provisions an operator, audits, and never flips local auth", func(t *testing.T) {
		f := &fakeOwnerStore{}
		op := breakGlassOp{
			mode:           "recovery",
			accountable:    "root",
			osUser:         "alice",
			ownerUsername:  "ops-jordan",
			ownerEmail:     "jordan@example.com",
			attemptedAdmin: "root",
			verifiedBy:     verifiedByEmailOTP,
			codeSentTo:     "root@example.com",
		}
		out, err := performAddOperator(ctx, f, op)
		if err != nil {
			t.Fatalf("performAddOperator: %v", err)
		}
		if out.auditErr != nil {
			t.Errorf("auditErr = %v, want nil", out.auditErr)
		}
		if len(f.inserts) != 1 || f.inserts[0].username != "ops-jordan" {
			t.Errorf("operator was not provisioned: %+v", f.inserts)
		}
		// Adding an Operator must NOT flip the global local-auth gate (Owner-only).
		if _, ok := f.settings[api.LocalAuthEnabledKey]; ok {
			t.Error("local auth was enabled — operator-add must not touch the global gate")
		}
		e, payload := auditOf(t, f)
		if e.Actor != "root" || e.Source != "break-glass" || e.Action != "break_glass.operator_create" {
			t.Errorf("audit envelope = %+v, want actor=root source=break-glass action=break_glass.operator_create", e)
		}
		// The new account is recorded under "operator", not "owner".
		if payload["operator"] != "ops-jordan" {
			t.Errorf("payload.operator = %v, want ops-jordan", payload["operator"])
		}
		if _, present := payload["owner"]; present {
			t.Error("payload.owner present, want the new account under the operator key")
		}
		if payload["verified"] != true || payload["admin_account"] != "root" || payload["verified_by"] != verifiedByEmailOTP {
			t.Errorf("payload = %v, want verified=true admin_account=root verified_by=email_otp", payload)
		}
	})

	t.Run("root override records an unverified operator row", func(t *testing.T) {
		f := &fakeOwnerStore{}
		op := breakGlassOp{mode: "root_override", accountable: "alice", osUser: "alice", ownerUsername: "ops", attemptedAdmin: "typo-admin", otpSkipped: otpSkipUnknownAdmin}
		if _, err := performAddOperator(ctx, f, op); err != nil {
			t.Fatalf("performAddOperator: %v", err)
		}
		if len(f.inserts) != 1 {
			t.Fatalf("want 1 insert, got %d", len(f.inserts))
		}
		_, payload := auditOf(t, f)
		if payload["verified"] != false || payload["otp_skipped"] != otpSkipUnknownAdmin {
			t.Errorf("payload = %v, want verified=false otp_skipped=unknown_admin", payload)
		}
	})

	t.Run("an audit failure does not fail the operator-add", func(t *testing.T) {
		f := &fakeOwnerStore{auditErr: errors.New("audit sink down")}
		op := breakGlassOp{mode: "recovery", accountable: "root", osUser: "alice", ownerUsername: "ops", attemptedAdmin: "root"}
		out, err := performAddOperator(ctx, f, op)
		if err != nil {
			t.Fatalf("performAddOperator returned %v, want nil — a dead audit sink must not fail the add", err)
		}
		if out.auditErr == nil {
			t.Error("auditErr = nil, want the surfaced audit failure")
		}
		if len(f.inserts) != 1 {
			t.Error("operator was not provisioned despite a recoverable audit failure")
		}
	})

	t.Run("a conflict mints nothing and writes no audit row", func(t *testing.T) {
		f := &fakeOwnerStore{users: map[string]*api.StaffUser{
			"owner": {ID: "usr-owner", Username: "owner", Role: "admin"},
		}}
		op := breakGlassOp{mode: "recovery", accountable: "root", osUser: "alice", ownerUsername: "owner", attemptedAdmin: "root"}
		if _, err := performAddOperator(ctx, f, op); err == nil {
			t.Fatal("want error when the operator username is already taken")
		}
		if len(f.inserts) != 0 {
			t.Error("a conflicting add should mint nothing")
		}
		if len(f.audits) != 0 {
			t.Error("a conflicting add should write no audit row")
		}
	})
}

func TestPerformSetupMCBind(t *testing.T) {
	ctx := context.Background()

	t.Run("binds the owner and mints a setup URL whose token hash is what is stored", func(t *testing.T) {
		f := &fakeOwnerStore{redeemUserID: "usr-owner-1", redeemMCUUID: "mc-uuid-1", redeemAuthSource: "mojang"}
		out, err := performSetupMCBind(ctx, f, "  abc-123  ", "console.example.com", "deploybot")
		if err != nil {
			t.Fatalf("performSetupMCBind: %v", err)
		}
		// The Owner this mints has no password and no email, so the setup token is the
		// only door — and handleSetupRedeem is gated on local_auth_enabled. A bind that
		// leaves the toggle off hands back a URL that answers 403.
		if _, ok := f.settings[api.LocalAuthEnabledKey]; !ok {
			t.Error("local auth was not enabled — the setup URL would 403 local_auth_disabled")
		}
		// The bind is attributed by Minecraft identity, because that is what the login
		// gate verified; a username would be the one thing nobody checked.
		e, payload := auditOf(t, f)
		if e.Actor != "deploybot" || e.Source != "setup" || e.Action != "setup.owner_bind" {
			t.Errorf("audit = %+v, want actor=deploybot source=setup action=setup.owner_bind", e)
		}
		if payload["mc_uuid"] != "mc-uuid-1" || payload["auth_source"] != "mojang" {
			t.Errorf("audit payload = %v, want the redeemed mc_uuid + auth_source", payload)
		}
		if out.ownerIdentity != "mc-uuid-1" {
			t.Errorf("owner identity = %q, want the verified Minecraft UUID", out.ownerIdentity)
		}
		const prefix = "https://console.example.com/setup?token="
		if !strings.HasPrefix(out.setupTokenURL, prefix) {
			t.Fatalf("setup URL = %q, want prefix %q", out.setupTokenURL, prefix)
		}
		// The link code is trimmed and upper-cased before redemption.
		if len(f.redeems) != 1 {
			t.Fatalf("want 1 redeem, got %d", len(f.redeems))
		}
		if f.redeems[0].code != "ABC-123" {
			t.Errorf("redeemed code = %q, want ABC-123 (trimmed + upper-cased)", f.redeems[0].code)
		}
		if !strings.HasPrefix(f.redeems[0].newUserID, "usr-") {
			t.Errorf("redeem newUserID = %q, want usr- prefix", f.redeems[0].newUserID)
		}
		// Exactly one token minted, for the redeemed user, and only its hash stored —
		// the stored hash must be sha-256 of the raw token carried in the URL.
		if len(f.tokens) != 1 {
			t.Fatalf("want 1 setup token, got %d", len(f.tokens))
		}
		tok := f.tokens[0]
		if tok.userID != "usr-owner-1" {
			t.Errorf("token userID = %q, want usr-owner-1 (the redeemed owner)", tok.userID)
		}
		raw := strings.TrimPrefix(out.setupTokenURL, prefix)
		sum := sha256.Sum256([]byte(raw))
		if tok.tokenHash != hex.EncodeToString(sum[:]) {
			t.Error("stored token hash is not sha-256 of the raw token in the URL")
		}
		if tok.tokenHash == raw || tok.tokenHash == "" {
			t.Error("the raw token (or nothing) was stored instead of its hash")
		}
		// The token is short-lived and in the future.
		if !tok.expiresAt.After(time.Now()) {
			t.Errorf("token expiresAt = %v, want a future time", tok.expiresAt)
		}
	})

	t.Run("an empty link code mints nothing", func(t *testing.T) {
		f := &fakeOwnerStore{}
		if _, err := performSetupMCBind(ctx, f, "   ", "console.example.com", "root"); err == nil {
			t.Fatal("want error for an empty link code")
		}
		if len(f.redeems) != 0 || len(f.tokens) != 0 {
			t.Errorf("want no redeem/token on an empty code, got redeems=%d tokens=%d", len(f.redeems), len(f.tokens))
		}
		if _, ok := f.settings[api.LocalAuthEnabledKey]; ok {
			t.Error("local auth was enabled without an owner — the gate must not open on a failed bind")
		}
	})

	t.Run("a link-code redemption failure mints no token", func(t *testing.T) {
		f := &fakeOwnerStore{redeemErr: errors.New("code expired")}
		if _, err := performSetupMCBind(ctx, f, "abc-123", "console.example.com", "root"); err == nil {
			t.Fatal("want error when the link code cannot be redeemed")
		}
		if len(f.tokens) != 0 {
			t.Errorf("want no token minted on a redeem failure, got %d", len(f.tokens))
		}
		if _, ok := f.settings[api.LocalAuthEnabledKey]; ok {
			t.Error("local auth was enabled without an owner — the gate must not open on a failed redeem")
		}
	})

	t.Run("a local-auth failure fails the bind rather than minting an unredeemable URL", func(t *testing.T) {
		f := &fakeOwnerStore{redeemUserID: "usr-owner-1", setErr: errors.New("db down")}
		if _, err := performSetupMCBind(ctx, f, "abc-123", "console.example.com", "root"); err == nil {
			t.Fatal("want error when local auth cannot be enabled")
		}
		if len(f.redeems) != 0 || len(f.tokens) != 0 {
			t.Errorf("atomic setup was partially recorded: redeems=%d tokens=%d", len(f.redeems), len(f.tokens))
		}
	})

	t.Run("a token-store failure rolls the bind back", func(t *testing.T) {
		f := &fakeOwnerStore{redeemUserID: "usr-owner-1", createTokenErr: errors.New("db down")}
		if _, err := performSetupMCBind(ctx, f, "abc-123", "console.example.com", "root"); err == nil {
			t.Fatal("want error when the setup token cannot be stored")
		}
		if len(f.redeems) != 0 {
			t.Errorf("link code was consumed despite token failure, got %d redeems", len(f.redeems))
		}
		if len(f.tokens) != 0 {
			t.Errorf("want no recorded token when the store fails, got %d", len(f.tokens))
		}
		if _, ok := f.settings[api.LocalAuthEnabledKey]; ok {
			t.Error("local auth stayed enabled despite transaction rollback")
		}
	})

	t.Run("an audit failure does not cost the operator their install", func(t *testing.T) {
		f := &fakeOwnerStore{redeemUserID: "usr-owner-1", auditErr: errors.New("audit sink down")}
		out, err := performSetupMCBind(ctx, f, "abc-123", "console.example.com", "root")
		if err != nil {
			t.Fatalf("an audit failure must not fail the bind: %v", err)
		}
		if out.auditErr == nil {
			t.Error("the audit failure was swallowed instead of surfaced on the outcome")
		}
		if out.setupTokenURL == "" {
			t.Error("no setup URL minted despite a recoverable audit failure")
		}
		if _, ok := f.settings[api.LocalAuthEnabledKey]; !ok {
			t.Error("local auth was not enabled despite a recoverable audit failure")
		}
	})

	t.Run("defaults to the op.console host when adminHostname is empty", func(t *testing.T) {
		f := &fakeOwnerStore{redeemUserID: "usr-owner-1"}
		out, err := performSetupMCBind(ctx, f, "abc-123", "  ", "root")
		if err != nil {
			t.Fatalf("performSetupMCBind: %v", err)
		}
		// The Owner is staff, so onboarding lands on the operator console, not the
		// player panel — the empty-host fallback must reflect that.
		if !strings.HasPrefix(out.setupTokenURL, "https://op.console.localhost/setup?token=") {
			t.Errorf("setup URL = %q, want the op.console.localhost default host", out.setupTokenURL)
		}
	})
}
