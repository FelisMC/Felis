package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// fakeOwnerStore records what break-glass provisioning writes, so the core logic
// is exercised without a database or a terminal.
type fakeOwnerStore struct {
	upserts   []upsertCall
	settings  map[string][]byte
	upsertErr error
	setErr    error
}

type upsertCall struct {
	id, username, email, passwordHash string
	mustChange                        bool
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

func TestProvisionOwner(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path mints a must-change admin with a verifiable hash", func(t *testing.T) {
		f := &fakeOwnerStore{}
		pw, err := provisionOwner(ctx, f, "owner", "me@example.com")
		if err != nil {
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
		// The plaintext is returned only to be shown; only the hash is stored. The
		// returned password must verify against the stored hash.
		if bcrypt.CompareHashAndPassword([]byte(got.passwordHash), []byte(pw)) != nil {
			t.Error("returned password does not verify against the stored hash")
		}
		if pw == got.passwordHash {
			t.Error("stored hash equals plaintext — password was not hashed")
		}
	})

	t.Run("trims surrounding whitespace", func(t *testing.T) {
		f := &fakeOwnerStore{}
		if _, err := provisionOwner(ctx, f, "  owner  ", "  e@x.io  "); err != nil {
			t.Fatalf("provisionOwner: %v", err)
		}
		if f.upserts[0].username != "owner" || f.upserts[0].email != "e@x.io" {
			t.Errorf("got username=%q email=%q, want trimmed", f.upserts[0].username, f.upserts[0].email)
		}
	})

	t.Run("rejects an empty username before any write", func(t *testing.T) {
		f := &fakeOwnerStore{}
		if _, err := provisionOwner(ctx, f, "   ", ""); err == nil {
			t.Fatal("want error for empty username")
		}
		if len(f.upserts) != 0 {
			t.Errorf("want no upsert on validation failure, got %d", len(f.upserts))
		}
	})

	t.Run("propagates a store error", func(t *testing.T) {
		f := &fakeOwnerStore{upsertErr: errors.New("boom")}
		if _, err := provisionOwner(ctx, f, "owner", ""); err == nil {
			t.Fatal("want error when the store fails")
		}
	})

	t.Run("two runs mint distinct passwords", func(t *testing.T) {
		f := &fakeOwnerStore{}
		a, err := provisionOwner(ctx, f, "owner", "")
		if err != nil {
			t.Fatal(err)
		}
		b, err := provisionOwner(ctx, f, "owner", "")
		if err != nil {
			t.Fatal(err)
		}
		if a == b {
			t.Error("two provisions produced the same bootstrap password")
		}
	})
}

func TestEnableLocalAuth(t *testing.T) {
	f := &fakeOwnerStore{}
	if err := enableLocalAuth(context.Background(), f); err != nil {
		t.Fatalf("enableLocalAuth: %v", err)
	}
	raw, ok := f.settings[localAuthEnabledSettingKey]
	if !ok {
		t.Fatalf("setting %q not written", localAuthEnabledSettingKey)
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
	// bcrypt's hard limit is 72 bytes; a bootstrap password must stay well under it.
	if len(pw) > 72 {
		t.Errorf("length %d exceeds bcrypt's 72-byte limit", len(pw))
	}
	other, err := generateBootstrapPassword()
	if err != nil {
		t.Fatal(err)
	}
	if pw == other {
		t.Error("two calls produced the same password")
	}
}

func TestProvisionCmd(t *testing.T) {
	t.Run("performs both load-bearing writes", func(t *testing.T) {
		f := &fakeOwnerStore{}
		msg := provisionCmd(context.Background(), f, "owner", "")()
		pm, ok := msg.(provisionedMsg)
		if !ok {
			t.Fatalf("got %T, want provisionedMsg", msg)
		}
		if pm.err != nil {
			t.Fatalf("provisionCmd error: %v", pm.err)
		}
		if pm.password == "" {
			t.Error("empty password in result")
		}
		if len(f.upserts) != 1 {
			t.Errorf("want 1 owner upsert, got %d", len(f.upserts))
		}
		if _, ok := f.settings[localAuthEnabledSettingKey]; !ok {
			t.Error("local auth was not enabled — login would 403")
		}
	})

	t.Run("does not enable local auth if the owner write fails", func(t *testing.T) {
		f := &fakeOwnerStore{upsertErr: errors.New("boom")}
		msg := provisionCmd(context.Background(), f, "owner", "")()
		pm := msg.(provisionedMsg)
		if pm.err == nil {
			t.Fatal("want error when the owner write fails")
		}
		if len(f.settings) != 0 {
			t.Error("local auth should not be enabled when provisioning failed")
		}
	})
}
