//go:build pgint

package pgint

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/reaper"
)

func TestPortPatchRechecksOwnerAfterWaitingForServerRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldOwner, newOwner := newUser(t, "user", "port-old"), newUser(t, "user", "port-new")
	name := "ports-owner-" + suffix(t)
	seedOwnedServer(t, name, oldOwner.ID, false)
	hold, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback()
	if _, err := hold.ExecContext(ctx, `SELECT name FROM servers WHERE name = $1 FOR UPDATE`, name); err != nil {
		t.Fatal(err)
	}
	var applied atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- repo.PatchServerPorts(ctx, name, oldOwner.ID, func() error { applied.Add(1); return nil })
	}()
	waitForLockWait(t, "SELECT COALESCE(owner_id, ''), protected, retire_requested_at")
	if _, err := hold.ExecContext(ctx, `UPDATE servers SET owner_id = $2 WHERE name = $1`, name, newOwner.ID); err != nil {
		t.Fatal(err)
	}
	if err := hold.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || applied.Load() != 0 {
		t.Fatal("the old owner's queued port write ran after ownership changed")
	}
	if err := repo.PatchServerPorts(ctx, name, newOwner.ID, func() error { applied.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := repo.PatchServerPorts(ctx, name, "", func() error { applied.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	if applied.Load() != 2 || serverOwner(t, name) != newOwner.ID {
		t.Fatal("authorized owner/admin writes changed ownership or failed to apply")
	}
}

func TestReaperPortCleanupWaitsForInflightPortPatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := newUser(t, "user", "port-reap")
	name := "ports-reap-" + suffix(t)
	seedOwnedServer(t, name, u.ID, false)
	entered, proceed := make(chan struct{}), make(chan struct{})
	patched := make(chan error, 1)
	go func() {
		patched <- repo.PatchServerPorts(ctx, name, u.ID, func() error {
			close(entered)
			select {
			case <-proceed:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("port patch did not acquire its server row")
	}
	var cleaned atomic.Int32
	released := make(chan error, 1)
	go func() {
		released <- reaper.NewPGStore(db).ReleaseWorld(ctx, name, time.Now(), func(owner string) error {
			if owner != u.ID {
				return errors.New("cleanup saw another owner")
			}
			cleaned.Add(1)
			return nil
		})
	}()
	waitForLockWait(t, "SELECT COALESCE(owner_id, '') FROM servers")
	if cleaned.Load() != 0 || serverOwner(t, name) != u.ID {
		t.Fatal("ownership was released before the prior port patch finished")
	}
	close(proceed)
	if err := <-patched; err != nil {
		t.Fatal(err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if cleaned.Load() != 1 || serverOwner(t, name) != "" {
		t.Fatal("confirmed cleanup failed to release ownership")
	}
	if err := repo.PatchServerPorts(ctx, name, u.ID, func() error { t.Error("released owner wrote ports"); return nil }); err == nil {
		t.Fatal("released owner remained authorized")
	}
}

func TestDeleteUserPortCleanupBlocksClaimAndStaleOwnerPatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldOwner, newOwner := newUser(t, "user", "port-del"), newUser(t, "user", "port-claim")
	name := "ports-delete-" + suffix(t)
	seedOwnedServer(t, name, oldOwner.ID, false)
	entered, proceed := make(chan struct{}), make(chan struct{})
	deleted := make(chan error, 1)
	go func() {
		deleted <- repo.DeleteUser(ctx, oldOwner.ID, "pgint", func(ctx context.Context, server string) error {
			if server != name {
				return errors.New("cleaned an unrelated server")
			}
			close(entered)
			select {
			case <-proceed:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("deletion did not enter port cleanup")
	}
	var applied atomic.Int32
	patched := make(chan error, 1)
	go func() {
		patched <- repo.PatchServerPorts(ctx, name, oldOwner.ID, func() error { applied.Add(1); return nil })
	}()
	waitForLockWait(t, "SELECT COALESCE(owner_id, ''), protected, retire_requested_at")
	claimed := make(chan bool, 1)
	claimErr := make(chan error, 1)
	go func() {
		ok, err := repo.ClaimServer(ctx, name, newOwner.ID, nil)
		claimed <- ok
		claimErr <- err
	}()
	waitForLockWait(t, "SELECT owner_id, retire_requested_at")
	if applied.Load() != 0 || serverOwner(t, name) != oldOwner.ID {
		t.Fatal("a waiting port write or claim crossed unconfirmed cleanup")
	}
	close(proceed)
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if err := <-patched; err == nil || applied.Load() != 0 {
		t.Fatal("stale owner wrote ports after account deletion")
	}
	if err := <-claimErr; err != nil {
		t.Fatal(err)
	}
	if <-claimed || serverOwner(t, name) != oldOwner.ID {
		t.Fatal("claim succeeded before reaper archived and released the deleted user's server")
	}
	rec, err := repo.ServerByName(ctx, name)
	if err != nil || rec.Retire == nil || rec.Retire.Delete {
		t.Fatalf("server after user deletion = %+v, %v; want pending release retirement", rec, err)
	}
	mine, err := repo.MyServers(ctx, newOwner.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range mine {
		if s.Name == name {
			t.Fatalf("deleted user's unarchived server surfaced in MyServers: %+v", s)
		}
	}
	if err := reaper.NewPGStore(db).ReleaseWorld(ctx, name, time.Now(), func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.ClaimServer(ctx, name, newOwner.ID, nil); err != nil || !ok || serverOwner(t, name) != newOwner.ID {
		t.Fatalf("claim after reaper release = (%v, %v), owner=%q", ok, err, serverOwner(t, name))
	}
}

func TestPortCleanupFailurePreservesAccountAndOwnership(t *testing.T) {
	for _, operation := range []string{"delete user", "release world", "claim", "assign"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			u := newUser(t, "user", "port-fail")
			name := "ports-failed-" + suffix(t)
			failure := errors.New("operator has not confirmed port removal")
			var err error
			switch operation {
			case "delete user":
				seedOwnedServer(t, name, u.ID, false)
				err = repo.DeleteUser(ctx, u.ID, "pgint", func(context.Context, string) error { return failure })
			case "release world":
				seedOwnedServer(t, name, u.ID, false)
				err = reaper.NewPGStore(db).ReleaseWorld(ctx, name, time.Now(), func(string) error { return failure })
			case "claim":
				seedOwnerless(t, name, 128)
				_, err = repo.ClaimServer(ctx, name, u.ID, func(context.Context, string) error { return failure })
				if !errors.Is(err, failure) || serverOwner(t, name) != "" {
					t.Fatal("failed cleanup still claimed the server")
				}
				return
			case "assign":
				next := newUser(t, "user", "port-fail-dst")
				seedOwnedServer(t, name, u.ID, false)
				res := api.ResourceSpec{CPUMilli: 100, MemoryMB: 128, StorageMB: 1}
				_, _, err = repo.AssignServer(ctx, name, next.ID, res, true, func(context.Context, string) error { return failure })
			}
			if !errors.Is(err, failure) || serverOwner(t, name) != u.ID {
				t.Fatal("failed cleanup still released the server")
			}
			var active bool
			if err := db.QueryRowContext(ctx, `SELECT NOT disabled AND deleted_at IS NULL FROM users WHERE id = $1`, u.ID).Scan(&active); err != nil || !active {
				t.Fatal("failed cleanup retired the account")
			}
		})
	}
}

func TestAssignCleanupBlocksStaleOwnerPatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldOwner, newOwner := newUser(t, "user", "port-as-old"), newUser(t, "user", "port-as-new")
	name := "ports-assign-" + suffix(t)
	seedOwnedServer(t, name, oldOwner.ID, false)
	entered, proceed := make(chan struct{}), make(chan struct{})
	assigned := make(chan error, 1)
	res := api.ResourceSpec{CPUMilli: 100, MemoryMB: 128, StorageMB: 1}
	go func() {
		_, _, err := repo.AssignServer(ctx, name, newOwner.ID, res, true, func(ctx context.Context, server string) error {
			if server != name {
				return errors.New("cleaned an unrelated server")
			}
			close(entered)
			select {
			case <-proceed:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		assigned <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("assignment did not enter owner cleanup")
	}
	var applied atomic.Int32
	patched := make(chan error, 1)
	go func() {
		patched <- repo.PatchServerPorts(ctx, name, oldOwner.ID, func() error { applied.Add(1); return nil })
	}()
	waitForLockWait(t, "SELECT COALESCE(owner_id, ''), protected, retire_requested_at")
	if applied.Load() != 0 || serverOwner(t, name) != oldOwner.ID {
		t.Fatal("a waiting owner patch crossed unconfirmed assignment cleanup")
	}
	close(proceed)
	if err := <-assigned; err != nil {
		t.Fatal(err)
	}
	if err := <-patched; err == nil || applied.Load() != 0 {
		t.Fatal("previous owner wrote settings after assignment")
	}
	if serverOwner(t, name) != newOwner.ID {
		t.Fatal("new owner did not receive the server")
	}
}

func TestAccountMigrationWaitsForPortPatchAndRetainsConfiguration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := mustNow()
	src, dst := newUser(t, "user", "port-mig-src"), newUser(t, "user", "port-mig-dst")
	name := "ports-migration-" + suffix(t)
	seedOwnedServer(t, name, src.ID, false)
	code := "port-migration-" + suffix(t)
	startToCode(t, src.ID, dst.ID, code, now, now.Add(10*time.Minute))
	entered, proceed := make(chan struct{}), make(chan struct{})
	patched := make(chan error, 1)
	var configuration atomic.Int32
	go func() {
		patched <- repo.PatchServerPorts(ctx, name, src.ID, func() error {
			close(entered)
			select {
			case <-proceed:
				configuration.Store(24454)
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("port patch did not acquire the server row")
	}
	migrated := make(chan error, 1)
	go func() { _, _, err := repo.RedeemMigration(ctx, dst.ID, code, now); migrated <- err }()
	waitForLockWait(t, "UPDATE servers SET owner_id")
	close(proceed)
	if err := <-patched; err != nil {
		t.Fatal(err)
	}
	if err := <-migrated; err != nil {
		t.Fatal(err)
	}
	if configuration.Load() != 24454 || serverOwner(t, name) != dst.ID {
		t.Fatal("migration reset the port configuration or failed to transfer the server")
	}
	if err := repo.PatchServerPorts(ctx, name, src.ID, func() error { configuration.Store(19132); return nil }); err == nil || configuration.Load() != 24454 {
		t.Fatal("retired migration source overwrote the new owner's ports")
	}
}
