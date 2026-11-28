package api

import (
	"context"
	"net/http"

	"felis.lolicon.best/internal/naming"
)

type ServerAction int

const (
	ActionViewStatus ServerAction = iota
	ActionOperate
	ActionManage
	ActionAdminDestructive
)

const (
	CollabRoleOperator = "operator"
	CollabRoleManager  = "manager"
)

const (
	AccessRoleOwner    = "owner"
	AccessRoleAdmin    = "admin"
	AccessRoleManager  = "manager"
	AccessRoleOperator = "operator"
	AccessRoleReadonly = "readonly"
	AccessRoleNone     = ""
)

type serverAccessInput struct {
	UserID       string
	PlatformRole string
	ServerOwner  string
	Protected    bool
	CollabRole   string
	System       bool
	Action       ServerAction
}

func validCollaboratorRole(role string) bool {
	return role == CollabRoleOperator || role == CollabRoleManager
}

func effectiveAccessRole(in serverAccessInput) string {
	switch in.PlatformRole {
	case "owner":
		return AccessRoleOwner
	case "admin":
		if !in.Protected || in.System {
			return AccessRoleAdmin
		}
		if validCollaboratorRole(in.CollabRole) {
			return in.CollabRole
		}
		return AccessRoleReadonly
	default:
		if in.System {
			return AccessRoleNone
		}
		if in.Protected {
			if validCollaboratorRole(in.CollabRole) {
				return in.CollabRole
			}
			return AccessRoleNone
		}
		if in.ServerOwner != "" && in.ServerOwner == in.UserID {
			return AccessRoleOwner
		}
		if validCollaboratorRole(in.CollabRole) {
			return in.CollabRole
		}
		return AccessRoleNone
	}
}

func serverAccessAllowed(in serverAccessInput) bool {
	role := effectiveAccessRole(in)
	switch in.Action {
	case ActionViewStatus:
		return role != AccessRoleNone
	case ActionOperate:
		return role == AccessRoleOwner || role == AccessRoleAdmin || role == AccessRoleManager || role == AccessRoleOperator
	case ActionManage:
		return role == AccessRoleOwner || role == AccessRoleAdmin || role == AccessRoleManager
	case ActionAdminDestructive:
		if in.PlatformRole == "owner" {
			return true
		}
		return in.PlatformRole == "admin" && (!in.Protected || in.System)
	default:
		return false
	}
}

func canManageCollaborators(platformRole string, protected bool) bool {
	if platformRole == "owner" {
		return true
	}
	return platformRole == "admin" && !protected
}

func canRetireServer(p *Principal, rec *ServerRecord, del bool) bool {
	if p == nil || rec == nil {
		return false
	}
	if p.IsOwner() {
		return true
	}
	if rec.Protected {
		return false
	}
	if p.IsAdmin() {
		return true
	}
	return !del && rec.OwnerID != "" && rec.OwnerID == p.UserID
}

func principalPlatformRole(p *Principal) string {
	if p.IsOwner() {
		return "owner"
	}
	if p.IsAdmin() {
		return "admin"
	}
	return "user"
}

func (a *API) resolveServerAccess(ctx context.Context, p *Principal, name string, rec *ServerRecord) (accessRole, collabRole string, err error) {
	if p == nil {
		return AccessRoleNone, "", nil
	}
	system := naming.IsSystemServer(name)
	var serverOwner string
	var protected bool
	if rec != nil {
		serverOwner = rec.OwnerID
		protected = rec.Protected
	}
	if !system && !p.IsOwner() && p.UserID != "" {
		collabRole, err = a.Repo.UserCollaboratorRole(ctx, name, p.UserID)
		if err != nil {
			return AccessRoleNone, "", err
		}
	}
	role := effectiveAccessRole(serverAccessInput{
		UserID:       p.UserID,
		PlatformRole: principalPlatformRole(p),
		ServerOwner:  serverOwner,
		Protected:    protected,
		CollabRole:   collabRole,
		System:       system,
	})
	return role, collabRole, nil
}

func (a *API) checkServerAccess(ctx context.Context, p *Principal, name string, rec *ServerRecord, action ServerAction) (bool, string, error) {
	if p == nil {
		return false, "", nil
	}
	system := naming.IsSystemServer(name)
	var serverOwner string
	var protected bool
	if rec != nil {
		serverOwner = rec.OwnerID
		protected = rec.Protected
	}
	var collabRole string
	if !system && !p.IsOwner() && p.UserID != "" {
		var err error
		collabRole, err = a.Repo.UserCollaboratorRole(ctx, name, p.UserID)
		if err != nil {
			return false, "", err
		}
	}
	allowed := serverAccessAllowed(serverAccessInput{
		UserID:       p.UserID,
		PlatformRole: principalPlatformRole(p),
		ServerOwner:  serverOwner,
		Protected:    protected,
		CollabRole:   collabRole,
		System:       system,
		Action:       action,
	})
	return allowed, collabRole, nil
}

func (a *API) authorizeServer(w http.ResponseWriter, r *http.Request, action ServerAction) (*ServerRecord, string, bool) {
	return a.authorizeServerName(w, r, r.PathValue("name"), action)
}

func (a *API) authorizeServerName(w http.ResponseWriter, r *http.Request, name string, action ServerAction) (*ServerRecord, string, bool) {
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return nil, "", false
	}
	rec, err := a.managedServerRecord(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return nil, "", false
	}
	p := principalFromContext(r.Context())
	allowed, collabRole, err := a.checkServerAccess(r.Context(), p, name, rec, action)
	if err != nil {
		writeError(w, r, err)
		return nil, "", false
	}
	if !allowed {
		writeError(w, r, errForbidden)
		return nil, "", false
	}
	return rec, collabRole, true
}
