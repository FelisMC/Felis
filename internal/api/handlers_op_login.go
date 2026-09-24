package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// op.console STAFF login (spec §B op-login): the two-factor door for the most
// sensitive tier. Unlike the console.<root_domain> player doors (email OTP / bind
// code), a staff web session is never minted from a single factor. The flow is a
// three-call state machine over op_login_requests (migration 0016), all Public
// pre-session routes (the caller has no principal yet), plus two internal-face routes
// for the in-game side (approve is driven by velocity's /felis command; pending has
// no consumer yet — see handleOpLoginPending):
//
//	POST /api/v1/auth/op-login/start            (public)   — mint a request + mail an OTP
//	GET  /api/v1/auth/op-login/status/{id}      (public)   — poll until an admin approves
//	POST /api/v1/auth/op-login/finish           (public)   — redeem code+approval → session
//	GET  /api/v1/internal/op-login/pending      (internal) — list requests awaiting a vouch
//	POST /api/v1/internal/op-login/{id}/approve (internal) — an in-game admin vouches
//
// The two factors:
//
//   - Possession of the staff mailbox — an email-OTP under purpose op_login, minted by
//     start and redeemed by finish, reusing the email_otps lifecycle (the purpose
//     column keeps it from ever colliding with a console login_email or onboard code).
//   - An in-game vouch — an already-trusted admin who is ONLINE approves the pending
//     request via velocity's /felis command (internal approve). The API's own user
//     table is the sole authority: only a UUID linked to a staff account may
//     approve (velocity's command runs for any player and relies on this check).
//
// finish mints the session only when BOTH have landed. Neither factor alone — a mailed
// code without an approval, or an approval without the code — yields a session.
//
// Anti-enumeration. op.console sits behind Cloudflare Zero-Trust at the edge, but the
// external API is hostname-agnostic at the route level, so these Public routes are
// reachable from console.<root_domain> too and must not become a staff oracle:
//
//   - start resolves the typed email; a non-staff or unknown address gets the SAME 202
//     with a plausible (non-persisted, random) request_id and mails nothing, so a
//     caller cannot tell a staff address from any other.
//   - status returns approved:false for an unknown/expired/denied/consumed id exactly
//     as for a live-but-unapproved one; only a genuinely approved live request reads
//     approved:true, and driving an id to that state REQUIRES an in-game admin vouch a
//     fabricated id can never obtain.
//   - finish collapses unknown id, not-yet-approved, wrong code, locked, and lost-race
//     into one uniform failure, and (like the console door) never reveals staffness.

// otpPurposeOpLogin scopes an email code to the op.console staff door, keeping it from
// ever colliding with or satisfying a console login_email or onboarding code for the
// same account. VerifyEmailOTP/ConsumeLoginEmailOTP are queried per (user, purpose),
// so the op-login factor is fully independent of the player-console doors.
const otpPurposeOpLogin = "op_login"

// opLoginStartRequest is the start body: the staff address the code is mailed to.
type opLoginStartRequest struct {
	Email string `json:"email"`
}

// handleOpLoginStart begins a staff op.console login (Public, pre-session): it mints an
// op_login_requests row for the resolved staff account and mails an email-OTP under
// otpPurposeOpLogin, returning the request handle the browser polls. A non-staff or
// unknown address yields the SAME 202 with a random, non-persisted handle and no mail,
// so this never doubles as a staff-enumeration oracle (op.console's own Zero-Trust is
// the edge gate; this app-layer neutrality covers the hostname-agnostic route).
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

	// The install-wide mail budget is checked before the address is resolved,
	// so while it is spent every address gets the same 429.
	if err := a.checkMailBudget(); err != nil {
		writeError(w, r, err)
		return
	}

	// Per-recipient cooldown reserved BEFORE any work, identical to the console email
	// door: one winner per window, and the neutral (non-staff) branch keeps the
	// reservation too so probing an address is throttled exactly like a real send. The
	// key is namespaced apart from the console door's "login:email:" so the two
	// unauthenticated doors never perturb each other's throttle.
	emailKey := "oplogin:email:" + strings.ToLower(email)
	lim := a.otpLimiter()
	emailAt, ok := lim.reserve(emailKey, otpResendCooldown)
	if !ok {
		writeError(w, r, newError(http.StatusTooManyRequests, "otp_resend_cooldown",
			"a code was sent recently; wait a moment before requesting another"))
		return
	}
	committed := false
	defer func() {
		if !committed {
			lim.release(emailKey, emailAt)
		}
	}()

	// Compute expiry once so the neutral and real branches return identical-shaped
	// bodies and (real branch) the request row and its OTP are coterminous.
	expiresAt := a.now().Add(otpTTL)

	// neutral returns the indistinguishable no-op success: a plausible but non-persisted
	// handle that status(id) reads approved:false forever (no row, never approvable). It
	// mints nothing and mails nothing, and KEEPS the reservation so probing is throttled
	// exactly like a real send.
	neutral := func() {
		fakeID, err := newOTPID()
		if err != nil {
			writeError(w, r, err) // committed stays false → deferred rollback frees the window
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
		neutral()
		return
	case err != nil:
		// Real read fault: leave committed false so the deferred rollback frees the
		// window (a transient DB blip must not burn it).
		writeError(w, r, err)
		return
	}
	// op.console is the STAFF door: a player who typed their address here (they belong
	// on console.<root_domain>) gets the neutral response, never a request or a code.
	// Staff means admin OR owner — the Owner is the primary op.console user.
	if !staffRole(u.Role) {
		neutral()
		return
	}
	// A locked door (wrong-code budget spent) is neutral too: no request, no mail.
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
	if err := a.Repo.CreateOpLoginRequest(r.Context(), id, u.ID, u.Email, expiresAt); err != nil {
		writeError(w, r, err)
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
	// Mint+mail against the STORED staff address (UserByEmail matched case-insensitively);
	// the request row snapshots the same address for its audit trail.
	if err := a.Repo.CreateEmailOTP(r.Context(), otpID, u.ID, u.Email, otpCodeHash(code), otpPurposeOpLogin, expiresAt); err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.deliverOTP(r.Context(), u.Email, code); err != nil {
		writeError(w, r, err)
		return
	}
	committed = true
	a.audit(r, u.Username, "auth.op_login.otp_sent", "")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"request_id": id, "expires_at": expiresAt.UTC(),
	})
}

// handleOpLoginStatus reports whether a staff login request has been approved in-game
// (Public, pre-session). It is a pure read the browser polls after start: it returns
// approved:true only for a genuinely approved, live, unconsumed request, and
// approved:false for everything else — including an unknown, expired, denied, or
// already-consumed id — so a fabricated handle polls as approved:false forever and the
// endpoint is not a staff-enumeration oracle (only an in-game admin vouch, impossible
// against a fake id, flips it true).
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
		// Unknown handle: neutral approved:false (never 404), uniform with a real request
		// still awaiting approval.
	case err != nil:
		writeError(w, r, err)
		return
	default:
		approved = req.Status == "approved" && !req.Consumed && req.ExpiresAt.After(a.now())
	}
	writeJSON(w, http.StatusOK, map[string]any{"approved": approved})
}

// opLoginFinishRequest is the finish body: the request handle from start and the code
// read from the staff mailbox. The handle selects the account (there is no principal);
// the code proves possession of the mailbox this session.
type opLoginFinishRequest struct {
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
}

// handleOpLoginFinish redeems an approved request plus its mailed code into a staff
// session (Public, pre-session). It mints the session only when BOTH factors have
// landed: the request is approved-and-live AND the code verifies. Every failure mode —
// unknown handle, not-yet-approved, wrong or locked code, lost race — collapses into
// ONE uniform 400, so a code-less caller learns nothing (not staffness, not approval
// state). The approval is read BEFORE the code is consumed so a valid code submitted
// early (before an admin approves) is preserved for a retry rather than burned.
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

	// The single uniform failure every "cannot complete" branch returns, so unknown
	// handle / not-approved / wrong code / locked / lost-race are indistinguishable.
	invalid := newError(http.StatusBadRequest, "op_login_invalid",
		"this operator login could not be completed; restart the sign-in")

	now := a.now()
	loginReq, err := a.Repo.OpLoginRequestByID(r.Context(), requestID)
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, r, invalid)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	// Read the approval state BEFORE touching the code: an early finish (user typed the
	// code before an admin approved) must not consume the code. Not-approved collapses
	// into the same uniform failure as a bad code, so the ordering leaks nothing.
	if loginReq.Status != "approved" || loginReq.Consumed || !loginReq.ExpiresAt.After(now) {
		writeError(w, r, invalid)
		return
	}
	// Consume the mailed code (op_login purpose). A wrong/expired/locked code charges an
	// attempt without minting anything and returns the uniform failure — the code, not
	// the request, is the problem, and the request stays approved for a retry.
	switch err := a.Repo.ConsumeLoginEmailOTP(r.Context(), loginReq.UserID, otpPurposeOpLogin, otpCodeHash(code), now); {
	case errors.Is(err, ErrOTPInvalid), errors.Is(err, ErrOTPLocked), errors.Is(err, ErrOTPAccountLocked):
		a.noteOTPLock(r, err, loginReq.UserID, otpPurposeOpLogin)
		writeError(w, r, invalid)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	// Both factors proven. Atomically spend the request (approved→consumed, single-use):
	// this serialises against a concurrent finish and records which request completed.
	switch err := a.Repo.ConsumeOpLoginRequest(r.Context(), requestID, now); {
	case errors.Is(err, ErrNotFound):
		// Lost a race (another finish consumed it) or it expired between the checks —
		// uniform failure. The code was already spent by the winner.
		writeError(w, r, invalid)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	// Load the staff account for the session + response. Re-assert staff as defence in
	// depth: only staff ever get a request minted, but the session must never be issued
	// to a non-staff identity even if the row were somehow otherwise.
	u, err := a.Repo.UserByID(r.Context(), loginReq.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !staffRole(u.Role) {
		writeError(w, r, newError(http.StatusForbidden, "staff_account", "that account is not an operator"))
		return
	}
	token, err := newSessionToken()
	if err != nil {
		writeError(w, r, err)
		return
	}
	expires := now.Add(sessionTTL)
	if err := a.Repo.CreateSession(r.Context(), hashCookie(token), u.ID, expires); err != nil {
		writeError(w, r, err)
		return
	}
	setSessionCookie(w, token, expires)
	a.audit(r, u.Username, "auth.op_login", "")
	writeJSON(w, http.StatusOK, map[string]any{"user_id": u.ID, "role": u.Role})
}

// handleOpLoginPending lists live pending staff login requests, oldest first (internal
// face). Today no plugin consumes it: the approver learns the request id out-of-band
// (the op.console start screen shows it to the person logging in) and runs
// /felis web op approve <id>. The route exists so velocity can later push the waiting
// list to online admins without an API change. Internal-only: velocity holds a service
// token and no pending request is secret to the operator crew.
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
			"created_at": req.CreatedAt.UTC(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": out})
}

// opLoginApproveRequest is the internal approve body: the online-mode UUID of the
// in-game admin running /felis web op approve. The API resolves it to a linked account
// and refuses unless that account is staff (admin or owner) — this check against the API's
// authoritative user table is the only gate; velocity's command itself is unprivileged.
type opLoginApproveRequest struct {
	ApproverUUID string `json:"approver_uuid"`
}

// handleOpLoginApprove records an in-game admin's vouch for a pending staff login
// (internal face), supplying the second factor. It resolves the approver UUID to a
// linked staff account (admin or owner; else 403), then flips the request approved.
// A missing or no-longer-pending request is 404. Self-approval is allowed: a staff
// member online as their own admin identity supplies a genuine second factor
// (in-game session control) distinct from the mailbox factor.
func (a *API) handleOpLoginApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req opLoginApproveRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	approverUUID := strings.TrimSpace(req.ApproverUUID)
	if approverUUID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "approver_uuid is required"))
		return
	}
	// Resolve the in-game approver to a linked account and require a staff role
	// (admin, or the owner superset). An unlinked UUID or a non-staff player may
	// never vouch for an op.console login. All three refusals share one response so
	// a caller cannot tell "not linked" from "linked but not staff".
	notAdmin := newError(http.StatusForbidden, "not_admin", "only a linked administrator may approve an operator login")
	approverID, err := a.Repo.UserByMCUUID(r.Context(), approverUUID)
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, r, notAdmin)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	approver, err := a.Repo.UserByID(r.Context(), approverID)
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, r, notAdmin)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	if !staffRole(approver.Role) {
		writeError(w, r, notAdmin)
		return
	}
	switch err := a.Repo.ApproveOpLogin(r.Context(), id, approverID, a.now()); {
	case errors.Is(err, ErrNotFound):
		writeError(w, r, newError(http.StatusNotFound, "op_login_not_found", "no pending operator login with that id"))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	payload, _ := json.Marshal(map[string]string{"request_id": id, "approver_user_id": approverID})
	_ = a.Repo.Audit(r.Context(), AuditEntry{
		Actor: approver.Username, Source: "internal", Action: "auth.op_login.approved",
		RequestID: requestIDFromContext(r.Context()), Payload: payload,
	})
	writeJSON(w, http.StatusOK, map[string]any{"approved": true})
}
