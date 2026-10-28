//go:build pgint

package pgint

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
)

func newKey(t *testing.T, userID, tag string, expires *time.Time) (*api.APIToken, string) {
	t.Helper()
	tok := &api.APIToken{ID: tag + "-" + suffix(t), UserID: userID, Name: tag, Prefix: "felis_pat_" + tag}
	tok.ExpiresAt = expires
	hash := "hash-" + tok.ID
	if err := repo.CreateAPIToken(context.Background(), tok, hash, 10, mustNow()); err != nil {
		t.Fatalf("CreateAPIToken(%s): %v", tag, err)
	}
	return tok, hash
}

func TestAPITokenLimitHoldsUnderConcurrency(t *testing.T) {
	u := newUser(t, "user", "key-limit")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok := &api.APIToken{ID: "race-" + suffix(t), UserID: u.ID, Name: "race", Prefix: "felis_pat_race"}
			errs[i] = repo.CreateAPIToken(context.Background(), tok, "hash-"+tok.ID, 3, mustNow())
		}()
	}
	wg.Wait()
	created := 0
	for _, err := range errs {
		switch {
		case err == nil:
			created++
		case !errors.Is(err, api.ErrAPITokenLimit):
			t.Fatalf("CreateAPIToken: %v", err)
		}
	}
	if created != 3 {
		t.Fatalf("created %d keys under a limit of 3", created)
	}
}

func TestAPITokenLookupSkipsDeadKeysAndUsers(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	u := newUser(t, "user", "key-live")
	other := newUser(t, "user", "key-other")
	soon := now.Add(time.Hour)
	live, liveHash := newKey(t, u.ID, "live", &soon)
	revoked, revokedHash := newKey(t, u.ID, "revoked", nil)

	if err := repo.RevokeAPIToken(ctx, other.ID, revoked.ID, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("revoke by another user = %v, want ErrNotFound", err)
	}
	if err := repo.RevokeAPIToken(ctx, u.ID, revoked.ID, now); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if err := repo.RevokeAPIToken(ctx, u.ID, revoked.ID, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("second revoke = %v, want ErrNotFound", err)
	}
	if _, _, err := repo.APITokenUser(ctx, revokedHash, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("revoked key lookup = %v", err)
	}

	got, tok, err := repo.APITokenUser(ctx, liveHash, now)
	if err != nil || got.ID != u.ID || tok.ID != live.ID {
		t.Fatalf("live key lookup = %+v %+v %v", got, tok, err)
	}
	if _, _, err := repo.APITokenUser(ctx, liveHash, soon); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("expired key lookup = %v", err)
	}
	if list, err := repo.ListAPITokens(ctx, u.ID, now); err != nil || len(list) != 1 || list[0].ID != live.ID {
		t.Fatalf("ListAPITokens = %+v %v, want only the live key", list, err)
	}

	if err := repo.SetUserDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.APITokenUser(ctx, liveHash, now); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("disabled user's key lookup = %v", err)
	}
}

func TestAdminAPITokenMustExpire(t *testing.T) {
	u := newUser(t, "admin", "key-admin")
	tok := &api.APIToken{ID: "admin-" + suffix(t), UserID: u.ID, Name: "ops", Prefix: "felis_pat_ops", Admin: true}
	if err := repo.CreateAPIToken(context.Background(), tok, "hash-"+tok.ID, 10, mustNow()); err == nil {
		t.Fatal("an admin key without an expiry was stored")
	}
}
