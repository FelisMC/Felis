package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"felis.lolicon.best/internal/api"

	"golang.org/x/crypto/bcrypt"
)

// fakeOwnerStore records what break-glass provisioning writes and answers the
// identity lookups, so the core logic (authentication, provisioning, accountability
// audit) is exercised without a database or a terminal.
type fakeOwnerStore struct {
	upserts  []upsertCall
	settings map[string][]byte
	audits   []api.AuditEntry
	users    map[string]*api.StaffUser // keyed by username
	admins   bool                      // AdminExists answer

	upsertErr error
	setErr    error
	auditErr  error
	userErr   error // non-not-found error from UserByUsername
	adminErr  error
}

type upsertCall struct {
	id, username, email, passwordHash string
	mustChange                        bool
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

func (f *fakeOwnerStore) UpsertOwner(_ context.Context, id, username, email, passwordHash string, mustChange bool) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, upsertCall{id, username, email, passwordHash, mustChange})
	return nil
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

// mkAdmin builds an authenticatable admin row (role=admin, real bcrypt hash) for the
// fake. MinCost keeps the hash fast — these tests are about wiring, not bcrypt.
func mkAdmin(t *testing.T, username, password string) *api.StaffUser {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return &api.StaffUser{ID: "usr-admin", Username: username, Role: "admin", PasswordHash: string(h)}
}

func TestValidateOwnerPassword(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		ok   bool
	}{
		{"too short", "1234567", false},
		{"minimum", "12345678", true},
		{"comfortable", "Mid-Range-1", true},
		{"at the bcrypt limit", strings.Repeat("a", 72), true},
		{"past the bcrypt limit", strings.Repeat("a", 73), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOwnerPassword(tc.pw)
			if tc.ok && err != nil {
				t.Errorf("validateOwnerPassword(%d bytes) = %v, want nil", len(tc.pw), err)
			}
			if !tc.ok && err == nil {
				t.Errorf("validateOwnerPassword(%d bytes) = nil, want error", len(tc.pw))
			}
		})
	}
}

func TestProvisionOwner(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path mints a must-change admin with a verifiable hash", func(t *testing.T) {
		f := &fakeOwnerStore{}
		const pw = "valid-test-pw"
		if err := provisionOwner(ctx, f, "owner", "me@example.com", pw); err != nil {
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
		// must_change_password=true is load-bearing: it arms the API lockdown so the
		// Owner can do nothing but change the password on first login.
		if !got.mustChange {
			t.Error("mustChange = false, want true (forced first-login change)")
		}
		if !strings.HasPrefix(got.id, "usr-") {
			t.Errorf("id = %q, want usr- prefix", got.id)
		}
		// Only the hash is stored; the typed plaintext must verify against it.
		if bcrypt.CompareHashAndPassword([]byte(got.passwordHash), []byte(pw)) != nil {
			t.Error("typed password does not verify against the stored hash")
		}
		if got.passwordHash == pw {
			t.Error("stored hash equals plaintext — password was not hashed")
		}
	})

	t.Run("trims surrounding whitespace", func(t *testing.T) {
		f := &fakeOwnerStore{}
		if err := provisionOwner(ctx, f, "  owner  ", "  e@x.io  ", "valid-test-pw"); err != nil {
			t.Fatalf("provisionOwner: %v", err)
		}
		if f.upserts[0].username != "owner" || f.upserts[0].email != "e@x.io" {
			t.Errorf("got username=%q email=%q, want trimmed", f.upserts[0].username, f.upserts[0].email)
		}
	})

	t.Run("rejects an empty username before any write", func(t *testing.T) {
		f := &fakeOwnerStore{}
		if err := provisionOwner(ctx, f, "   ", "", "valid-test-pw"); err == nil {
			t.Fatal("want error for empty username")
		}
		if len(f.upserts) != 0 {
			t.Errorf("want no upsert on validation failure, got %d", len(f.upserts))
		}
	})

	t.Run("rejects a weak password before any write", func(t *testing.T) {
		f := &fakeOwnerStore{}
		if err := provisionOwner(ctx, f, "owner", "", "short"); err == nil {
			t.Fatal("want error for a sub-8-byte password")
		}
		if len(f.upserts) != 0 {
			t.Errorf("want no upsert on weak password, got %d", len(f.upserts))
		}
	})

	t.Run("propagates a store error", func(t *testing.T) {
		f := &fakeOwnerStore{upsertErr: errors.New("boom")}
		if err := provisionOwner(ctx, f, "owner", "", "valid-test-pw"); err == nil {
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

func TestGenerateBootstrapPassword(t *testing.T) {
	const want = 20
	pw, err := generateBootstrapPassword()
	if err != nil {
		t.Fatalf("generateBootstrapPassword: %v", err)
	}
	if len(pw) != want {
		t.Errorf("length = %d, want %d", len(pw), want)
	}
	for _, c := range pw {
		if !strings.ContainsRune(bootstrapPasswordAlphabet, c) {
			t.Errorf("password contains out-of-alphabet rune %q", c)
		}
	}
	// A generated password must satisfy the same rule provisionOwner enforces.
	if err := validateOwnerPassword(pw); err != nil {
		t.Errorf("generated password fails validateOwnerPassword: %v", err)
	}
	other, err := generateBootstrapPassword()
	if err != nil {
		t.Fatal(err)
	}
	if pw == other {
		t.Error("two calls produced the same password")
	}
}

func TestAuthenticateAdmin(t *testing.T) {
	ctx := context.Background()

	t.Run("verifies a matching admin credential", func(t *testing.T) {
		f := &fakeOwnerStore{users: map[string]*api.StaffUser{"root": mkAdmin(t, "root", "correct horse")}}
		matched, ok, err := authenticateAdmin(ctx, f, "root", "correct horse")
		if err != nil {
			t.Fatalf("authenticateAdmin: %v", err)
		}
		if !ok {
			t.Fatal("ok = false, want true for the correct password")
		}
		if matched != "root" {
			t.Errorf("matched = %q, want root", matched)
		}
	})

	t.Run("a wrong password is a non-match, not an error", func(t *testing.T) {
		f := &fakeOwnerStore{users: map[string]*api.StaffUser{"root": mkAdmin(t, "root", "correct horse")}}
		_, ok, err := authenticateAdmin(ctx, f, "root", "wrong")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Error("ok = true, want false for a wrong password")
		}
	})

	t.Run("a non-admin role can never attribute a break-glass", func(t *testing.T) {
		player := mkAdmin(t, "alice", "correct horse")
		player.Role = "user" // a player row, even with a hash, is not staff
		f := &fakeOwnerStore{users: map[string]*api.StaffUser{"alice": player}}
		_, ok, err := authenticateAdmin(ctx, f, "alice", "correct horse")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Error("ok = true, want false for a non-admin role")
		}
	})

	t.Run("a hashless admin row is a non-match", func(t *testing.T) {
		f := &fakeOwnerStore{users: map[string]*api.StaffUser{
			"ghost": {Username: "ghost", Role: "admin", PasswordHash: ""},
		}}
		_, ok, err := authenticateAdmin(ctx, f, "ghost", "anything")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Error("ok = true, want false when no hash is set")
		}
	})

	t.Run("an unknown user is a non-match, not an error", func(t *testing.T) {
		f := &fakeOwnerStore{}
		_, ok, err := authenticateAdmin(ctx, f, "nobody", "pw")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Error("ok = true, want false for an unknown user")
		}
	})

	t.Run("empty input is a non-match with no store call", func(t *testing.T) {
		f := &fakeOwnerStore{userErr: errors.New("must not be called")}
		if _, ok, err := authenticateAdmin(ctx, f, "", "pw"); ok || err != nil {
			t.Errorf("empty username: ok=%v err=%v, want false,nil", ok, err)
		}
		if _, ok, err := authenticateAdmin(ctx, f, "root", ""); ok || err != nil {
			t.Errorf("empty password: ok=%v err=%v, want false,nil", ok, err)
		}
	})

	t.Run("a datastore fault is surfaced", func(t *testing.T) {
		f := &fakeOwnerStore{userErr: errors.New("db down")}
		if _, _, err := authenticateAdmin(ctx, f, "root", "pw"); err == nil {
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

	t.Run("bootstrap uses the typed password and never echoes it", func(t *testing.T) {
		f := &fakeOwnerStore{}
		op := breakGlassOp{
			mode:          "bootstrap",
			accountable:   "deploybot",
			osUser:        "deploybot",
			ownerUsername: "owner",
			ownerEmail:    "owner@example.com",
			ownerPassword: "valid-test-pw",
		}
		out, err := performBreakGlass(ctx, f, op)
		if err != nil {
			t.Fatalf("performBreakGlass: %v", err)
		}
		// The operator typed their own password, so it must NOT be surfaced for display.
		if out.displayPassword != "" {
			t.Errorf("displayPassword = %q, want empty for a typed bootstrap password", out.displayPassword)
		}
		if out.auditErr != nil {
			t.Errorf("auditErr = %v, want nil", out.auditErr)
		}
		if len(f.upserts) != 1 || bcrypt.CompareHashAndPassword([]byte(f.upserts[0].passwordHash), []byte("valid-test-pw")) != nil {
			t.Error("owner was not provisioned with the typed password")
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

	t.Run("recovery generates a one-time password and records a verified row", func(t *testing.T) {
		f := &fakeOwnerStore{}
		op := breakGlassOp{
			mode:           "recovery",
			accountable:    "root",
			osUser:         "alice",
			ownerUsername:  "owner",
			attemptedAdmin: "root",
		}
		out, err := performBreakGlass(ctx, f, op)
		if err != nil {
			t.Fatalf("performBreakGlass: %v", err)
		}
		if out.displayPassword == "" {
			t.Fatal("displayPassword empty, want a generated one-time password")
		}
		// The shown password must be the one actually stored (as a hash).
		if bcrypt.CompareHashAndPassword([]byte(f.upserts[0].passwordHash), []byte(out.displayPassword)) != nil {
			t.Error("displayed password does not match the stored hash")
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
	})

	t.Run("root override records an unverified row attributed to the OS user", func(t *testing.T) {
		f := &fakeOwnerStore{}
		op := breakGlassOp{
			mode:           "root_override",
			accountable:    "alice",
			osUser:         "alice",
			ownerUsername:  "owner",
			attemptedAdmin: "typo-admin",
		}
		out, err := performBreakGlass(ctx, f, op)
		if err != nil {
			t.Fatalf("performBreakGlass: %v", err)
		}
		if out.displayPassword == "" {
			t.Error("displayPassword empty, want a generated one-time password")
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
		op := breakGlassOp{mode: "bootstrap", accountable: "root", osUser: "root", ownerUsername: "owner", ownerPassword: "valid-test-pw"}
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
		// The credential is reset by provisionOwner; if the audit were written only
		// after enableLocalAuth, a failed toggle write would leave that reset with no
		// "who did it" row. Order guarantees the accountability row lands first.
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
