package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Migration transfers server ownership and retires the source; game links, email and passkeys remain with their accounts.

const (
	// otpPurposeMigrate isolates migration OTPs from onboarding and login codes.
	otpPurposeMigrate = "migrate_confirm"
	// passkeyPurposeMigrate isolates migration assertions from login challenges.
	passkeyPurposeMigrate = "passkey_migrate"
	migrateCodeTTL        = 10 * time.Minute
	migrateConfirmWindow  = 10 * time.Minute
)

// migrationStage resets expired codes and confirmations from another session or outside migrateConfirmWindow to initiated.
func migrationStage(m *MigrationView, session string, now time.Time) string {
	switch m.State {
	case "confirmed":
		if m.ConfirmSession != session || m.ConfirmedAt == nil || !now.Before(m.ConfirmedAt.Add(migrateConfirmWindow)) {
			return "initiated"
		}
	case "code_issued":
		if m.CodeExpiresAt == nil || !now.Before(*m.CodeExpiresAt) {
			return "initiated"
		}
	}
	return m.State
}

func newMigrationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type migrateStartRequest struct {
	MCUUID string `json:"mc_uuid"`
}

// handleMigrateStart trusts the internal caller's verified game UUID; transfer still requires fresh web proof.
func (a *API) handleMigrateStart(w http.ResponseWriter, r *http.Request) {
	var req migrateStartRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	mcUUID, err := parseMCUUID(req.MCUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	sourceUserID, err := a.Repo.UserByMCUUID(r.Context(), mcUUID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_linked",
				"this in-game identity is not linked to a Felis account"))
			return
		}
		writeError(w, r, err)
		return
	}
	id, err := newMigrationID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.Repo.StartMigration(r.Context(), id, sourceUserID, a.now()); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusConflict, "account_retired",
				"the linked account can no longer start a migration"))
			return
		}
		writeError(w, r, err)
		return
	}
	a.auditEntry(r, AuditEntry{
		Actor:  "mc:" + mcUUID,
		Source: internalSource(r),
		Action: "account.migrate.start",
	})
	writeJSON(w, http.StatusCreated, map[string]any{"started": true, "state": "initiated"})
}

func (a *API) handleMigrateStatus(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	m, err := a.Repo.MigrationForSource(r.Context(), p.UserID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"active": false})
			return
		}
		writeError(w, r, err)
		return
	}
	stage := migrationStage(m, currentSessionHash(r), a.now())
	resp := map[string]any{"active": true, "state": stage}
	switch stage {
	case "confirmed":
		resp["confirm_factor"] = m.ConfirmFactor
		resp["confirm_expires_at"] = m.ConfirmedAt.Add(migrateConfirmWindow).UTC()
	case "code_issued":
		resp["confirm_factor"] = m.ConfirmFactor
		resp["target_user_id"] = m.TargetUserID
		resp["code_expires_at"] = m.CodeExpiresAt.UTC()
	}
	writeJSON(w, http.StatusOK, resp)
}

// requireInitiatedMigration writes the error response when returning ok=false.
func (a *API) requireInitiatedMigration(w http.ResponseWriter, r *http.Request, userID string) (*MigrationView, bool) {
	m, err := a.Repo.MigrationForSource(r.Context(), userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "no_migration",
				"no migration is in progress; start one in-game with /felis migrate"))
			return nil, false
		}
		writeError(w, r, err)
		return nil, false
	}
	if migrationStage(m, currentSessionHash(r), a.now()) != "initiated" {
		writeError(w, r, newError(http.StatusConflict, "already_confirmed",
			"this migration has already been confirmed"))
		return nil, false
	}
	return m, true
}

func (a *API) userHasPasskey(ctx context.Context, userID string) (bool, error) {
	creds, err := a.Repo.PasskeyCredentialsForUser(ctx, userID)
	if err != nil {
		return false, err
	}
	return len(creds) > 0, nil
}

// handleMigrateConfirmOTPStart refuses email when a passkey is enrolled to prevent factor downgrade.
func (a *API) handleMigrateConfirmOTPStart(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if _, ok := a.requireInitiatedMigration(w, r, p.UserID); !ok {
		return
	}
	hasPk, err := a.userHasPasskey(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if hasPk {
		writeError(w, r, newError(http.StatusConflict, "passkey_required",
			"this account has a passkey; confirm the migration with your passkey"))
		return
	}
	if p.Email == "" {
		writeError(w, r, newError(http.StatusConflict, "no_step_up_factor",
			"no verified email or passkey on this account to confirm the migration"))
		return
	}
	a.startStepUpOTP(w, r, p, otpPurposeMigrate, "migrate:confirm:", "account.migrate.confirm_otp_sent")
}

// handleMigrateConfirmOTPVerify consumes proof without changing the verified email address.
func (a *API) handleMigrateConfirmOTPVerify(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	var req stepUpOTPVerifyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "code is required"))
		return
	}
	if _, ok := a.requireInitiatedMigration(w, r, p.UserID); !ok {
		return
	}
	if !a.verifyStepUpOTP(w, r, p, otpPurposeMigrate, "migrate_confirm", code) {
		return
	}
	if err := a.Repo.ConfirmMigration(r.Context(), p.UserID, "email_otp", currentSessionHash(r), a.now()); err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "already_confirmed",
				"this migration has already been confirmed"))
			return
		}
		writeError(w, r, err)
		return
	}
	a.audit(r, "account.migrate.confirmed", "")
	writeJSON(w, http.StatusOK, map[string]any{"confirmed": true})
}

func (a *API) handleMigrateConfirmPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if a.Passkey == nil {
		writeError(w, r, errPasskeyUnavailable)
		return
	}
	if _, ok := a.requireInitiatedMigration(w, r, p.UserID); !ok {
		return
	}
	a.beginStepUpPasskey(w, r, p, passkeyPurposeMigrate,
		"no passkey enrolled; confirm the migration with an email code")
}

func (a *API) handleMigrateConfirmPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if a.Passkey == nil {
		writeError(w, r, errPasskeyUnavailable)
		return
	}
	var req stepUpPasskeyFinishRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if len(req.Assertion) == 0 {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "assertion is required"))
		return
	}
	if _, ok := a.requireInitiatedMigration(w, r, p.UserID); !ok {
		return
	}
	if !a.finishStepUpPasskey(w, r, p, passkeyPurposeMigrate, "migrate_passkey", req.Assertion) {
		return
	}
	if err := a.Repo.ConfirmMigration(r.Context(), p.UserID, "passkey", currentSessionHash(r), a.now()); err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "already_confirmed",
				"this migration has already been confirmed"))
			return
		}
		writeError(w, r, err)
		return
	}
	a.audit(r, "account.migrate.confirmed", "")
	writeJSON(w, http.StatusOK, map[string]any{"confirmed": true})
}

type migrateIssueCodeRequest struct {
	TargetUserID string `json:"target_user_id"`
}

// handleMigrateIssueCode binds the target and requires fresh proof from this session; only the code hash is stored.
func (a *API) handleMigrateIssueCode(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	var req migrateIssueCodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	targetID := strings.TrimSpace(req.TargetUserID)
	if targetID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "target_user_id is required"))
		return
	}
	if targetID == p.UserID {
		writeError(w, r, newError(http.StatusBadRequest, "invalid_target",
			"the target account must be different from the source"))
		return
	}
	m, err := a.Repo.MigrationForSource(r.Context(), p.UserID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "no_migration",
				"no migration is in progress; start one in-game with /felis migrate"))
			return
		}
		writeError(w, r, err)
		return
	}
	session, now := currentSessionHash(r), a.now()
	if migrationStage(m, session, now) != "confirmed" {
		writeError(w, r, errMigrateNotConfirmed)
		return
	}
	// Validate target liveness before issuing the code to avoid a database constraint error.
	target, err := a.Repo.UserDetail(r.Context(), targetID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusBadRequest, "target_not_found", "no account with that id"))
			return
		}
		writeError(w, r, err)
		return
	}
	if target.DeletedAt != nil || target.Disabled {
		writeError(w, r, newError(http.StatusBadRequest, "target_unavailable",
			"the target account is not available"))
		return
	}
	code, err := newLinkCode()
	if err != nil {
		writeError(w, r, err)
		return
	}
	expiresAt := now.Add(migrateCodeTTL)
	if err := a.Repo.IssueMigrationCode(r.Context(), p.UserID, targetID, session, otpCodeHash(code), now, expiresAt); err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, errMigrateNotConfirmed)
			return
		}
		writeError(w, r, err)
		return
	}
	a.audit(r, "account.migrate.code_issued", targetID)
	a.notifyMigrateCodeIssued(r, verifiedEmail(p), target.Username, expiresAt)
	writeJSON(w, http.StatusCreated, map[string]any{"code": code, "expires_at": expiresAt.UTC()})
}

var errMigrateNotConfirmed = newError(http.StatusConflict, "not_confirmed",
	"confirm the migration on this browser first; a confirmation lasts 10 minutes")

// errMigrateQuotaExceeded leaves the code unspent so redemption can retry after a quota increase.
var errMigrateQuotaExceeded = newError(http.StatusForbidden, "migrate_quota_exceeded",
	"the servers this migration moves do not fit this account's quota; ask an admin to raise it, then redeem the same code again before it expires")

type migrateRedeemRequest struct {
	Code string `json:"code"`
}

// handleMigrateRedeem requires the named target's session and atomically transfers servers and retires the source.
func (a *API) handleMigrateRedeem(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	var req migrateRedeemRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	code := strings.ToUpper(strings.TrimSpace(req.Code))
	if code == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "code is required"))
		return
	}
	sourceUserID, moved, err := a.Repo.RedeemMigration(r.Context(), p.UserID, otpCodeHash(code), a.now())
	if err != nil {
		if errors.Is(err, ErrLinkCodeInvalid) {
			writeError(w, r, newError(http.StatusBadRequest, "invalid_code", "migrate code is invalid or expired"))
			return
		}
		if errors.Is(err, ErrQuotaExceeded) {
			writeError(w, r, errMigrateQuotaExceeded)
			return
		}
		writeError(w, r, err)
		return
	}
	a.audit(r, "account.migrate.redeemed", sourceUserID)
	a.notifyMigrateRedeemed(r, sourceUserID, p, moved)
	writeJSON(w, http.StatusOK, map[string]any{
		"migrated":      true,
		"servers_moved": len(moved),
		"servers":       moved,
	})
}
