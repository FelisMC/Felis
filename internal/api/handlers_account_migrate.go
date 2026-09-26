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

// Account migration (spec §B3 inherit, scenario A). A LIVE old account hands its
// owned servers to a new account and is retired. The flow, and which side of the
// house each step lives on:
//
//	1. in-game  /felis migrate            → handleMigrateStart (internal face): the
//	                                         source, known by its verified mc_uuid, enters
//	                                         migrate mode (state 'initiated').
//	2. web      step-up confirm           → handleMigrateConfirm{OTP,Passkey}*: the
//	                                         source proves control with a FRESH factor —
//	                                         passkey if any is enrolled (forced), else an
//	                                         email-OTP — advancing to 'confirmed'. Mere
//	                                         session possession is never enough; a stolen
//	                                         session cannot read the mailbox nor present the
//	                                         authenticator, and cannot enroll one of its own
//	                                         without a recent proof of an existing factor
//	                                         (reauth.go).
//	3. web      issue code + name target  → handleMigrateIssueCode: the source names the
//	                                         target account by id and mints a one-time code
//	                                         ('code_issued'). Only the session that gave the
//	                                         step-up may, within migrateConfirmWindow of it,
//	                                         and the source's mailbox is told.
//	4. web      target redeems code       → handleMigrateRedeem: the target, logged in as
//	                                         itself, submits the code; ownership of the
//	                                         source's servers moves to the target and the
//	                                         source is retired ('redeemed'), and told so.
//
// The code is bound to the named target at issue AND the redeemer must authenticate AS
// that target, so an intercepted code is useless to anyone else. Binding the step-up to
// its session and a short window keeps a confirmation from outliving the moment: any
// other live session of the source (a shared machine, a stolen cookie) meets the
// step-up again instead of a door left open. Only server ownership moves — the mc_uuid
// link and web credentials (email, passkeys) stay with their accounts; moving
// credentials would make migrate a credential-theft primitive.
//
// CODE-ONLY (Java/Velocity, not represented here): the /felis migrate command that calls
// handleMigrateStart, and the web forms that drive steps 2–4.

const (
	// otpPurposeMigrate scopes an email-OTP to the migration step-up, so a
	// migrate-confirm code never collides with an onboarding or login code for the
	// same user (see otpPurposeOnboard).
	otpPurposeMigrate = "migrate_confirm"
	// passkeyPurposeMigrate scopes a passkey assertion challenge to the migration
	// step-up, keeping it apart from the login assertion challenge (passkeyPurposeLogin).
	passkeyPurposeMigrate = "passkey_migrate"
	// migrateCodeTTL bounds the one-time code the source hands to the target. Short
	// enough that a leaked code is useless soon, long enough to switch accounts and type.
	migrateCodeTTL = 10 * time.Minute
	// migrateConfirmWindow is how long the step-up lets the session that gave it issue
	// the code. Enough to paste the target's account id.
	migrateConfirmWindow = 10 * time.Minute
)

// migrationStage is where the caller on session stands in m at now: the stored state,
// except that a confirmation this session cannot use (another session's, or older
// than migrateConfirmWindow) and a code that expired unspent put the caller back at
// the step-up, "initiated". ConfirmMigration and IssueMigrationCode apply the same
// rules in SQL.
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

// newMigrationID returns an opaque random row id (128 bits, hex) for an
// account_migrations row, mirroring the other one-time-handle mints.
func newMigrationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// migrateStartRequest is the in-game /felis migrate callback body (internal face): the
// verified UUID of the player who ran the command. Its linked account becomes the
// migration source.
type migrateStartRequest struct {
	MCUUID string `json:"mc_uuid"`
}

// handleMigrateStart puts the account linked to a verified in-game UUID into migrate
// mode (spec §B3, internal face). It is the server side of /felis migrate: velocity has
// already established the UUID via online-mode auth, so the initiator is trustworthy;
// the sensitive proof (step-up) still happens on the web before anything transfers. An
// unlinked UUID has no account to migrate (404); a retired/already-migrated account
// cannot re-initiate (409).
func (a *API) handleMigrateStart(w http.ResponseWriter, r *http.Request) {
	var req migrateStartRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	mcUUID := strings.TrimSpace(req.MCUUID)
	if mcUUID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "mc_uuid is required"))
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
	// Internal-face event: attribute to the in-game initiator, Source 'internal'.
	a.auditEntry(r, AuditEntry{
		Actor:  "mc:" + mcUUID,
		Source: internalSource(r),
		Action: "account.migrate.start",
	})
	writeJSON(w, http.StatusCreated, map[string]any{"started": true, "state": "initiated"})
}

// handleMigrateStatus reports the caller's live migration for the web flow to drive its
// next step (spec §B3, external app face). No migration in flight → {active:false}. The
// state is migrationStage's, so a confirmation made elsewhere or lapsed reads as
// "initiated" and the panel asks for the step-up again.
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

// requireInitiatedMigration loads the caller's live migration and requires migrationStage
// to put the caller at 'initiated' — the only stage from which step-up may run. It writes
// the right error and returns ok=false when the caller should stop, so the confirm
// handlers stay flat.
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

// userHasPasskey reports whether the account has any passkey enrolled — the predicate
// that forces the passkey factor for the step-up.
func (a *API) userHasPasskey(ctx context.Context, userID string) (bool, error) {
	creds, err := a.Repo.PasskeyCredentialsForUser(ctx, userID)
	if err != nil {
		return false, err
	}
	return len(creds) > 0, nil
}

// handleMigrateConfirmOTPStart mints and delivers a fresh email-OTP for the migration
// step-up (spec §B3, external app face). It is refused when the account has a passkey
// enrolled — a strong factor must not be downgradable to email for an identity transfer.
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
	// Per-recipient cooldown, namespaced apart from the other OTP doors so they never
	// perturb each other's throttle.
	a.startStepUpOTP(w, r, p, otpPurposeMigrate, "migrate:confirm:", "account.migrate.confirm_otp_sent")
}

// handleMigrateConfirmOTPVerify redeems the migration step-up code and, on a match,
// advances the migration to 'confirmed' (spec §B3, external app face). The code lifecycle
// is the login-door one (no identity side-effect): the address is already proven.
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

// handleMigrateConfirmPasskeyBegin starts a fresh passkey assertion bound to the
// migration step-up (spec §B3, external app face). Unlike the login door it needs no
// email — the caller is already authenticated — so it scopes the challenge to the
// session principal and purpose passkeyPurposeMigrate.
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

// handleMigrateConfirmPasskeyFinish verifies the migration step-up assertion and, on
// success, advances the migration to 'confirmed' (spec §B3, external app face). It
// consumes the stashed migrate challenge atomically (a missing/expired one → 400).
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

// migrateIssueCodeRequest is the issue-code body: the id of the new account the source
// nominates to receive its servers.
type migrateIssueCodeRequest struct {
	TargetUserID string `json:"target_user_id"`
}

// handleMigrateIssueCode binds the named target and mints the one-time migrate code
// (spec §B3, external app face). Requires the step-up done on this session within
// migrateConfirmWindow. The target must be a live account other than the source. The
// code is returned once, out of band to the target; only its hash is stored. The
// source's verified address is told, so a code its owner did not issue does not go
// unnoticed.
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
	// The target must exist and be a live (non-deleted, non-disabled) account. Validate
	// here so a typo'd id fails with a clear message rather than a bare FK error.
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

// errMigrateNotConfirmed refuses a code to a caller without a usable step-up.
var errMigrateNotConfirmed = newError(http.StatusConflict, "not_confirmed",
	"confirm the migration on this browser first; a confirmation lasts 10 minutes")

// migrateRedeemRequest is the redeem body: the one-time code the target received.
type migrateRedeemRequest struct {
	Code string `json:"code"`
}

// handleMigrateRedeem spends the migrate code as the named target (spec §B3, external
// app face). The redeemer must be authenticated as the account the code was bound to;
// a code whose target is a different account simply does not match (an intercepted code
// is useless). On success the source's servers are re-pointed to the caller and the
// source account is retired, atomically.
func (a *API) handleMigrateRedeem(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	var req migrateRedeemRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	// Trim + uppercase so a target who typed the code with stray spaces or in lowercase
	// still matches the minted value (the alphabet is uppercase); then hash — the raw
	// code is never compared against the database.
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
