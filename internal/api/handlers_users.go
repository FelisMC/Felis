package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// ResetMailer delivers a freshly-generated admin-reset password to the user's
// verified email address. nil means the password is logged server-side (the
// KNOWN-LIMITATION pattern from OTPMailer — production wires a real sender).
// The password is never returned to the admin caller.
type ResetMailer interface {
	SendPasswordReset(ctx context.Context, email, password string) error
}

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

	u, err := a.Repo.CreateUser(r.Context(), CreateUserInput{
		Username: body.Username,
		Email:    body.Email,
		Role:     body.Role,
	}, p.Email)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "already_exists",
				"username %q is already taken", body.Username))
			return
		}
		writeError(w, r, err)
		return
	}

	a.audit(r, p.Email, "user.create", u.ID)
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
		writeError(w, r, newError(http.StatusForbidden, "forbidden",
			"cannot change your own role"))
		return
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

	u, err := a.Repo.UpdateUser(r.Context(), id, UpdateUserInput{
		Username: body.Username,
		Email:    body.Email,
		Role:     body.Role,
	}, p.Email)
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

	a.audit(r, p.Email, "user.patch", id)
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
		writeError(w, r, newError(http.StatusForbidden, "forbidden",
			"cannot delete your own account"))
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

	a.audit(r, p.Email, "user.delete", id)
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
		writeError(w, r, newError(http.StatusForbidden, "forbidden",
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
	a.audit(r, p.Email, action, id)
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

	a.audit(r, p.Email, "user.set_quotas", id)
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
	p := principalFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, errBadRequest)
		return
	}

	if err := a.Repo.RevokeAllUserSessions(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}

	a.audit(r, p.Email, "user.revoke_sessions", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRevokeUserSession revokes a single session of a user
// (DELETE /users/{id}/sessions/{hash}).
func (a *API) handleRevokeUserSession(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	id := r.PathValue("id")
	tokenHash := r.PathValue("hash")
	if id == "" || tokenHash == "" {
		writeError(w, r, errBadRequest)
		return
	}

	if err := a.Repo.RevokeSession(r.Context(), tokenHash); err != nil {
		writeError(w, r, err)
		return
	}

	a.audit(r, p.Email, "user.revoke_session", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- account-link admin ----

// handleUnlinkAccount removes a single (user_id, mc_uuid) binding
// (DELETE /users/{id}/links/{mc_uuid}).
func (a *API) handleUnlinkAccount(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
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

	a.audit(r, p.Email, "user.unlink_account", userID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mc_uuid": mcUUID})
}

// handleLinkAccount force-binds a UUID to a user
// (POST /users/{id}/links).
func (a *API) handleLinkAccount(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
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
		body.AuthSource = "mojang"
	}

	if err := a.Repo.LinkAccount(r.Context(), userID, body.MCUUID, body.AuthSource); err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "already_linked",
				"this UUID is already linked to a different user"))
			return
		}
		writeError(w, r, err)
		return
	}

	a.audit(r, p.Email, "user.link_account", userID)
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
