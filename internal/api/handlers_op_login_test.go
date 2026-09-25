package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// op.console STAFF login tests (spec §B op-login). The two-factor door's load-bearing
// properties, in the order the flow meets them:
//
//   - Two factors, both required. finish mints a session only when the mailed op_login
//     code verifies AND an in-game admin has approved the request; neither alone works.
//   - Neutral start. A non-staff or unknown address gets a 202 with a plausible but
//     non-persisted request_id and nothing mailed, so start is not a staff oracle.
//   - Neutral status. An unknown/expired/consumed handle reads approved:false exactly
//     like a real request awaiting approval, so a fabricated handle is not an oracle.
//   - Uniform finish failure. Unknown handle / not-approved / wrong code / lost race
//     all collapse to one op_login_invalid envelope; an early-but-correct code is
//     preserved (approval is read before the code is consumed), and a wrong code costs
//     an attempt without burning the approval.
//   - Admin-only approval. Only a linked role=admin UUID may vouch; the API's own
//     user table is the sole gate (velocity's command itself is unprivileged).

const opUUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" // the seeded admin's linked in-game UUID

// seedOpLoginAPI wires the op.console door: local sessions enabled and a single staff
// admin "op" (id a1) whose proven address is stored in MIXED case (so the mint-against-
// stored-casing contract is exercised by default) and whose in-game UUID opUUID is
// linked, so the admin can act as an in-game approver.
func seedOpLoginAPI(t *testing.T) (*API, *fakeRepo, *captureMailer) {
	t.Helper()
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["op"] = &StaffUser{
		ID: "a1", Username: "op", Email: "Op@Example.NET",
		Role: "admin", EmailVerified: true,
	}
	repo.links[opUUID] = "a1"
	mailer := &captureMailer{}
	api := newTestAPI(repo, newFakeCluster())
	api.Mailer = mailer
	return api, repo, mailer
}

// startOp / statusOp / finishOp drive the three public browser calls; approveOp drives
// the internal in-game vouch. They return the recorder so each test asserts its own
// codes and bodies.
func startOp(eh http.Handler, email string) *httptest.ResponseRecorder {
	return do(eh, "POST", "/api/v1/auth/op-login/start", `{"email":"`+email+`"}`, jsonHeader)
}
func statusOp(eh http.Handler, id string) *httptest.ResponseRecorder {
	return do(eh, "GET", "/api/v1/auth/op-login/status/"+id, "", nil)
}
func finishOp(eh http.Handler, id, code string) *httptest.ResponseRecorder {
	return do(eh, "POST", "/api/v1/auth/op-login/finish",
		`{"request_id":"`+id+`","code":"`+code+`"}`, jsonHeader)
}
func approveOp(ih http.Handler, id, approverUUID string) *httptest.ResponseRecorder {
	return do(ih, "POST", "/api/v1/internal/op-login/"+id+"/approve",
		`{"approver_uuid":"`+approverUUID+`"}`, nil)
}

// TestOpLoginVertical walks the whole two-factor slice end to end: start mails a code
// (purpose op_login) to the staff address of record and mints a pending request; the
// browser polls status until an in-game admin approves; finish redeems code+approval
// into the same host-only session the other doors mint. All three legs audit by the
// account's username, and the request is single-use.
func TestOpLoginVertical(t *testing.T) {
	api, repo, mailer := seedOpLoginAPI(t)
	eh := api.ExternalHandler()
	ih := api.InternalHandler()

	// 1) start: 202 with a request handle + expiry, never the code itself.
	w := startOp(eh, "op@example.net")
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: code = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	b := acctBody(t, w)
	reqID, _ := b["request_id"].(string)
	if reqID == "" {
		t.Fatal("start must return a request_id")
	}
	if _, leaked := b["code"]; leaked {
		t.Error("start response must NEVER carry the code")
	}
	if s, _ := b["expires_at"].(string); s == "" {
		t.Error("start must report expires_at")
	}
	// The code goes to the STORED casing (address of record), under the op_login purpose.
	if mailer.calls != 1 || mailer.email != "Op@Example.NET" {
		t.Fatalf("mailer: calls=%d email=%q, want 1 send to the STORED casing Op@Example.NET",
			mailer.calls, mailer.email)
	}
	code := mailer.code
	if len(repo.otps) != 1 {
		t.Fatalf("persisted codes = %d, want 1", len(repo.otps))
	}
	for _, o := range repo.otps {
		if o.purpose != otpPurposeOpLogin {
			t.Errorf("otp purpose = %q, want %q", o.purpose, otpPurposeOpLogin)
		}
	}
	// Exactly one pending request row, owned by the staff account.
	if len(repo.opLogins) != 1 {
		t.Fatalf("op_login_requests rows = %d, want 1", len(repo.opLogins))
	}
	if got := repo.opLogins[reqID]; got == nil || got.userID != "a1" || got.status != "pending" {
		t.Fatalf("request row = %+v, want {userID:a1, status:pending}", got)
	}

	// 2) status before approval: not approved yet.
	if sb := acctBody(t, statusOp(eh, reqID)); sb["approved"] != false {
		t.Fatalf("status before approval: approved = %v, want false", sb["approved"])
	}

	// 3) finish before approval is REFUSED and must NOT burn the code (approval is read
	// before the code is consumed).
	if w := finishOp(eh, reqID, code); w.Code != http.StatusBadRequest || decodeErr(t, w) != "op_login_invalid" {
		t.Fatalf("finish before approval: code = %d body %s, want 400 op_login_invalid", w.Code, w.Body.String())
	}
	if len(repo.sessions) != 0 {
		t.Fatal("no session may be minted before approval")
	}

	// 4) an in-game admin approves via the internal face.
	if w := approveOp(ih, reqID, opUUID); w.Code != http.StatusOK {
		t.Fatalf("approve: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if ab := acctBody(t, statusOp(eh, reqID)); ab["approved"] != true {
		t.Fatalf("status after approval: approved = %v, want true", ab["approved"])
	}

	// 5) finish with the preserved code: session minted, role=admin.
	w = finishOp(eh, reqID, code)
	if w.Code != http.StatusOK {
		t.Fatalf("finish: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	vb := acctBody(t, w)
	if vb["user_id"] != "a1" || vb["role"] != "admin" {
		t.Fatalf("finish body = %v, want user_id:a1 role:admin", vb)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName || cookies[0].Value == "" {
		t.Fatalf("want one non-empty %s cookie, got %v", sessionCookieName, cookies)
	}
	if s, ok := repo.sessions[hashCookie(cookies[0].Value)]; !ok || s.userID != "a1" {
		t.Fatalf("session row for the cookie = %+v (ok=%v), want userID a1", s, ok)
	}

	// Four audits by "op": otp_sent (start), the early finish refused before
	// approval, approved (in-game vouch), op_login (finish).
	if n := len(repo.audits); n != 4 {
		t.Fatalf("want 4 audits, got %d: %+v", n, repo.audits)
	}
	wantActions := []string{"auth.op_login.otp_sent", "auth.op_login.failed", "auth.op_login.approved", "auth.op_login"}
	for i, want := range wantActions {
		if repo.audits[i].Action != want || repo.audits[i].Actor != "op" || repo.audits[i].ActorUserID != "a1" {
			t.Errorf("audit[%d] = %+v, want action %q by op", i, repo.audits[i], want)
		}
	}

	// 6) single-use: the consumed request finishes no second time, and status flips back
	// to approved:false (consumed).
	if w := finishOp(eh, reqID, code); w.Code != http.StatusBadRequest || decodeErr(t, w) != "op_login_invalid" {
		t.Fatalf("replay finish: code = %d body %s, want 400 op_login_invalid", w.Code, w.Body.String())
	}
	if sb := acctBody(t, statusOp(eh, reqID)); sb["approved"] != false {
		t.Errorf("status after consume: approved = %v, want false", sb["approved"])
	}
}

// TestOpLoginOwnerAdmitted pins that the staff door admits the Owner (role=owner),
// not just plain admins: the Owner is the primary op.console identity, so a
// role check of "admin only" would strand it outside its own console.
func TestOpLoginOwnerAdmitted(t *testing.T) {
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["owner"] = &StaffUser{
		ID: "o1", Username: "owner", Email: "owner@example.net",
		Role: "owner", EmailVerified: true,
	}
	repo.links[opUUID] = "o1"
	mailer := &captureMailer{}
	api := newTestAPI(repo, newFakeCluster())
	api.Mailer = mailer
	eh := api.ExternalHandler()
	ih := api.InternalHandler()

	w := startOp(eh, "owner@example.net")
	if w.Code != http.StatusAccepted {
		t.Fatalf("owner start: code = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	reqID, _ := acctBody(t, w)["request_id"].(string)
	if reqID == "" || mailer.calls != 1 || len(repo.opLogins) != 1 {
		t.Fatalf("owner start must mint a request + mail a code: req=%q mails=%d rows=%d",
			reqID, mailer.calls, len(repo.opLogins))
	}
	if w := approveOp(ih, reqID, opUUID); w.Code != http.StatusOK {
		t.Fatalf("approve: code = %d (%s)", w.Code, w.Body.String())
	}
	w = finishOp(eh, reqID, mailer.code)
	if w.Code != http.StatusOK {
		t.Fatalf("owner finish: code = %d body %s, want 200", w.Code, w.Body.String())
	}
	if vb := acctBody(t, w); vb["role"] != "owner" {
		t.Fatalf("finish role = %v, want owner", vb["role"])
	}
}

// TestOpLoginStartNeutral pins the start-side anti-enumeration contract: op.console is
// the STAFF door, so a non-admin account AND an unknown address both get a 202 carrying
// a request_id + expires_at and mint/mail nothing, and a re-probe inside the cooldown
// does the same — so neither the response nor the throttle tells a caller who is staff.
func TestOpLoginStartNeutral(t *testing.T) {
	check := func(t *testing.T, seed func(*fakeRepo), email, wantReason, wantUser string) {
		t.Helper()
		repo := newFakeRepo()
		repo.settings[LocalAuthEnabledKey] = []byte("true")
		if seed != nil {
			seed(repo)
		}
		mailer := &captureMailer{}
		api := newTestAPI(repo, newFakeCluster())
		api.Mailer = mailer
		eh := api.ExternalHandler()

		w := startOp(eh, email)
		if w.Code != http.StatusAccepted {
			t.Fatalf("neutral start: code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		b := acctBody(t, w)
		if id, _ := b["request_id"].(string); id == "" {
			t.Error("neutral start must still return a plausible request_id")
		}
		if s, _ := b["expires_at"].(string); s == "" {
			t.Error("neutral start must still return expires_at")
		}
		if len(repo.opLogins) != 0 || len(repo.otps) != 0 || mailer.calls != 0 {
			t.Errorf("neutral start must mint/mail nothing: reqs=%d otps=%d mails=%d",
				len(repo.opLogins), len(repo.otps), mailer.calls)
		}
		// The response is neutral; the operator's record is not.
		if len(repo.audits) != 1 || repo.audits[0].Action != "auth.op_login.failed" ||
			!strings.Contains(string(repo.audits[0].Payload), `"reason":"`+wantReason+`"`) ||
			repo.audits[0].ActorUserID != wantUser {
			t.Errorf("neutral start audits = %+v, want one auth.op_login.failed %s by %q", repo.audits, wantReason, wantUser)
		}
		// Re-probing inside the window: the same 202 shape, still nothing behind it.
		w = startOp(eh, email)
		if b := acctBody(t, w); w.Code != http.StatusAccepted || b["request_id"] == "" || b["expires_at"] != "2023-11-14T22:23:20Z" {
			t.Fatalf("re-probe: code = %d body %s, want 202 with a request_id and the first expiry", w.Code, w.Body.String())
		}
		if len(repo.opLogins) != 0 || len(repo.otps) != 0 || mailer.calls != 0 {
			t.Errorf("re-probe must mint/mail nothing: reqs=%d otps=%d mails=%d",
				len(repo.opLogins), len(repo.otps), mailer.calls)
		}
	}

	t.Run("unknown address", func(t *testing.T) {
		check(t, nil, "ghost@example.net", "no_account", "")
	})
	t.Run("non-staff (role=user) address is ignored by the staff door", func(t *testing.T) {
		check(t, func(repo *fakeRepo) {
			repo.staff["p"] = &StaffUser{ID: "u9", Username: "p", Email: "player@example.net", Role: "user", EmailVerified: true}
		}, "player@example.net", "not_staff", "u9")
	})
}

// TestOpLoginStartByAStranger is the case of someone who knows a staff address and
// keeps starting logins for it. Their start mails the code to the staff inbox; the
// staff member's own start inside the cooldown still gets a request of its own
// (nothing new mailed, same expiry as the live code), and that inbox code finishes
// it. A start after the cooldown mails a second code without cancelling the first.
func TestOpLoginStartByAStranger(t *testing.T) {
	api, repo, mailer := seedOpLoginAPI(t)
	clock := time.Unix(1_700_000_000, 0)
	api.Now = func() time.Time { return clock }
	eh := api.ExternalHandler()
	ih := api.InternalHandler()
	requestID := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		if w.Code != http.StatusAccepted {
			t.Fatalf("start: code = %d, want 202 (%s)", w.Code, w.Body.String())
		}
		id, _ := acctBody(t, w)["request_id"].(string)
		return id
	}

	strangers := requestID(startOp(eh, "op@example.net"))
	inboxCode := mailer.code
	clock = clock.Add(30 * time.Second)
	w := startOp(eh, "op@example.net")
	own := requestID(w)
	if own == strangers || repo.opLogins[own] == nil {
		t.Fatalf("own request %q must be a new, stored request beside the stranger's %q", own, strangers)
	}
	if b := acctBody(t, w); b["expires_at"] != "2023-11-14T22:23:20Z" {
		t.Errorf("own start expires_at = %v, want 2023-11-14T22:23:20Z (the live code's)", b["expires_at"])
	}
	if mailer.calls != 1 || len(repo.otps) != 1 {
		t.Fatalf("start inside the cooldown: mails=%d otps=%d, want 1/1", mailer.calls, len(repo.otps))
	}
	if w := approveOp(ih, own, opUUID); w.Code != http.StatusOK {
		t.Fatalf("approve: code = %d (%s)", w.Code, w.Body.String())
	}
	if w := finishOp(eh, own, inboxCode); w.Code != http.StatusOK {
		t.Fatalf("finish own request with the inbox code: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	// After the cooldown a start mails a second code; the first one still works.
	clock = clock.Add(otpResendCooldown)
	first := requestID(startOp(eh, "op@example.net"))
	firstCode := mailer.code
	clock = clock.Add(otpResendCooldown)
	requestID(startOp(eh, "op@example.net"))
	if mailer.calls != 3 {
		t.Fatalf("mails = %d, want 3", mailer.calls)
	}
	if w := approveOp(ih, first, opUUID); w.Code != http.StatusOK {
		t.Fatalf("approve: code = %d (%s)", w.Code, w.Body.String())
	}
	if w := finishOp(eh, first, firstCode); w.Code != http.StatusOK {
		t.Fatalf("finish with the earlier code after a newer start: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// TestOpLoginStatusNeutral proves status is never an enumeration oracle: it returns
// approved:true ONLY for a genuinely approved, live, unconsumed request, and
// approved:false (never 404) for an unknown, expired, denied, or consumed handle — all
// indistinguishable from a real request still awaiting approval.
func TestOpLoginStatusNeutral(t *testing.T) {
	api, repo, _ := seedOpLoginAPI(t)
	eh := api.ExternalHandler()
	future := time.Unix(1_700_000_600, 0)
	past := time.Unix(1_699_999_999, 0)

	repo.opLogins["pending"] = &fakeOpLogin{id: "pending", userID: "a1", email: "op@example.net", status: "pending", expiresAt: future, createdAt: future}
	repo.opLogins["expired"] = &fakeOpLogin{id: "expired", userID: "a1", email: "op@example.net", status: "approved", expiresAt: past, createdAt: past}
	repo.opLogins["consumed"] = &fakeOpLogin{id: "consumed", userID: "a1", email: "op@example.net", status: "approved", consumed: true, expiresAt: future, createdAt: future}
	repo.opLogins["denied"] = &fakeOpLogin{id: "denied", userID: "a1", email: "op@example.net", status: "denied", expiresAt: future, createdAt: future}
	repo.opLogins["live"] = &fakeOpLogin{id: "live", userID: "a1", email: "op@example.net", status: "approved", expiresAt: future, createdAt: future}

	for _, id := range []string{"unknown-handle", "pending", "expired", "consumed", "denied"} {
		w := statusOp(eh, id)
		if w.Code != http.StatusOK {
			t.Fatalf("status %q: code = %d, want 200", id, w.Code)
		}
		if acctBody(t, w)["approved"] != false {
			t.Errorf("status %q: approved = true, want false (must not be an oracle)", id)
		}
	}
	// Only the genuinely-approved live request reads true.
	if acctBody(t, statusOp(eh, "live"))["approved"] != true {
		t.Error("status of an approved live request must read approved:true")
	}
}

// TestOpLoginFinishUniform is the redeem-side failure matrix. The anchor is uniformity:
// an unknown handle and a wrong code for an approved request answer with the SAME
// (code, message) envelope, so finish never doubles as an oracle. Two lifecycle
// invariants are pinned alongside: a correct code submitted BEFORE approval is
// preserved (approval read before consume), and a wrong code costs an attempt without
// burning the approval.
func TestOpLoginFinishUniform(t *testing.T) {
	t.Run("unknown handle and wrong code are indistinguishable", func(t *testing.T) {
		api, _, mailer := seedOpLoginAPI(t)
		eh, ih := api.ExternalHandler(), api.InternalHandler()
		reqID := acctBody(t, startOp(eh, "op@example.net"))["request_id"].(string)
		code := mailer.code
		if w := approveOp(ih, reqID, opUUID); w.Code != http.StatusOK {
			t.Fatalf("approve: %d (%s)", w.Code, w.Body.String())
		}
		// Wrong code for a real, approved request.
		wWrong := finishOp(eh, reqID, code+"x")
		// Unknown handle.
		wGhost := finishOp(eh, "deadbeefdeadbeefdeadbeefdeadbeef", code)
		if wWrong.Code != http.StatusBadRequest || wGhost.Code != http.StatusBadRequest {
			t.Fatalf("codes = %d/%d, want 400/400", wWrong.Code, wGhost.Code)
		}
		wc, wm := errEnvelope(t, wWrong)
		gc, gm := errEnvelope(t, wGhost)
		if wc != "op_login_invalid" || wc != gc || wm != gm {
			t.Errorf("envelopes differ: wrong=(%s,%q) unknown=(%s,%q) — must be identical", wc, wm, gc, gm)
		}
	})

	t.Run("correct code before approval is preserved, not burned", func(t *testing.T) {
		api, repo, mailer := seedOpLoginAPI(t)
		eh, ih := api.ExternalHandler(), api.InternalHandler()
		reqID := acctBody(t, startOp(eh, "op@example.net"))["request_id"].(string)
		code := mailer.code

		// Finish before approval: refused, and the code is NOT consumed.
		if w := finishOp(eh, reqID, code); w.Code != http.StatusBadRequest || decodeErr(t, w) != "op_login_invalid" {
			t.Fatalf("early finish: code = %d body %s, want 400 op_login_invalid", w.Code, w.Body.String())
		}
		for _, o := range repo.otps {
			if o.consumed || o.attempts != 0 {
				t.Errorf("early finish must not touch the code: consumed=%v attempts=%d", o.consumed, o.attempts)
			}
		}
		// Approve, then the same code completes.
		if w := approveOp(ih, reqID, opUUID); w.Code != http.StatusOK {
			t.Fatalf("approve: %d (%s)", w.Code, w.Body.String())
		}
		if w := finishOp(eh, reqID, code); w.Code != http.StatusOK {
			t.Fatalf("finish with preserved code: code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("wrong code charges an attempt without burning the approval", func(t *testing.T) {
		api, repo, mailer := seedOpLoginAPI(t)
		eh, ih := api.ExternalHandler(), api.InternalHandler()
		reqID := acctBody(t, startOp(eh, "op@example.net"))["request_id"].(string)
		code := mailer.code
		if w := approveOp(ih, reqID, opUUID); w.Code != http.StatusOK {
			t.Fatalf("approve: %d (%s)", w.Code, w.Body.String())
		}
		// Wrong code: refused, one attempt charged, request still approved+unconsumed.
		if w := finishOp(eh, reqID, code+"x"); w.Code != http.StatusBadRequest || decodeErr(t, w) != "op_login_invalid" {
			t.Fatalf("wrong code: code = %d body %s, want 400 op_login_invalid", w.Code, w.Body.String())
		}
		for _, o := range repo.otps {
			if o.consumed || o.attempts != 1 {
				t.Errorf("wrong code must charge one attempt, not consume: consumed=%v attempts=%d", o.consumed, o.attempts)
			}
		}
		if r := repo.opLogins[reqID]; r.status != "approved" || r.consumed {
			t.Errorf("a wrong code must not burn the approval: status=%q consumed=%v", r.status, r.consumed)
		}
		// The right code still completes.
		if w := finishOp(eh, reqID, code); w.Code != http.StatusOK {
			t.Fatalf("retry with right code: code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
	})
}

// TestOpLoginApproveGate pins the in-game approval gate: only a linked staff UUID
// (role admin or owner) may vouch (all refusals share one 403 not_admin), a
// missing/no-longer-pending request is 404, and a bare request without an
// approver UUID is 400.
func TestOpLoginApproveGate(t *testing.T) {
	plantPending := func(repo *fakeRepo) string {
		repo.opLogins["r1"] = &fakeOpLogin{
			id: "r1", userID: "a1", email: "op@example.net", status: "pending",
			expiresAt: time.Unix(1_700_000_600, 0), createdAt: time.Unix(1_700_000_000, 0),
		}
		return "r1"
	}

	t.Run("unlinked approver UUID -> 403, request stays pending", func(t *testing.T) {
		api, repo, _ := seedOpLoginAPI(t)
		id := plantPending(repo)
		if w := approveOp(api.InternalHandler(), id, "ffffffff-ffff-ffff-ffff-ffffffffffff"); w.Code != http.StatusForbidden || decodeErr(t, w) != "not_admin" {
			t.Fatalf("unlinked approver: code = %d body %s, want 403 not_admin", w.Code, w.Body.String())
		}
		if repo.opLogins[id].status != "pending" {
			t.Error("a refused approval must leave the request pending")
		}
	})

	t.Run("linked non-admin approver -> 403", func(t *testing.T) {
		api, repo, _ := seedOpLoginAPI(t)
		id := plantPending(repo)
		repo.staff["p"] = &StaffUser{ID: "u9", Username: "p", Email: "player@example.net", Role: "user", EmailVerified: true}
		repo.links["bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"] = "u9"
		if w := approveOp(api.InternalHandler(), id, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"); w.Code != http.StatusForbidden || decodeErr(t, w) != "not_admin" {
			t.Fatalf("non-admin approver: code = %d body %s, want 403 not_admin", w.Code, w.Body.String())
		}
	})

	t.Run("a linked owner-role approver vouches too", func(t *testing.T) {
		// The owner role is a superset of admin (auth.go staffRole), so a manually
		// promoted Owner (migration 0011) must pass the in-game approve gate.
		api, repo, _ := seedOpLoginAPI(t)
		repo.staff["boss"] = &StaffUser{ID: "b1", Username: "boss", Role: "owner"}
		repo.links["cccccccc-cccc-cccc-cccc-cccccccccccc"] = "b1"
		id := plantPending(repo)
		if w := approveOp(api.InternalHandler(), id, "cccccccc-cccc-cccc-cccc-cccccccccccc"); w.Code != http.StatusOK {
			t.Fatalf("owner-role approver: code = %d body %s, want 200", w.Code, w.Body.String())
		}
	})

	t.Run("missing approver_uuid -> 400", func(t *testing.T) {
		api, repo, _ := seedOpLoginAPI(t)
		id := plantPending(repo)
		if w := do(api.InternalHandler(), "POST", "/api/v1/internal/op-login/"+id+"/approve", `{}`, nil); w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
			t.Fatalf("missing approver_uuid: code = %d body %s, want 400 bad_request", w.Code, w.Body.String())
		}
	})

	t.Run("unknown request id -> 404", func(t *testing.T) {
		api, _, _ := seedOpLoginAPI(t)
		if w := approveOp(api.InternalHandler(), "nosuchrequest", opUUID); w.Code != http.StatusNotFound || decodeErr(t, w) != "op_login_not_found" {
			t.Fatalf("unknown request: code = %d body %s, want 404 op_login_not_found", w.Code, w.Body.String())
		}
	})

	t.Run("re-approving an approved request -> 404 (first approval stands)", func(t *testing.T) {
		api, repo, _ := seedOpLoginAPI(t)
		id := plantPending(repo)
		if w := approveOp(api.InternalHandler(), id, opUUID); w.Code != http.StatusOK {
			t.Fatalf("first approve: code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		if w := approveOp(api.InternalHandler(), id, opUUID); w.Code != http.StatusNotFound {
			t.Fatalf("second approve: code = %d, want 404 (no longer pending)", w.Code)
		}
		if repo.opLogins[id].status != "approved" {
			t.Error("the request must remain approved after a redundant re-approval")
		}
	})
}

// TestOpLoginPendingList covers the internal push list: live pending requests are
// returned oldest first, carrying the joined username, and an approved or expired
// request is absent.
func TestOpLoginPendingList(t *testing.T) {
	api, repo, _ := seedOpLoginAPI(t)
	ih := api.InternalHandler()
	future := time.Unix(1_700_000_600, 0)

	// Two pending (distinct createdAt so ordering is deterministic), one approved, one
	// expired.
	repo.opLogins["r2"] = &fakeOpLogin{id: "r2", userID: "a1", email: "op@example.net", status: "pending", expiresAt: future, createdAt: time.Unix(1_700_000_200, 0)}
	repo.opLogins["r1"] = &fakeOpLogin{id: "r1", userID: "a1", email: "op@example.net", status: "pending", expiresAt: future, createdAt: time.Unix(1_700_000_100, 0)}
	repo.opLogins["ap"] = &fakeOpLogin{id: "ap", userID: "a1", email: "op@example.net", status: "approved", expiresAt: future, createdAt: time.Unix(1_700_000_150, 0)}
	repo.opLogins["ex"] = &fakeOpLogin{id: "ex", userID: "a1", email: "op@example.net", status: "pending", expiresAt: time.Unix(1_699_999_999, 0), createdAt: time.Unix(1_700_000_050, 0)}

	w := do(ih, "GET", "/api/v1/internal/op-login/pending", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("pending: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	body := acctBody(t, w)
	pending, _ := body["pending"].([]any)
	if len(pending) != 2 {
		t.Fatalf("pending count = %d, want 2 (only live pending rows) — %v", len(pending), body["pending"])
	}
	// Oldest first: r1 (created earlier) before r2.
	first, _ := pending[0].(map[string]any)
	second, _ := pending[1].(map[string]any)
	if first["request_id"] != "r1" || second["request_id"] != "r2" {
		t.Errorf("order = [%v, %v], want [r1, r2] (oldest first)", first["request_id"], second["request_id"])
	}
	if first["username"] != "op" || first["email"] != "op@example.net" {
		t.Errorf("row projection = %v, want username op / email op@example.net", first)
	}
}

// TestOpLoginGates covers the shared front doors: the fail-closed local-auth toggle on
// all three public legs, the CSRF Content-Type guard on the credential-minting POSTs,
// and the input gates that must reject before any lookup or mint.
func TestOpLoginGates(t *testing.T) {
	t.Run("local auth disabled -> 403 on start/status/finish", func(t *testing.T) {
		api := newTestAPI(newFakeRepo(), newFakeCluster()) // no LocalAuthEnabledKey: fails closed
		eh := api.ExternalHandler()
		if w := startOp(eh, "op@example.net"); w.Code != http.StatusForbidden || decodeErr(t, w) != "local_auth_disabled" {
			t.Errorf("start: code = %d body %s, want 403 local_auth_disabled", w.Code, w.Body.String())
		}
		if w := statusOp(eh, "anything"); w.Code != http.StatusForbidden || decodeErr(t, w) != "local_auth_disabled" {
			t.Errorf("status: code = %d body %s, want 403 local_auth_disabled", w.Code, w.Body.String())
		}
		if w := finishOp(eh, "anything", "123456"); w.Code != http.StatusForbidden || decodeErr(t, w) != "local_auth_disabled" {
			t.Errorf("finish: code = %d body %s, want 403 local_auth_disabled", w.Code, w.Body.String())
		}
	})

	t.Run("non-JSON content type -> 415 on the minting POSTs", func(t *testing.T) {
		api, _, _ := seedOpLoginAPI(t)
		eh := api.ExternalHandler()
		for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
			if w := do(eh, "POST", "/api/v1/auth/op-login/start", `{"email":"op@example.net"}`, ctHeader(ct)); w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("start Content-Type %q: code = %d, want 415", ct, w.Code)
			}
			if w := do(eh, "POST", "/api/v1/auth/op-login/finish", `{"request_id":"x","code":"1"}`, ctHeader(ct)); w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("finish Content-Type %q: code = %d, want 415", ct, w.Code)
			}
		}
	})

	t.Run("start bad email -> 400, nothing minted", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"email":""}`, `{"email":"notanemail"}`, `{"email":"op@example.net","x":1}`} {
			api, repo, mailer := seedOpLoginAPI(t)
			w := do(api.ExternalHandler(), "POST", "/api/v1/auth/op-login/start", body, jsonHeader)
			if w.Code != http.StatusBadRequest {
				t.Errorf("start %q: code = %d, want 400 (%s)", body, w.Code, w.Body.String())
			}
			if len(repo.opLogins) != 0 || len(repo.otps) != 0 || mailer.calls != 0 {
				t.Errorf("start %q: a rejected start must mint nothing", body)
			}
		}
	})

	t.Run("finish missing request_id or code -> 400 bad_request", func(t *testing.T) {
		api, _, _ := seedOpLoginAPI(t)
		eh := api.ExternalHandler()
		for _, body := range []string{`{"code":"123456"}`, `{"request_id":"x"}`, `{"request_id":"","code":""}`} {
			if w := do(eh, "POST", "/api/v1/auth/op-login/finish", body, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
				t.Errorf("finish %q: code = %d body %s, want 400 bad_request", body, w.Code, w.Body.String())
			}
		}
	})
}

// TestOpLoginFaceSeparation enforces the two-face split: the three public browser legs
// must 404 on the internal (service-token) face, and the two internal in-game legs must
// 404 on the external (session) face.
func TestOpLoginFaceSeparation(t *testing.T) {
	api, _, _ := seedOpLoginAPI(t)
	eh, ih := api.ExternalHandler(), api.InternalHandler()

	// Public legs must not appear on the internal face.
	if w := do(ih, "POST", "/api/v1/auth/op-login/start", `{"email":"op@example.net"}`, jsonHeader); w.Code != http.StatusNotFound {
		t.Errorf("start on internal face: code = %d, want 404", w.Code)
	}
	if w := do(ih, "GET", "/api/v1/auth/op-login/status/x", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("status on internal face: code = %d, want 404", w.Code)
	}
	if w := do(ih, "POST", "/api/v1/auth/op-login/finish", `{"request_id":"x","code":"1"}`, jsonHeader); w.Code != http.StatusNotFound {
		t.Errorf("finish on internal face: code = %d, want 404", w.Code)
	}
	// Internal legs must not appear on the external face.
	if w := do(eh, "GET", "/api/v1/internal/op-login/pending", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("pending on external face: code = %d, want 404", w.Code)
	}
	if w := do(eh, "POST", "/api/v1/internal/op-login/x/approve", `{"approver_uuid":"`+opUUID+`"}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("approve on external face: code = %d, want 404", w.Code)
	}
}
