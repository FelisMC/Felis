package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Staff op-login requires a mailbox code and approval from an in-game staff identity.

// otpPurposeOpLogin isolates staff codes from player-login and onboarding codes.
const otpPurposeOpLogin = "op_login"

type opLoginStartRequest struct {
	Email string `json:"email"`
}

// handleOpLoginStart preserves existing codes and returns indistinguishable responses for unknown or non-staff addresses.
func (a *API) handleOpLoginStart(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req opLoginStartRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	email := strings.TrimSpace(req.Email)
	if !looksLikeEmail(email) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "a valid email is required"))
		return
	}

	// Check the mail budget before resolving the address so every address receives the same 429.
	if err := a.checkMailBudget(r.Context()); err != nil {
		writeError(w, r, err)
		return
	}

	// Reserve before lookup; unknown addresses retain the cooldown, scoped separately from player login.
	emailKey := "oplogin:email:" + strings.ToLower(email)
	lim := a.otpLimiter()
	emailAt, fresh := lim.reserve(emailKey, otpResendCooldown)
	committed := false
	if fresh {
		defer func() {
			if !committed {
				lim.release(emailKey, emailAt)
			}
		}()
	}

	// Use the reservation's expiry for responses, requests and OTPs, including starts within the cooldown.
	expiresAt := emailAt.Add(otpTTL)

	// Unknown addresses retain the cooldown and receive a random, unpersisted handle to prevent enumeration.
	neutral := func() {
		fakeID, err := newOTPID()
		if err != nil {
			writeError(w, r, err)
			return
		}
		committed = true
		writeJSON(w, http.StatusAccepted, map[string]any{
			"request_id": fakeID, "expires_at": expiresAt.UTC(),
		})
	}

	u, err := a.Repo.UserByEmail(r.Context(), email)
	switch {
	case errors.Is(err, ErrNotFound):
		a.authFailure(r, "op_login", "no_account", nil)
		neutral()
		return
	case err != nil:
		// Release the cooldown on store failures so an outage does not consume it.
		writeError(w, r, err)
		return
	}
	if !staffRole(u.Role) {
		a.authFailure(r, "op_login", "not_staff", u)
		neutral()
		return
	}
	// Locked accounts receive the same response without a request or email.
	switch until, err := a.Repo.OTPLockedUntil(r.Context(), u.ID, otpPurposeOpLogin, a.now()); {
	case err != nil:
		writeError(w, r, err)
		return
	case !until.IsZero():
		neutral()
		return
	}

	id, err := newOTPID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	origin := ""
	if addr := a.clientIP(r); addr.IsValid() {
		origin = addr.String()
	}
	if err := a.Repo.CreateOpLoginRequest(r.Context(), NewOpLoginRequest{
		ID: id, UserID: u.ID, Email: u.Email, ExpiresAt: expiresAt,
		ClientIP: origin, UserAgent: truncateUTF8(r.UserAgent(), maxSessionUserAgent),
	}); err != nil {
		writeError(w, r, err)
		return
	}
	if !fresh {
		// Requests created within the cooldown share the existing code and its expiry.
		a.auditAccount(r, u, "auth.op_login.requested", "")
		writeJSON(w, http.StatusAccepted, map[string]any{
			"request_id": id, "expires_at": expiresAt.UTC(),
		})
		return
	}
	code, err := newEmailOTP()
	if err != nil {
		writeError(w, r, err)
		return
	}
	otpID, err := newOTPID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Deliver to the stored address; lookup is case-insensitive.
	if err := a.Repo.AddLoginEmailOTP(r.Context(), otpID, u.ID, u.Email, otpCodeHash(code), otpPurposeOpLogin, a.now(), expiresAt); err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.deliverOTP(r.Context(), u.Email, code); err != nil {
		writeError(w, r, err)
		return
	}
	committed = true
	a.auditAccount(r, u, "auth.op_login.otp_sent", "")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"request_id": id, "expires_at": expiresAt.UTC(),
	})
}

// handleOpLoginStatus returns approved:false for unknown, expired, denied or consumed requests.
func (a *API) handleOpLoginStatus(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	id := r.PathValue("id")
	approved := false
	switch req, err := a.Repo.OpLoginRequestByID(r.Context(), id); {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		writeError(w, r, err)
		return
	default:
		approved = req.Status == "approved" && !req.Consumed && req.ExpiresAt.After(a.now())
	}
	writeJSON(w, http.StatusOK, map[string]any{"approved": approved})
}

type opLoginFinishRequest struct {
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
}

// handleOpLoginFinish requires both factors and returns one uniform failure for invalid, locked or raced requests.
func (a *API) handleOpLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req opLoginFinishRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	requestID := strings.TrimSpace(req.RequestID)
	code := strings.TrimSpace(req.Code)
	if requestID == "" || code == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "request_id and code are required"))
		return
	}

	invalid := newError(http.StatusBadRequest, "op_login_invalid",
		"this operator login could not be completed; restart the sign-in")

	now := a.now()
	loginReq, err := a.Repo.OpLoginRequestByID(r.Context(), requestID)
	switch {
	case errors.Is(err, ErrNotFound):
		a.authFailure(r, "op_login", "unknown_request", nil)
		writeError(w, r, invalid)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	// Check approval before consuming the code so an early submission can retry.
	if loginReq.Status != "approved" || loginReq.Consumed || !loginReq.ExpiresAt.After(now) {
		a.authFailure(r, "op_login", "not_approved", a.opLoginAccount(r, loginReq.UserID))
		writeError(w, r, invalid)
		return
	}
	// Recheck account status before consuming the code; the account may have been disabled since start.
	d, err := a.Repo.UserDetail(r.Context(), loginReq.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if d.Disabled {
		a.authFailure(r, "op_login", "account_retired", a.opLoginAccount(r, loginReq.UserID))
		writeError(w, r, invalid)
		return
	}
	// A failed code attempt leaves the approved request available for retry.
	switch err := a.Repo.ConsumeLoginEmailOTP(r.Context(), loginReq.UserID, otpPurposeOpLogin, otpCodeHash(code), now); {
	case errors.Is(err, ErrOTPInvalid), errors.Is(err, ErrOTPLocked), errors.Is(err, ErrOTPAccountLocked):
		a.noteOTPLock(r, err, loginReq.UserID, otpPurposeOpLogin)
		a.authFailure(r, "op_login", otpFailureReason(err), a.opLoginAccount(r, loginReq.UserID))
		writeError(w, r, invalid)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	// Atomically consume the request so concurrent finishes cannot mint multiple sessions.
	switch err := a.Repo.ConsumeOpLoginRequest(r.Context(), requestID, now); {
	case errors.Is(err, ErrNotFound):
		writeError(w, r, invalid)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	// Recheck the staff role before minting a session, even for an approved request.
	u, err := a.Repo.UserByID(r.Context(), loginReq.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !staffRole(u.Role) {
		a.authFailure(r, "op_login", "not_staff", u)
		writeError(w, r, newError(http.StatusForbidden, "staff_account", "that account is not an operator"))
		return
	}
	if err := a.startSession(w, r, u.ID, provenSignIn); err != nil {
		writeError(w, r, err)
		return
	}
	a.auditAccount(r, u, "auth.op_login", "")
	writeJSON(w, http.StatusOK, map[string]any{"user_id": u.ID, "role": u.Role})
}

// opLoginAccount falls back to the account ID when loading audit details fails.
func (a *API) opLoginAccount(r *http.Request, userID string) *StaffUser {
	if u, err := a.Repo.UserByID(r.Context(), userID); err == nil {
		return u
	}
	return &StaffUser{ID: userID}
}

func (a *API) handleOpLoginPending(w http.ResponseWriter, r *http.Request) {
	reqs, err := a.Repo.ListPendingOpLogins(r.Context(), a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, map[string]any{
			"request_id": req.ID,
			"username":   req.Username,
			"email":      req.Email,
			"client_ip":  req.ClientIP,
			"created_at": req.CreatedAt.UTC(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": out})
}

// opLoginApprover returns the same 403 for unlinked and non-staff game identities.
func (a *API) opLoginApprover(r *http.Request, mcUUID string) (*StaffUser, error) {
	notAdmin := newError(http.StatusForbidden, "not_admin", "only a linked administrator may approve an operator login")
	approverID, err := a.Repo.UserByMCUUID(r.Context(), mcUUID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, notAdmin
	case err != nil:
		return nil, err
	}
	approver, err := a.Repo.UserByID(r.Context(), approverID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, notAdmin
	case err != nil:
		return nil, err
	}
	if !staffRole(approver.Role) {
		return nil, notAdmin
	}
	return approver, nil
}

// pendingOpLogin returns the same 404 for all requests that cannot be approved.
func (a *API) pendingOpLogin(r *http.Request, id string) (*OpLoginRequest, error) {
	notFound := newError(http.StatusNotFound, "op_login_not_found", "no pending operator login with that id")
	req, err := a.Repo.OpLoginRequestByID(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, notFound
	case err != nil:
		return nil, err
	}
	if req.Status != "pending" || req.Consumed || !req.ExpiresAt.After(a.now()) {
		return nil, notFound
	}
	return req, nil
}

// handleOpLoginShow requires a linked staff approver because the Velocity command is available to all players.
func (a *API) handleOpLoginShow(w http.ResponseWriter, r *http.Request) {
	approverUUID := strings.TrimSpace(r.URL.Query().Get("approver_uuid"))
	if approverUUID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "approver_uuid is required"))
		return
	}
	if _, err := a.opLoginApprover(r, approverUUID); err != nil {
		writeError(w, r, err)
		return
	}
	req, err := a.pendingOpLogin(r, r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id": req.ID,
		"username":   req.Username,
		"email":      req.Email,
		"client_ip":  req.ClientIP,
		"user_agent": req.UserAgent,
		"created_at": req.CreatedAt.UTC(),
		"expires_at": req.ExpiresAt.UTC(),
	})
}

type opLoginApproveRequest struct {
	ApproverUUID string `json:"approver_uuid"`
	Username     string `json:"username"`
}

// handleOpLoginApprove permits self-approval; control of the in-game staff identity is the second factor.
func (a *API) handleOpLoginApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req opLoginApproveRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	approverUUID := strings.TrimSpace(req.ApproverUUID)
	typed := strings.TrimSpace(req.Username)
	if approverUUID == "" || typed == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "approver_uuid and username are required"))
		return
	}
	approver, err := a.opLoginApprover(r, approverUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	loginReq, err := a.pendingOpLogin(r, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !strings.EqualFold(typed, loginReq.Username) {
		payload, _ := json.Marshal(map[string]string{"request_id": id, "typed_username": typed})
		a.auditEntry(r, AuditEntry{
			Actor: approver.Username, ActorUserID: approver.ID, Source: internalSource(r),
			Action: "auth.op_login.approve_mismatch", Payload: payload,
		})
		writeError(w, r, newError(http.StatusConflict, "op_login_mismatch",
			"that operator login is for a different account"))
		return
	}
	switch err := a.Repo.ApproveOpLogin(r.Context(), id, approver.ID, a.now()); {
	case errors.Is(err, ErrNotFound):
		writeError(w, r, newError(http.StatusNotFound, "op_login_not_found", "no pending operator login with that id"))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"request_id": id, "approver_user_id": approver.ID,
		"username": loginReq.Username, "client_ip": loginReq.ClientIP,
	})
	a.auditEntry(r, AuditEntry{
		Actor: approver.Username, ActorUserID: approver.ID, Source: internalSource(r),
		Action: "auth.op_login.approved", Payload: payload,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"approved": true, "username": loginReq.Username, "email": loginReq.Email,
	})
}
