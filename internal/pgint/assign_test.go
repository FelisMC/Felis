//go:build pgint

package pgint

import (
	"context"
	"errors"
	"testing"

	"felis.lolicon.best/internal/api"
)

func TestAssignServerCountsTheNewOwnersQuota(t *testing.T) {
	ctx := context.Background()
	alice := newUser(t, "user", "assign-from")
	bob := newUser(t, "user", "assign-to")
	one, cpu := 1, 150
	if _, err := repo.SetQuotas(ctx, bob.ID, api.QuotaInput{MaxServers: &one, MaxCPUMilli: &cpu}, "pgint"); err != nil {
		t.Fatal(err)
	}
	kept, moved := "as-kept-"+suffix(t), "as-moved-"+suffix(t)
	seedOwnedServer(t, kept, bob.ID, false)
	seedOwnedServer(t, moved, alice.ID, false)
	mustExec(t, `UPDATE servers SET tags = '{pvp}' WHERE name = $1`, moved)
	mustExec(t, `INSERT INTO server_allowlist (server_name, mc_uuid) VALUES ($1, $2)`, moved, testUUID(t))
	res := api.ResourceSpec{CPUMilli: 100, MemoryMB: 128, StorageMB: 1}

	owner, over, err := repo.AssignPreview(ctx, moved, bob.ID, res)
	want := []api.QuotaExcess{{Resource: "servers", Limit: 1, After: 2}, {Resource: "cpu_milli", Limit: 150, After: 200}}
	if err != nil || owner != alice.ID || len(over) != 2 || over[0] != want[0] || over[1] != want[1] {
		t.Fatalf("preview = %q %+v %v", owner, over, err)
	}
	if _, _, err := repo.AssignServer(ctx, moved, bob.ID, res, false); !errors.Is(err, api.ErrQuotaExceeded) || serverOwner(t, moved) != alice.ID {
		t.Fatalf("unconfirmed assign = %v, owner %s", err, serverOwner(t, moved))
	}

	previous, _, err := repo.AssignServer(ctx, moved, bob.ID, res, true)
	if err != nil || previous != alice.ID || serverOwner(t, moved) != bob.ID {
		t.Fatalf("confirmed assign = %q %v, owner %s", previous, err, serverOwner(t, moved))
	}
	var allowlisted, tags int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM server_allowlist WHERE server_name = $1),
		(SELECT cardinality(tags) FROM servers WHERE name = $1)`, moved).Scan(&allowlisted, &tags); err != nil {
		t.Fatal(err)
	}
	if allowlisted != 0 || tags != 0 {
		t.Fatalf("after assign: %d allowlist rows, %d tags, want none", allowlisted, tags)
	}

	if _, _, err := repo.AssignServer(ctx, moved, bob.ID, res, true); !errors.Is(err, api.ErrAlreadyOwner) {
		t.Fatalf("assign to the current owner = %v", err)
	}
	if err := repo.SetUserDisabled(ctx, alice.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.AssignServer(ctx, moved, alice.ID, res, true); !errors.Is(err, api.ErrUnknownAssignee) {
		t.Fatalf("assign to a disabled account = %v", err)
	}
	if _, err := repo.RequestRetire(ctx, kept, false); err != nil {
		t.Fatal(err)
	}
	carol := newUser(t, "user", "assign-third")
	if _, _, err := repo.AssignServer(ctx, kept, carol.ID, res, true); !errors.Is(err, api.ErrAssignRetiring) {
		t.Fatalf("assign a retiring server = %v", err)
	}
}
