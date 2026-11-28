//go:build pgint

package pgint

import (
	"context"
	"errors"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
)

func TestProtectedServersAndCollaboratorsPGContract(t *testing.T) {
	ctx := context.Background()
	sfx := suffix(t)
	owner := newUser(t, "owner", "po-"+sfx)
	admin := newUser(t, "admin", "pa-"+sfx)
	opUser := newUser(t, "user", "op-"+sfx)
	mgrUser := newUser(t, "user", "mg-"+sfx)
	formerOwner := newUser(t, "user", "fo-"+sfx)

	protSrv := "pg-prot-" + sfx
	openSrv := "pg-open-" + sfx
	freeProt := "pg-free-" + sfx
	for _, n := range []string{protSrv, openSrv, freeProt} {
		if err := repo.SeedServer(ctx, n, n+"-sub", 1000, 2048, 10240); err != nil {
			t.Fatalf("SeedServer(%s): %v", n, err)
		}
	}
	if _, _, err := repo.AssignServer(ctx, protSrv, formerOwner.ID, api.ResourceSpec{CPUMilli: 100, MemoryMB: 128, StorageMB: 1}, true, nil); err != nil {
		t.Fatalf("AssignServer(prot): %v", err)
	}
	if _, _, err := repo.AssignServer(ctx, openSrv, opUser.ID, api.ResourceSpec{CPUMilli: 100, MemoryMB: 128, StorageMB: 1}, true, nil); err != nil {
		t.Fatalf("AssignServer(open): %v", err)
	}

	if err := repo.SetServerProtected(ctx, protSrv, true); err != nil {
		t.Fatalf("SetServerProtected(true): %v", err)
	}
	if err := repo.SetServerProtected(ctx, freeProt, true); err != nil {
		t.Fatalf("SetServerProtected(freeProt): %v", err)
	}
	if err := repo.SetServerProtected(ctx, "pg-missing-"+sfx, true); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("SetServerProtected(missing) = %v, want ErrNotFound", err)
	}

	if _, err := repo.UpsertServerCollaborator(ctx, protSrv, opUser.ID, api.CollabRoleOperator, owner.ID); err != nil {
		t.Fatalf("UpsertServerCollaborator(opUser): %v", err)
	}
	if _, err := repo.UpsertServerCollaborator(ctx, protSrv, mgrUser.ID, api.CollabRoleManager, owner.ID); err != nil {
		t.Fatalf("UpsertServerCollaborator(mgrUser): %v", err)
	}
	if _, err := repo.UpsertServerCollaborator(ctx, protSrv, opUser.ID, api.CollabRoleManager, owner.ID); err != nil {
		t.Fatalf("UpsertServerCollaborator(upgrade): %v", err)
	}
	if role, err := repo.UserCollaboratorRole(ctx, protSrv, opUser.ID); err != nil || role != api.CollabRoleManager {
		t.Fatalf("UserCollaboratorRole = %q, %v; want manager", role, err)
	}
	if _, err := repo.UpsertServerCollaborator(ctx, protSrv, opUser.ID, api.CollabRoleOperator, owner.ID); err != nil {
		t.Fatalf("UpsertServerCollaborator(downgrade): %v", err)
	}

	list, err := repo.ServerCollaborators(ctx, protSrv)
	if err != nil || len(list) != 2 {
		t.Fatalf("ServerCollaborators = %+v, %v; want 2", list, err)
	}
	cands, err := repo.CollaboratorCandidates(ctx, 200)
	if err != nil {
		t.Fatalf("CollaboratorCandidates: %v", err)
	}
	for _, c := range cands {
		if c.ID == owner.ID || c.Role == "owner" {
			t.Fatalf("CollaboratorCandidates leaked owner account: %+v", c)
		}
	}

	if ok, err := repo.HasAnyCollaboratorAccess(ctx, admin.ID); err != nil || ok {
		t.Fatalf("HasAnyCollaboratorAccess(admin) = %v, %v; want false", ok, err)
	}
	if ok, err := repo.HasAnyCollaboratorAccess(ctx, mgrUser.ID); err != nil || !ok {
		t.Fatalf("HasAnyCollaboratorAccess(mgr) = %v, %v; want true", ok, err)
	}

	// MyServers exposes collaborator servers and excludes protected unclaimed ones.
	mineMgr, err := repo.MyServers(ctx, mgrUser.ID)
	if err != nil {
		t.Fatalf("MyServers(mgr): %v", err)
	}
	foundProt := false
	for _, v := range mineMgr {
		if v.Name == freeProt {
			t.Fatalf("MyServers exposed protected unclaimed server %s", freeProt)
		}
		if v.Name == protSrv {
			foundProt = true
			if v.AccessRole != api.AccessRoleManager || !v.Protected || v.Claimable || v.Owned {
				t.Fatalf("protSrv view = %+v", v)
			}
		}
	}
	if !foundProt {
		t.Fatal("MyServers omitted collaborated protected server")
	}

	// Unclaimed protected server cannot be claimed even with quota.
	if ok, err := repo.ClaimServer(ctx, freeProt, mgrUser.ID, nil); err != nil || ok {
		t.Fatalf("ClaimServer(protected) = %v, %v; want false, nil", ok, err)
	}

	// PatchServerPorts allows manager collaborator and refuses operator or non-collaborator owner on protected servers.
	if err := repo.PatchServerPorts(ctx, protSrv, mgrUser.ID, func() error { return nil }); err != nil {
		t.Fatalf("PatchServerPorts(manager): %v", err)
	}
	if err := repo.PatchServerPorts(ctx, protSrv, opUser.ID, func() error { return nil }); err == nil {
		t.Fatal("PatchServerPorts(operator) unexpectedly succeeded")
	}
	if err := repo.PatchServerPorts(ctx, protSrv, formerOwner.ID, func() error { return nil }); err == nil {
		t.Fatal("PatchServerPorts(non-collab owner on protected) unexpectedly succeeded")
	}

	// Backups scoping across Admin, former-owner, and Collaborator.
	now := time.Now().UTC()
	insBackup := func(id, srv, former string) {
		mustExec(t, `INSERT INTO world_backups (id, server_name, former_owner, backup_ref, size_bytes, reason, status, created_at, expires_at)
			VALUES ($1, $2, NULLIF($3, ''), $4, 100, 'manual', 'present', $5, $6)`,
			id, srv, former, "/arc/"+id+".tar.gz", now, now.Add(24*time.Hour))
	}
	bProtOwned, bProtPrev, bOpen := "bpo-"+sfx, "bpp-"+sfx, "bo-"+sfx
	insBackup(bProtOwned, protSrv, formerOwner.ID)
	insBackup(bProtPrev, protSrv, "someone-else")
	insBackup(bOpen, openSrv, opUser.ID)

	adminBackups, _, err := repo.BackupsForAdmin(ctx, admin.ID, api.BackupListOpts{Limit: 100})
	if err != nil {
		t.Fatalf("BackupsForAdmin: %v", err)
	}
	for _, b := range adminBackups {
		if b.ServerName == protSrv {
			t.Fatalf("BackupsForAdmin leaked protected server backup %+v", b)
		}
	}

	mgrBackups, _, err := repo.BackupsForUser(ctx, mgrUser.ID, api.BackupListOpts{Server: protSrv, Limit: 100})
	if err != nil {
		t.Fatalf("BackupsForUser(mgr): %v", err)
	}
	if len(mgrBackups) != 1 || mgrBackups[0].ID != bProtOwned {
		t.Fatalf("BackupsForUser(mgr) = %+v, want only %s (must exclude previous owner's %s)", mgrBackups, bProtOwned, bProtPrev)
	}

	foBackups, _, err := repo.BackupsForUser(ctx, formerOwner.ID, api.BackupListOpts{Server: protSrv, Limit: 100})
	if err != nil {
		t.Fatalf("BackupsForUser(non-collab formerOwner on protected): %v", err)
	}
	if len(foBackups) != 0 {
		t.Fatalf("BackupsForUser leaked protected server backup to non-collaborator former owner: %+v", foBackups)
	}

	// Claiming/Assigning/Seeding/Deleting clears collaborators cleanly.
	if err := repo.DeleteUser(ctx, mgrUser.ID, "pgint", func(context.Context, string) error { return nil }); err != nil {
		t.Fatalf("DeleteUser(mgr): %v", err)
	}
	if role, err := repo.UserCollaboratorRole(ctx, protSrv, mgrUser.ID); err != nil || role != "" {
		t.Fatalf("deleted user retained collaborator role %q", role)
	}

	if err := repo.SetServerProtected(ctx, protSrv, false); err != nil {
		t.Fatal(err)
	}
	newOwner := newUser(t, "user", "no-"+sfx)
	if _, _, err := repo.AssignServer(ctx, protSrv, newOwner.ID, api.ResourceSpec{CPUMilli: 100, MemoryMB: 128, StorageMB: 1}, true, nil); err != nil {
		t.Fatalf("AssignServer(clear collab): %v", err)
	}
	if list, err := repo.ServerCollaborators(ctx, protSrv); err != nil || len(list) != 0 {
		t.Fatalf("AssignServer left %d collaborators, want 0", len(list))
	}

	if _, err := repo.UpsertServerCollaborator(ctx, freeProt, opUser.ID, api.CollabRoleOperator, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetServerProtected(ctx, freeProt, false); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.ClaimServer(ctx, freeProt, newOwner.ID, nil); err != nil || !ok {
		t.Fatalf("ClaimServer(unprotected) = %v, %v", ok, err)
	}
	if list, err := repo.ServerCollaborators(ctx, freeProt); err != nil || len(list) != 0 {
		t.Fatalf("ClaimServer left %d collaborators, want 0", len(list))
	}
}
