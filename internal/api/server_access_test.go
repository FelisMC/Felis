package api

import "testing"

func TestPrincipalPlatformRole(t *testing.T) {
	cases := []struct {
		name string
		p    *Principal
		want string
	}{
		{"nil", nil, "user"},
		{"owner with admin access", &Principal{Role: "owner", ViaAdminAccess: true}, "owner"},
		{"owner without admin access", &Principal{Role: "owner", ViaAdminAccess: false}, "user"},
		{"admin with admin access", &Principal{Role: "admin", ViaAdminAccess: true}, "admin"},
		{"admin without admin access", &Principal{Role: "admin", ViaAdminAccess: false}, "user"},
		{"regular user", &Principal{Role: "user", ViaAdminAccess: false}, "user"},
	}
	for _, tc := range cases {
		if got := principalPlatformRole(tc.p); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEffectiveAccessRoleAndPermissions(t *testing.T) {
	cases := []struct {
		name            string
		in              serverAccessInput
		wantRole        string
		wantView        bool
		wantOperate     bool
		wantManage      bool
		wantDestructive bool
	}{
		{
			name:            "owner on unprotected",
			in:              serverAccessInput{UserID: "o1", PlatformRole: "owner"},
			wantRole:        AccessRoleOwner,
			wantView:        true,
			wantOperate:     true,
			wantManage:      true,
			wantDestructive: true,
		},
		{
			name:            "owner on protected",
			in:              serverAccessInput{UserID: "o1", PlatformRole: "owner", Protected: true},
			wantRole:        AccessRoleOwner,
			wantView:        true,
			wantOperate:     true,
			wantManage:      true,
			wantDestructive: true,
		},
		{
			name:            "admin on unprotected",
			in:              serverAccessInput{UserID: "a1", PlatformRole: "admin"},
			wantRole:        AccessRoleAdmin,
			wantView:        true,
			wantOperate:     true,
			wantManage:      true,
			wantDestructive: true,
		},
		{
			name:            "admin on system server ignoring protected flag",
			in:              serverAccessInput{UserID: "a1", PlatformRole: "admin", Protected: true, System: true},
			wantRole:        AccessRoleAdmin,
			wantView:        true,
			wantOperate:     true,
			wantManage:      true,
			wantDestructive: true,
		},
		{
			name:            "unauthorized admin on protected is readonly",
			in:              serverAccessInput{UserID: "a1", PlatformRole: "admin", Protected: true},
			wantRole:        AccessRoleReadonly,
			wantView:        true,
			wantOperate:     false,
			wantManage:      false,
			wantDestructive: false,
		},
		{
			name:            "admin operator collaborator on protected",
			in:              serverAccessInput{UserID: "a1", PlatformRole: "admin", Protected: true, CollabRole: CollabRoleOperator},
			wantRole:        AccessRoleOperator,
			wantView:        true,
			wantOperate:     true,
			wantManage:      false,
			wantDestructive: false,
		},
		{
			name:            "admin manager collaborator on protected",
			in:              serverAccessInput{UserID: "a1", PlatformRole: "admin", Protected: true, CollabRole: CollabRoleManager},
			wantRole:        AccessRoleManager,
			wantView:        true,
			wantOperate:     true,
			wantManage:      true,
			wantDestructive: false,
		},
		{
			name:            "server owner on unprotected",
			in:              serverAccessInput{UserID: "u1", PlatformRole: "user", ServerOwner: "u1"},
			wantRole:        AccessRoleOwner,
			wantView:        true,
			wantOperate:     true,
			wantManage:      true,
			wantDestructive: false,
		},
		{
			name:            "server owner on protected without collaborator is blocked",
			in:              serverAccessInput{UserID: "u1", PlatformRole: "user", ServerOwner: "u1", Protected: true},
			wantRole:        AccessRoleNone,
			wantView:        false,
			wantOperate:     false,
			wantManage:      false,
			wantDestructive: false,
		},
		{
			name:            "server owner on protected with operator collaborator gets operator",
			in:              serverAccessInput{UserID: "u1", PlatformRole: "user", ServerOwner: "u1", Protected: true, CollabRole: CollabRoleOperator},
			wantRole:        AccessRoleOperator,
			wantView:        true,
			wantOperate:     true,
			wantManage:      false,
			wantDestructive: false,
		},
		{
			name:            "regular user with operator collaborator",
			in:              serverAccessInput{UserID: "u2", PlatformRole: "user", ServerOwner: "u1", CollabRole: CollabRoleOperator},
			wantRole:        AccessRoleOperator,
			wantView:        true,
			wantOperate:     true,
			wantManage:      false,
			wantDestructive: false,
		},
		{
			name:            "regular user with manager collaborator",
			in:              serverAccessInput{UserID: "u2", PlatformRole: "user", ServerOwner: "u1", CollabRole: CollabRoleManager},
			wantRole:        AccessRoleManager,
			wantView:        true,
			wantOperate:     true,
			wantManage:      true,
			wantDestructive: false,
		},
		{
			name:            "regular user on system server ignores collaborator",
			in:              serverAccessInput{UserID: "u2", PlatformRole: "user", CollabRole: CollabRoleManager, System: true},
			wantRole:        AccessRoleNone,
			wantView:        false,
			wantOperate:     false,
			wantManage:      false,
			wantDestructive: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveAccessRole(tc.in); got != tc.wantRole {
				t.Fatalf("effectiveAccessRole = %q, want %q", got, tc.wantRole)
			}
			check := func(action ServerAction, want bool) {
				t.Helper()
				in := tc.in
				in.Action = action
				if got := serverAccessAllowed(in); got != want {
					t.Errorf("serverAccessAllowed(%d) = %v, want %v", action, got, want)
				}
			}
			check(ActionViewStatus, tc.wantView)
			check(ActionOperate, tc.wantOperate)
			check(ActionManage, tc.wantManage)
			check(ActionAdminDestructive, tc.wantDestructive)
		})
	}
}

func TestCanManageCollaboratorsAndRetire(t *testing.T) {
	if !canManageCollaborators("owner", true) || !canManageCollaborators("owner", false) {
		t.Fatal("platform owner must always manage collaborators")
	}
	if !canManageCollaborators("admin", false) || canManageCollaborators("admin", true) {
		t.Fatal("platform admin may manage collaborators only on unprotected servers")
	}
	if canManageCollaborators("user", false) || canManageCollaborators("user", true) {
		t.Fatal("non-staff user must never manage collaborators")
	}

	po := &Principal{UserID: "po", Role: "owner", ViaAdminAccess: true}
	pa := &Principal{UserID: "pa", Role: "admin", ViaAdminAccess: true}
	paNoAccess := &Principal{UserID: "pa", Role: "admin", ViaAdminAccess: false}
	so := &Principal{UserID: "so", Role: "user"}
	unprotected := &ServerRecord{Name: "s1", OwnerID: "so", Protected: false}
	protected := &ServerRecord{Name: "s2", OwnerID: "so", Protected: true}

	if !canRetireServer(po, protected, true) || !canRetireServer(po, protected, false) {
		t.Fatal("platform owner must always retire or delete")
	}
	if !canRetireServer(pa, unprotected, true) || canRetireServer(pa, protected, true) || canRetireServer(pa, protected, false) {
		t.Fatal("admin retirement must be blocked on protected servers")
	}
	if canRetireServer(paNoAccess, unprotected, true) || canRetireServer(paNoAccess, unprotected, false) {
		t.Fatal("admin without ViaAdminAccess must not retire as admin")
	}
	if !canRetireServer(so, unprotected, false) || canRetireServer(so, unprotected, true) {
		t.Fatal("server owner may release unprotected server only")
	}
	if canRetireServer(so, protected, false) || canRetireServer(so, protected, true) {
		t.Fatal("server owner must not retire protected server")
	}
}
