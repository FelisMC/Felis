package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"felis.lolicon.best/internal/metrics"
)

// ---- user CRUD ----

// handleListUsers is the admin-tier user list (GET /users). It gates on
// adminOnly, so the caller is already a verified admin principal.
func (a *API) handleListUsers(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	q := r.URL.Query()

	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	opts := ListUsersOpts{
		Query:  q.Get("query"),
		Role:   q.Get("role"),
		Hidden: q.Get("disabled"),
		Limit:  limit,
		Offset: offset,
	}

	users, total, err := a.Repo.ListUsers(r.Context(), opts)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if users == nil {
		users = []UserView{}
	}

	_ = p // admin check done by adminOnly middleware
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "total": total})
}

// handleGetUser is the admin-tier user detail (GET /users/{id}).
func (a *API) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, errBadRequest)
		return
	}
	d, err := a.Repo.UserDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "user not found"))
			return
		}
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// createUserRequest is the admin create-user form.
type createUserRequest struct {
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	Role     string `json:"role"`
}

// handleCreateUser is the admin-tier create-user endpoint (POST /users).
func (a *API) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())

	var body createUserRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	// Validate username: 1–32 alphanumeric + limited symbols, no whitespace.
	if err := validateUsername(body.Username); err != nil {
		writeError(w, r, err)
		return
	}

	// Validate role.
	if body.Role != "admin" && body.Role != "user" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"role must be 'admin' or 'user', got %q", body.Role))
		return
	}

	u, err := a.Repo.CreateUser(r.Context(), CreateUserInput(body), p.Email)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "already_exists",
				"username %q is already taken", body.Username))
			return
		}
		writeError(w, r, err)
		return
	}

	a.audit(r, "user.create", u.ID)
	writeJSON(w, http.StatusCreated, u)
}

// patchUserRequest is the admin patch-user form. Every field is a pointer so
// "absent" is distinguishable from "set to empty".
type patchUserRequest struct {
	Username *string `json:"username,omitempty"`
	Email    *string `json:"email,omitempty"`
	Role     *string `json:"role,omitempty"`
}

// handlePatchUser is the admin-tier patch-user endpoint (PATCH /users/{id}).
// The two refusals an admin meets on the user page get codes of their own, so
// the panel can say why instead of a bare "not allowed": acting on your own
// account (a slip that would lock you out), and changing the owner account,
// which only the local break-glass console (sudo felis breakGlass) may do.
const (
	codeSelfProtected  = "self_protected"
	codeOwnerProtected = "owner_protected"
)

func (a *API) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, errBadRequest)
		return
	}

	var body patchUserRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	if body.Username == nil && body.Email == nil && body.Role == nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"patch must set at least one field"))
		return
	}

	// Self-demotion guard: an admin/owner may edit their own email or username,
	// but must never downgrade themselves to a lower role.
	if body.Role != nil && id == p.UserID && *body.Role != p.Role {
		writeError(w, r, newError(http.StatusForbidden, codeSelfProtected,
			"cannot change your own role"))
		return
	}

	// Owner protection (migration 0011): the owner row is the one identity the
	// panel may never demote — only the local break-glass console resets it.
	// Username/email edits on it stay allowed. A failed detail read falls through;
	// UpdateUser then answers the real 404.
	if body.Role != nil && *body.Role != "owner" {
		if d, err := a.Repo.UserDetail(r.Context(), id); err == nil && d.Role == "owner" {
			writeError(w, r, newError(http.StatusForbidden, codeOwnerProtected,
				"the owner account's role cannot be changed from the panel"))
			return
		}
	}

	if body.Username != nil {
		if err := validateUsername(*body.Username); err != nil {
			writeError(w, r, err)
			return
		}
	}
	if body.Role != nil && *body.Role != "admin" && *body.Role != "user" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"role must be 'admin' or 'user', got %q", *body.Role))
		return
	}

	u, err := a.Repo.UpdateUser(r.Context(), id, UpdateUserInput(body), p.Email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "user not found"))
			return
		}
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "already_exists",
				"username is already taken"))
			return
		}
		writeError(w, r, err)
		return
	}

	a.audit(r, "user.patch", id)
	writeJSON(w, http.StatusOK, u)
}

// handleDeleteUser is the admin-tier soft-delete endpoint (DELETE /users/{id}).
func (a *API) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, errBadRequest)
		return
	}

	if id == p.UserID {
		writeError(w, r, newError(http.StatusForbidden, codeSelfProtected,
			"cannot delete your own account"))
		return
	}

	// Same owner protection as the role guard above: only break-glass retires the
	// owner identity. A failed detail read falls through to the real 404.
	if d, err := a.Repo.UserDetail(r.Context(), id); err == nil && d.Role == "owner" {
		writeError(w, r, newError(http.StatusForbidden, codeOwnerProtected,
			"the owner account cannot be deleted from the panel"))
		return
	}

	if err := a.Repo.DeleteUser(r.Context(), id, p.Email); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "user not found"))
			return
		}
		writeError(w, r, err)
		return
	}

	a.audit(r, "user.delete", id)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleDisableUser is the admin-tier disable/enable toggle (POST /users/{id}/disable).
func (a *API) handleDisableUser(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, errBadRequest)
		return
	}

	if id == p.UserID {
		writeError(w, r, newError(http.StatusForbidden, codeSelfProtected,
			"cannot disable your own account"))
		return
	}

	var body struct {
		Disabled bool `json:"disabled"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	// Owner protection (migration 0011): disabling locks the owner out and revokes
	// its sessions — effectively a demotion, so the panel refuses it; only
	// break-glass touches the owner identity. Re-enabling stays allowed.
	if body.Disabled {
		if d, err := a.Repo.UserDetail(r.Context(), id); err == nil && d.Role == "owner" {
			writeError(w, r, newError(http.StatusForbidden, codeOwnerProtected,
				"the owner account cannot be disabled from the panel"))
			return
		}
	}

	if err := a.Repo.SetUserDisabled(r.Context(), id, body.Disabled); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "user not found"))
			return
		}
		writeError(w, r, err)
		return
	}

	action := "user.enable"
	if body.Disabled {
		action = "user.disable"
	}
	a.audit(r, action, id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "disabled": body.Disabled})
}

// ---- quota admin ----

// handleGetQuotas is the admin-tier quotas read (GET /users/{id}/quotas).
func (a *API) handleGetQuotas(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, errBadRequest)
		return
	}
	v, err := a.Repo.GetQuotas(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "user not found"))
			return
		}
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleSetQuotas is the admin-tier quotas write (PUT /users/{id}/quotas).
func (a *API) handleSetQuotas(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, errBadRequest)
		return
	}

	var body QuotaInput
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	// Reject a body where every field is nil — a silent no-op is a client mistake.
	if body.MaxServers == nil && body.MaxCPUMilli == nil && body.MaxMemoryMB == nil && body.MaxStorageGB == nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"at least one quota field must be set"))
		return
	}

	v, err := a.Repo.SetQuotas(r.Context(), id, body, p.Email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "user not found"))
			return
		}
		writeError(w, r, err)
		return
	}

	a.audit(r, "user.set_quotas", id)
	writeJSON(w, http.StatusOK, v)
}

// ---- session admin ----

// handleListUserSessions lists every live session for a user (GET /users/{id}/sessions).
func (a *API) handleListUserSessions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, errBadRequest)
		return
	}
	sessions, err := a.Repo.ListUserSessions(r.Context(), id, a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if sessions == nil {
		sessions = []SessionView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// handleRevokeUserSessions revokes every live session of a user
// (DELETE /users/{id}/sessions).
func (a *API) handleRevokeUserSessions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, errBadRequest)
		return
	}

	if err := a.Repo.RevokeAllUserSessions(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}

	metrics.SessionsRevokedTotal.WithLabelValues("admin").Inc()
	a.audit(r, "user.revoke_sessions", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRevokeUserSession revokes a single session of a user
// (DELETE /users/{id}/sessions/{hash}).
func (a *API) handleRevokeUserSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tokenHash := r.PathValue("hash")
	if id == "" || tokenHash == "" {
		writeError(w, r, errBadRequest)
		return
	}

	if err := a.Repo.RevokeUserSession(r.Context(), id, tokenHash); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "session_not_found",
				"that session has already ended or does not belong to this user"))
			return
		}
		writeError(w, r, err)
		return
	}

	metrics.SessionsRevokedTotal.WithLabelValues("admin").Inc()
	a.audit(r, "user.revoke_session", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUnbindUserPasskeys unbinds every passkey a user holds
// (DELETE /users/{id}/passkeys). It is the admin account-remediation for a
// compromised authenticator: a passkey planted (or retained) via a transiently
// hijacked session is a standing login foothold that outlives a mere session
// revoke, so severing it needs its own owner-tier action. It is deliberately NOT a
// lockout — the account keeps every other way back in: a player re-enters through
// the email-OTP door and re-enrolls, an operator through op-login's in-game
// approval — so an owner can cut a bad credential without stranding the account.
// DeleteAllPasskeyCredentialsForUser treats removing zero rows as success, so
// unbinding an account that holds no passkeys is a 200 no-op, not a 404.
func (a *API) handleUnbindUserPasskeys(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, errBadRequest)
		return
	}

	if err := a.Repo.DeleteAllPasskeyCredentialsForUser(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}

	a.audit(r, "user.unbind_passkeys", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- account-link admin ----

// handleUnlinkAccount removes a single (user_id, mc_uuid) binding
// (DELETE /users/{id}/links/{mc_uuid}).
func (a *API) handleUnlinkAccount(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	mcUUID := r.PathValue("mc_uuid")
	if userID == "" || mcUUID == "" {
		writeError(w, r, errBadRequest)
		return
	}

	if err := a.Repo.UnlinkAccount(r.Context(), userID, mcUUID); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found",
				"no linked account for this UUID"))
			return
		}
		writeError(w, r, err)
		return
	}

	a.audit(r, "user.unlink_account", userID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mc_uuid": mcUUID})
}

// handleLinkAccount force-binds a UUID to a user
// (POST /users/{id}/links).
func (a *API) handleLinkAccount(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	if userID == "" {
		writeError(w, r, errBadRequest)
		return
	}

	var body struct {
		MCUUID     string `json:"mc_uuid"`
		AuthSource string `json:"auth_source"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.MCUUID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"mc_uuid is required"))
		return
	}
	if body.AuthSource == "" {
		// Same version-nibble inference as the mint path (handlers_account.go):
		// defaulting to mojang here would leave a force-linked thirdparty UUID
		// outside the reclaim guard.
		body.AuthSource = deriveAuthSource(body.MCUUID)
	}
	if !validAuthSource(body.AuthSource) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"auth_source must be %q or %q", authSourceMojang, authSourceThirdParty))
		return
	}

	if err := a.Repo.LinkAccount(r.Context(), userID, body.MCUUID, body.AuthSource); err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "already_linked",
				"this UUID is already linked to a different user"))
			return
		}
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "user not found"))
			return
		}
		writeError(w, r, err)
		return
	}

	a.audit(r, "user.link_account", userID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"mc_uuid":     body.MCUUID,
		"auth_source": body.AuthSource,
	})
}

// ---- validation ----

// validateUsername checks that name is a non-empty string of 1–32 characters
// consisting only of lowercase alphanumerics, hyphens, underscores, and dots,
// and without leading/trailing hyphens or consecutive dots.
func validateUsername(name string) error {
	if len(name) == 0 || len(name) > 32 {
		return newError(http.StatusBadRequest, "bad_request",
			"username must be 1–32 characters")
	}
	if strings.TrimSpace(name) != name {
		return newError(http.StatusBadRequest, "bad_request",
			"username must not contain leading or trailing whitespace")
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return newError(http.StatusBadRequest, "bad_request",
				"username contains invalid character %q", c)
		}
	}
	return nil
}
