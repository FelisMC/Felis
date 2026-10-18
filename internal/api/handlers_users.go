package api

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"felis.lolicon.best/internal/metrics"
)

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

	_ = p
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "total": total})
}

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

type createUserRequest struct {
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	Role     string `json:"role"`
}

func (a *API) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())

	var body createUserRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	if err := validateUsername(body.Username); err != nil {
		writeError(w, r, err)
		return
	}

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

// patchUserRequest uses nil for absent fields and pointers to empty values for explicit clearing.
type patchUserRequest struct {
	Username *string `json:"username,omitempty"`
	Email    *string `json:"email,omitempty"`
	Role     *string `json:"role,omitempty"`
}

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

	// Prevent self-demotion while allowing username and email edits.
	if body.Role != nil && id == p.UserID && *body.Role != p.Role {
		writeError(w, r, newError(http.StatusForbidden, codeSelfProtected,
			"cannot change your own role"))
		return
	}

	// Only local break-glass recovery may demote the owner; profile edits remain allowed.
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
	// Changing the caller's email removes an account factor and requires reauth.
	if body.Email != nil && id == p.UserID && !a.requireReauth(w, r, p) {
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

	// Only local break-glass recovery may retire the owner identity.
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

	// Disabling the owner revokes its sessions, so the panel permits only re-enabling it.
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

	// Quota writes replace all four caps; an empty body clears them all.
	for _, f := range []struct {
		name string
		v    *int
	}{
		{"max_servers", body.MaxServers},
		{"max_cpu_milli", body.MaxCPUMilli},
		{"max_memory_mb", body.MaxMemoryMB},
		{"max_storage_gb", body.MaxStorageGB},
	} {
		if f.v != nil && (*f.v < 0 || *f.v > math.MaxInt32) {
			writeError(w, r, newError(http.StatusBadRequest, "invalid_quota",
				"%s must be a whole number from 0 to 2147483647, or null for unlimited", f.name))
			return
		}
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

func (a *API) handleClearQuotas(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, errBadRequest)
		return
	}
	v, err := a.Repo.ClearQuotas(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "user not found"))
			return
		}
		writeError(w, r, err)
		return
	}
	a.audit(r, "user.clear_quotas", id)
	writeJSON(w, http.StatusOK, v)
}

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

// handleUnbindUserPasskeys is idempotent and preserves other login methods; session revocation alone cannot remove compromised passkeys.
func (a *API) handleUnbindUserPasskeys(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, r, errBadRequest)
		return
	}
	// Removing the caller's passkeys requires the same reauth as self-service removal.
	if p := principalFromContext(r.Context()); id == p.UserID && !a.requireReauth(w, r, p) {
		return
	}

	if err := a.Repo.DeleteAllPasskeyCredentialsForUser(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}

	a.audit(r, "user.unbind_passkeys", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) handleUnlinkAccount(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	if userID == "" {
		writeError(w, r, errBadRequest)
		return
	}
	mcUUID, err := parseMCUUID(r.PathValue("mc_uuid"))
	if err != nil {
		writeError(w, r, err)
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
	mcUUID, err := parseMCUUID(body.MCUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if body.AuthSource == "" {
		// Infer third-party UUID sources as on code minting so force-linked accounts retain reclaim protection.
		body.AuthSource = deriveAuthSource(mcUUID)
	}
	if !validAuthSource(body.AuthSource) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"auth_source must be %q or %q", authSourceMojang, authSourceThirdParty))
		return
	}

	if err := a.Repo.LinkAccount(r.Context(), userID, mcUUID, body.AuthSource); err != nil {
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
		"mc_uuid":     mcUUID,
		"auth_source": body.AuthSource,
	})
}

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
