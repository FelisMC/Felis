package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Reauth: once an account has a passkey or a verified email, adding or removing a
// passkey and changing the email need a factor proven within reauthWindow. These
// tests drive the real SessionAuth, so the proof is read from the session row the
// cookie names, exactly as in production.

const opTok = "tok-op"

// reauthFixture is the sessions fixture (steve: a player with a verified email,
// signed in on the laptop and the phone; alex: a player with no factor) plus a
// passkey verifier and an operator, pam, with a verified email. Every session
// starts with no proof on it.
func reauthFixture(t *testing.T) *sessionsFixture {
	t.Helper()
	f := newSessionsFixture(t)
	f.api.Passkey = &fakePasskeyVerifier{}
	f.repo.staff["pam"] = &StaffUser{ID: "u3", Username: "pam", Email: "pam@example.net", Role: "admin", EmailVerified: true}
	now := f.api.now()
	f.repo.sessions[hashCookie(opTok)] = &fakeSession{userID: "u3", lastSeen: now, expiresAt: now.Add(time.Hour)}
	for _, s := range f.repo.sessions {
		s.reauthAt = time.Time{}
	}
	f.eh = f.api.ExternalHandler()
	return f
}

func (f *sessionsFixture) reauthAt(tok string) time.Time {
	return f.repo.sessions[hashCookie(tok)].reauthAt
}

func (f *sessionsFixture) setReauth(tok string, at time.Time) {
	f.repo.sessions[hashCookie(tok)].reauthAt = at
}

func jsonCookie(tok string) map[string]string {
	h := asCookie(tok)
	h["Content-Type"] = "application/json"
	return h
}

func TestSigningInByEmailCodeCountsAsReauth(t *testing.T) {
	api, repo, mailer := seedLoginEmailAPI(t)
	eh := api.ExternalHandler()
	if w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"player@example.net"}`, jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("start = %d (%s)", w.Code, w.Body.String())
	}
	w := do(eh, "POST", "/api/v1/auth/email/verify", `{"email":"player@example.net","code":"`+mailer.code+`"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("verify = %d (%s)", w.Code, w.Body.String())
	}
	s := repo.sessions[hashCookie(sessionCookieValue(t, w))]
	if !s.reauthAt.Equal(api.now()) {
		t.Fatalf("reauth_at = %v, want the sign-in time %v", s.reauthAt, api.now())
	}
}

// A bind code proves the Minecraft account, and nothing about the account's own
// passkey or mailbox.
func TestSigningInByBindCodeIsNoReauth(t *testing.T) {
	api, repo := seedBindAPI(t)
	mintBindCode(t, api, repo, "ABCD2345", bindTestUUID, authSourceMojang)
	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/bind", `{"code":"ABCD2345"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("bind = %d (%s)", w.Code, w.Body.String())
	}
	if s := repo.sessions[hashCookie(sessionCookieValue(t, w))]; !s.reauthAt.IsZero() {
		t.Fatalf("reauth_at = %v, want none after a bind-code sign-in", s.reauthAt)
	}
}

// guardedChange is a request the gate covers, the status it gets once the gate
// lets it through, and a check that it changed nothing when refused.
type guardedChange struct {
	name, method, path, body string
	ok                       int
	untouched                func(f *sessionsFixture) bool
}

var guardedChanges = []guardedChange{
	{"add a passkey", "POST", "/api/v1/account/passkey/register/begin", "", http.StatusOK,
		func(f *sessionsFixture) bool { return len(f.repo.passkeyChallenges) == 0 }},
	{"remove a passkey", "DELETE", "/api/v1/account/passkey/credentials/a", "", http.StatusNoContent,
		func(f *sessionsFixture) bool { _, ok := f.repo.passkeyCreds["a"]; return ok }},
	{"change the email", "POST", "/api/v1/account/email/start", `{"email":"steve@new.example"}`, http.StatusAccepted,
		func(f *sessionsFixture) bool { return len(f.repo.otps) == 0 }},
	{"record an unverified email", "POST", "/api/v1/account/email", `{"email":"steve@new.example"}`, http.StatusOK,
		func(f *sessionsFixture) bool { return f.repo.staff["steve"].EmailVerified }},
}

func TestGuardedChangesNeedARecentReauth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		proof  time.Duration // how long ago the session proved a factor; -1 = never
		wantOK bool
	}{
		{"never proved", -1, false},
		{"proved 6 minutes ago", 6 * time.Minute, false},
		{"proved exactly 5 minutes ago", 5 * time.Minute, false},
		{"proved 4m59s ago", 5*time.Minute - time.Second, true},
		{"proved just now", 0, true},
	} {
		for _, g := range guardedChanges {
			t.Run(tc.name+"/"+g.name, func(t *testing.T) {
				f := reauthFixture(t)
				f.api.Mailer = &captureMailer{}
				f.repo.passkeyCreds["a"] = PasskeyCredential{ID: "a", UserID: "u1", CredentialID: "c-a", CreatedAt: frozenNow}
				if tc.proof >= 0 {
					f.setReauth(laptopTok, f.api.now().Add(-tc.proof))
				}
				w := do(f.eh, g.method, g.path, g.body, jsonCookie(laptopTok))
				if tc.wantOK {
					if w.Code != g.ok {
						t.Fatalf("%s = %d (%s), want %d", g.name, w.Code, w.Body.String(), g.ok)
					}
					return
				}
				if w.Code != http.StatusForbidden || decodeErr(t, w) != "reauth_required" {
					t.Fatalf("%s = %d (%s), want 403 reauth_required", g.name, w.Code, w.Body.String())
				}
				if !g.untouched(f) {
					t.Fatalf("%s went through despite the refusal", g.name)
				}
			})
		}
	}
}

// With no passkey and no verified email the session is the account's only way
// in, so there is nothing a reauth could protect and nothing to give one with.
func TestAccountWithoutAFactorNeedsNoReauth(t *testing.T) {
	f := reauthFixture(t)
	f.api.Mailer = &captureMailer{}
	if w := do(f.eh, "POST", "/api/v1/account/passkey/register/begin", "", asCookie(alexTok)); w.Code != http.StatusOK {
		t.Fatalf("register begin = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(f.eh, "POST", "/api/v1/account/email/start", `{"email":"alex@example.net"}`, jsonCookie(alexTok)); w.Code != http.StatusAccepted {
		t.Fatalf("email start = %d (%s)", w.Code, w.Body.String())
	}
}

// An unverified address is no factor: it never received a code.
func TestUnverifiedEmailIsNoFactor(t *testing.T) {
	f := reauthFixture(t)
	f.repo.staff["alex"].Email = "alex@example.net"
	if w := do(f.eh, "POST", "/api/v1/account/passkey/register/begin", "", asCookie(alexTok)); w.Code != http.StatusOK {
		t.Fatalf("register begin = %d (%s)", w.Code, w.Body.String())
	}
}

// A passkey alone is a factor worth guarding.
func TestPasskeyAloneNeedsReauth(t *testing.T) {
	f := reauthFixture(t)
	f.repo.passkeyCreds["x"] = PasskeyCredential{ID: "x", UserID: "u2", CredentialID: "c-x", CreatedAt: frozenNow}
	w := do(f.eh, "POST", "/api/v1/account/passkey/register/begin", "", asCookie(alexTok))
	if w.Code != http.StatusForbidden || decodeErr(t, w) != "reauth_required" {
		t.Fatalf("register begin = %d (%s), want 403 reauth_required", w.Code, w.Body.String())
	}
}

// A Cloudflare Access caller is authenticated by the proxy on every request and
// has no session to mark.
func TestAccessCallerNeedsNoReauth(t *testing.T) {
	repo := newFakeRepo()
	repo.staff["op"] = &StaffUser{ID: "u1", Username: "op", Role: "admin", Email: "op@example.net", EmailVerified: true}
	repo.passkeyCreds["a"] = PasskeyCredential{ID: "a", UserID: "u1", CredentialID: "c-a", CreatedAt: frozenNow}
	api := newTestAPI(repo, newFakeCluster())
	api.Passkey = &fakePasskeyVerifier{}
	api.External = staticExternal{p: &Principal{UserID: "u1", Email: "op@example.net", Role: "admin", EmailVerified: true}}
	if w := do(api.ExternalHandler(), "POST", "/api/v1/account/passkey/register/begin", "", nil); w.Code != http.StatusOK {
		t.Fatalf("register begin = %d (%s)", w.Code, w.Body.String())
	}
}

type reauthStatusBody struct {
	Needed  bool       `json:"needed"`
	Until   *time.Time `json:"until"`
	Factors []string   `json:"factors"`
}

func getReauthStatus(t *testing.T, f *sessionsFixture, tok string) reauthStatusBody {
	t.Helper()
	w := do(f.eh, "GET", "/api/v1/account/reauth", "", asCookie(tok))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var b reauthStatusBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReauthStatusNamesTheFactors(t *testing.T) {
	f := reauthFixture(t)
	f.api.Mailer = &captureMailer{}
	f.repo.passkeyCreds["a"] = PasskeyCredential{ID: "a", UserID: "u1", CredentialID: "c-a", CreatedAt: frozenNow}
	f.repo.passkeyCreds["p"] = PasskeyCredential{ID: "p", UserID: "u3", CredentialID: "c-p", CreatedAt: frozenNow}

	steve := getReauthStatus(t, f, laptopTok)
	if !steve.Needed || steve.Until != nil || strings.Join(steve.Factors, ",") != "passkey,email" {
		t.Fatalf("player status = %+v, want needed with passkey,email", steve)
	}
	// An operator's verified email is no factor: signing in as staff by email
	// also takes in-game approval.
	pam := getReauthStatus(t, f, opTok)
	if !pam.Needed || strings.Join(pam.Factors, ",") != "passkey,sign_in" {
		t.Fatalf("operator status = %+v, want needed with passkey,sign_in", pam)
	}
	alex := getReauthStatus(t, f, alexTok)
	if alex.Needed || len(alex.Factors) != 0 {
		t.Fatalf("no-factor status = %+v, want not needed and no factors", alex)
	}

	proved := f.api.now().Add(-time.Minute)
	f.setReauth(laptopTok, proved)
	steve = getReauthStatus(t, f, laptopTok)
	if steve.Needed || steve.Until == nil || !steve.Until.Equal(proved.Add(reauthWindow)) {
		t.Fatalf("after a proof status = %+v, want not needed until %v", steve, proved.Add(reauthWindow))
	}
}

// TestReauthWithoutMailRelay: with no [smtp] relay a verified email is no way in
// (the email and op-login doors answer 503), so it is neither offered as a factor
// nor guarded; a passkey still is, and the email start door says why it cannot help.
func TestReauthWithoutMailRelay(t *testing.T) {
	f := reauthFixture(t)
	f.repo.passkeyCreds["p"] = PasskeyCredential{ID: "p", UserID: "u3", CredentialID: "c-p", CreatedAt: frozenNow}

	steve := getReauthStatus(t, f, laptopTok)
	if steve.Needed || len(steve.Factors) != 0 {
		t.Fatalf("email-only player status = %+v, want not needed and no factors", steve)
	}
	pam := getReauthStatus(t, f, opTok)
	if !pam.Needed || strings.Join(pam.Factors, ",") != "passkey" {
		t.Fatalf("operator with a passkey status = %+v, want needed with passkey only", pam)
	}
	w := do(f.eh, "POST", "/api/v1/account/reauth/email/start", "", asCookie(laptopTok))
	if code, _ := errEnvelope(t, w); w.Code != http.StatusServiceUnavailable || code != "mail_unavailable" {
		t.Fatalf("email start = %d %s, want 503 mail_unavailable", w.Code, w.Body.String())
	}
	if n := len(f.repo.otps); n != 0 {
		t.Errorf("a refused start minted %d codes", n)
	}
}

func TestReauthByEmailCode(t *testing.T) {
	f := reauthFixture(t)
	mailer := &captureMailer{}
	f.api.Mailer = mailer

	if w := do(f.eh, "POST", "/api/v1/account/reauth/email/start", "", asCookie(laptopTok)); w.Code != http.StatusAccepted {
		t.Fatalf("start = %d (%s)", w.Code, w.Body.String())
	}
	if mailer.email != "steve@example.net" || mailer.code == "" {
		t.Fatalf("code went to %q (%q), want steve@example.net", mailer.email, mailer.code)
	}
	wrong := "000000"
	if mailer.code == wrong {
		wrong = "111111"
	}
	w := do(f.eh, "POST", "/api/v1/account/reauth/email/verify", `{"code":"`+wrong+`"}`, jsonCookie(laptopTok))
	if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("wrong code = %d (%s), want 400 invalid_code", w.Code, w.Body.String())
	}
	if !f.reauthAt(laptopTok).IsZero() {
		t.Fatal("a wrong code marked the session")
	}

	w = do(f.eh, "POST", "/api/v1/account/reauth/email/verify", `{"code":"`+mailer.code+`"}`, jsonCookie(laptopTok))
	if w.Code != http.StatusOK {
		t.Fatalf("verify = %d (%s)", w.Code, w.Body.String())
	}
	var body struct {
		OK    bool      `json:"ok"`
		Until time.Time `json:"until"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if !body.OK || !body.Until.Equal(f.api.now().Add(reauthWindow)) {
		t.Fatalf("verify body = %s, want ok until now+5m", w.Body.String())
	}
	if !f.reauthAt(laptopTok).Equal(f.api.now()) {
		t.Fatalf("laptop reauth_at = %v, want now", f.reauthAt(laptopTok))
	}
	// The proof belongs to the session that gave it.
	if !f.reauthAt(phoneTok).IsZero() {
		t.Fatal("the phone was marked by the laptop's code")
	}
	if w := do(f.eh, "POST", "/api/v1/account/passkey/register/begin", "", asCookie(laptopTok)); w.Code != http.StatusOK {
		t.Fatalf("register begin after reauth = %d (%s)", w.Code, w.Body.String())
	}
	var audited bool
	for _, e := range f.repo.audits {
		audited = audited || (e.Action == "account.reauth" && e.ServerName == "email")
	}
	if !audited {
		t.Fatalf("no account.reauth audit naming the email factor: %+v", f.repo.audits)
	}
}

// A code from another step-up (the email change, a migration) does not reauth.
func TestReauthCodeIsItsOwnPurpose(t *testing.T) {
	f := reauthFixture(t)
	mailer := &captureMailer{}
	f.api.Mailer = mailer
	f.setReauth(laptopTok, f.api.now())
	if w := do(f.eh, "POST", "/api/v1/account/email/start", `{"email":"steve@new.example"}`, jsonCookie(laptopTok)); w.Code != http.StatusAccepted {
		t.Fatalf("email start = %d (%s)", w.Code, w.Body.String())
	}
	w := do(f.eh, "POST", "/api/v1/account/reauth/email/verify", `{"code":"`+mailer.code+`"}`, jsonCookie(phoneTok))
	if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("onboarding code on the reauth door = %d (%s), want 400 invalid_code", w.Code, w.Body.String())
	}
}

func TestOperatorsCannotReauthByEmail(t *testing.T) {
	f := reauthFixture(t)
	mailer := &captureMailer{}
	f.api.Mailer = mailer
	for _, path := range []string{"/api/v1/account/reauth/email/start", "/api/v1/account/reauth/email/verify"} {
		w := do(f.eh, "POST", path, `{"code":"123456"}`, jsonCookie(opTok))
		if w.Code != http.StatusForbidden || decodeErr(t, w) != "staff_reauth" {
			t.Fatalf("%s = %d (%s), want 403 staff_reauth", path, w.Code, w.Body.String())
		}
	}
	if mailer.calls != 0 {
		t.Fatal("a code was mailed to an operator")
	}
}

func TestReauthByEmailNeedsAVerifiedAddress(t *testing.T) {
	f := reauthFixture(t)
	f.api.Mailer = &captureMailer{}
	f.repo.staff["alex"].Email = "alex@example.net"
	w := do(f.eh, "POST", "/api/v1/account/reauth/email/start", "", asCookie(alexTok))
	if w.Code != http.StatusConflict || decodeErr(t, w) != "no_step_up_factor" {
		t.Fatalf("start = %d (%s), want 409 no_step_up_factor", w.Code, w.Body.String())
	}
}

func TestReauthByPasskey(t *testing.T) {
	f := reauthFixture(t)
	pv := f.api.Passkey.(*fakePasskeyVerifier)
	f.repo.passkeyCreds["a"] = PasskeyCredential{ID: "a", UserID: "u1", CredentialID: "c-a", SignCount: 4, CreatedAt: frozenNow}
	finish := func() int {
		t.Helper()
		if w := do(f.eh, "POST", "/api/v1/account/reauth/passkey/begin", "", asCookie(laptopTok)); w.Code != http.StatusOK {
			t.Fatalf("begin = %d (%s)", w.Code, w.Body.String())
		}
		if len(pv.lastUser.Credentials) != 1 || pv.lastUser.Credentials[0].CredentialID != "c-a" {
			t.Fatalf("assertion offered %+v, want the caller's own passkey", pv.lastUser.Credentials)
		}
		return do(f.eh, "POST", "/api/v1/account/reauth/passkey/finish", `{"assertion":{"id":"c-a"}}`, jsonCookie(laptopTok)).Code
	}

	pv.failErr = errors.New("assertion rejected")
	if code := finish(); code != http.StatusBadRequest {
		t.Fatalf("bad assertion = %d, want 400", code)
	}
	pv.failErr = nil
	pv.assertion = VerifiedAssertion{CredentialID: "c-a", SignCount: 2, CloneWarning: true}
	if code := finish(); code != http.StatusBadRequest {
		t.Fatalf("cloned authenticator = %d, want 400", code)
	}
	if !f.reauthAt(laptopTok).IsZero() {
		t.Fatal("a failed assertion marked the session")
	}

	pv.assertion = VerifiedAssertion{CredentialID: "c-a", SignCount: 5}
	if code := finish(); code != http.StatusOK {
		t.Fatalf("finish = %d, want 200", code)
	}
	if !f.reauthAt(laptopTok).Equal(f.api.now()) {
		t.Fatalf("reauth_at = %v, want now", f.reauthAt(laptopTok))
	}
	if got := f.repo.passkeyCreds["a"].SignCount; got != 5 {
		t.Fatalf("sign count = %d, want it advanced to 5", got)
	}
	// The challenge was spent: a replayed finish has nothing to consume.
	w := do(f.eh, "POST", "/api/v1/account/reauth/passkey/finish", `{"assertion":{"id":"c-a"}}`, jsonCookie(laptopTok))
	if w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_login_invalid" {
		t.Fatalf("replayed finish = %d (%s), want 400 passkey_login_invalid", w.Code, w.Body.String())
	}
}

// A migration's passkey challenge cannot be spent on a reauth, or the reverse.
func TestReauthPasskeyChallengeIsItsOwnPurpose(t *testing.T) {
	f := reauthFixture(t)
	pv := f.api.Passkey.(*fakePasskeyVerifier)
	pv.assertion = VerifiedAssertion{CredentialID: "c-a", SignCount: 5}
	f.repo.passkeyCreds["a"] = PasskeyCredential{ID: "a", UserID: "u1", CredentialID: "c-a", CreatedAt: frozenNow}
	if err := f.repo.CreatePasskeyChallenge(t.Context(), "m1", "u1", passkeyPurposeMigrate, []byte("s"), f.api.now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	w := do(f.eh, "POST", "/api/v1/account/reauth/passkey/finish", `{"assertion":{"id":"c-a"}}`, jsonCookie(laptopTok))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("finish on a migration challenge = %d (%s), want 400", w.Code, w.Body.String())
	}
}

// Proving an address by code is an email reauth, so a first-time setup can go on
// to its next guarded step.
func TestVerifyingAnEmailCountsAsReauth(t *testing.T) {
	f := reauthFixture(t)
	mailer := &captureMailer{}
	f.api.Mailer = mailer
	if w := do(f.eh, "POST", "/api/v1/account/email/start", `{"email":"alex@example.net"}`, jsonCookie(alexTok)); w.Code != http.StatusAccepted {
		t.Fatalf("start = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(f.eh, "POST", "/api/v1/account/email/verify", `{"code":"`+mailer.code+`"}`, jsonCookie(alexTok)); w.Code != http.StatusOK {
		t.Fatalf("verify = %d (%s)", w.Code, w.Body.String())
	}
	if !f.reauthAt(alexTok).Equal(f.api.now()) {
		t.Fatalf("reauth_at = %v, want now", f.reauthAt(alexTok))
	}
}

func TestRegisteringAPasskeyCountsAsReauth(t *testing.T) {
	f := reauthFixture(t)
	f.api.Passkey.(*fakePasskeyVerifier).credential = VerifiedCredential{CredentialID: "c-new", PublicKey: "pk"}
	if w := do(f.eh, "POST", "/api/v1/account/passkey/register/begin", "", asCookie(alexTok)); w.Code != http.StatusOK {
		t.Fatalf("begin = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(f.eh, "POST", "/api/v1/account/passkey/register/finish", `{"attestation":{"id":"x"}}`, jsonCookie(alexTok)); w.Code != http.StatusCreated {
		t.Fatalf("finish = %d (%s)", w.Code, w.Body.String())
	}
	if !f.reauthAt(alexTok).Equal(f.api.now()) {
		t.Fatalf("reauth_at = %v, want now", f.reauthAt(alexTok))
	}
}

// ---- change notices ----

func noticeFixture(t *testing.T) (*sessionsFixture, *noticeMailer) {
	t.Helper()
	f := reauthFixture(t)
	mailer := &noticeMailer{}
	f.api.Mailer = mailer
	f.api.ClientIPHeader = "CF-Connecting-IP"
	f.setReauth(laptopTok, f.api.now())
	return f, mailer
}

func fromIP(h map[string]string) map[string]string {
	h["CF-Connecting-IP"] = "203.0.113.9"
	return h
}

func onlyNotice(t *testing.T, m *noticeMailer) (to, subject, body string) {
	t.Helper()
	if len(m.notices) != 1 {
		t.Fatalf("notices = %q, want exactly one", m.notices)
	}
	parts := strings.SplitN(m.notices[0], "|", 3)
	return parts[0], parts[1], parts[2]
}

func TestRemovingAPasskeyMailsTheAccount(t *testing.T) {
	f, mailer := noticeFixture(t)
	f.repo.passkeyCreds["a"] = PasskeyCredential{ID: "a", UserID: "u1", CredentialID: "c-a", CreatedAt: frozenNow}
	if w := do(f.eh, "DELETE", "/api/v1/account/passkey/credentials/a", "", fromIP(asCookie(laptopTok))); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d (%s)", w.Code, w.Body.String())
	}
	to, subject, body := onlyNotice(t, mailer)
	if to != "steve@example.net" || subject != "Felis 已删除 Passkey · passkey removed" {
		t.Fatalf("notice to %q subject %q", to, subject)
	}
	for _, want := range []string{
		"A passkey was just removed from your Felis account, and every other device was signed out.",
		"Time: 2023-11-14 22:13 UTC",
		"From IP: 203.0.113.9",
		"check the passkeys that remain",
		"来源 IP：203.0.113.9",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("notice body lacks %q:\n%s", want, body)
		}
	}
}

func TestAddingAPasskeyMailsTheAccount(t *testing.T) {
	f, mailer := noticeFixture(t)
	f.api.Passkey.(*fakePasskeyVerifier).credential = VerifiedCredential{CredentialID: "c-new", PublicKey: "pk"}
	if w := do(f.eh, "POST", "/api/v1/account/passkey/register/begin", "", asCookie(laptopTok)); w.Code != http.StatusOK {
		t.Fatalf("begin = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(f.eh, "POST", "/api/v1/account/passkey/register/finish", `{"attestation":{"id":"x"}}`, fromIP(jsonCookie(laptopTok))); w.Code != http.StatusCreated {
		t.Fatalf("finish = %d (%s)", w.Code, w.Body.String())
	}
	to, subject, body := onlyNotice(t, mailer)
	if to != "steve@example.net" || subject != "Felis 已添加 Passkey · passkey added" ||
		!strings.Contains(body, "A passkey was just added to your Felis account.") ||
		!strings.Contains(body, "remove that passkey") {
		t.Fatalf("notice to %q subject %q body:\n%s", to, subject, body)
	}
}

// Replacing the address mails the OLD one, which is the mailbox the owner still
// reads if someone else made the change. The new address is masked.
func TestChangingTheEmailMailsTheOldAddress(t *testing.T) {
	f, mailer := noticeFixture(t)
	if w := do(f.eh, "POST", "/api/v1/account/email/start", `{"email":"steve@new.example"}`, jsonCookie(laptopTok)); w.Code != http.StatusAccepted {
		t.Fatalf("start = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(f.eh, "POST", "/api/v1/account/email/verify", `{"code":"`+mailer.code+`"}`, fromIP(jsonCookie(laptopTok))); w.Code != http.StatusOK {
		t.Fatalf("verify = %d (%s)", w.Code, w.Body.String())
	}
	to, subject, body := onlyNotice(t, mailer)
	if to != "steve@example.net" || subject != "Felis 邮箱已更换 · email changed" {
		t.Fatalf("notice to %q subject %q", to, subject)
	}
	if !strings.Contains(body, "The email on your Felis account was just changed to s***@new.example.") ||
		!strings.Contains(body, "From IP: 203.0.113.9") {
		t.Fatalf("notice body:\n%s", body)
	}
	if strings.Contains(body, "steve@new.example") {
		t.Fatalf("notice hands the new address to the old mailbox:\n%s", body)
	}
}

// A first verification and a re-verification of the same address move nothing.
func TestVerifyingAFirstOrSameEmailMailsNoNotice(t *testing.T) {
	for _, tc := range []struct {
		name, tok, address string
	}{
		{"first address", alexTok, "alex@example.net"},
		{"same address", laptopTok, "Steve@Example.NET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, mailer := noticeFixture(t)
			if w := do(f.eh, "POST", "/api/v1/account/email/start", `{"email":"`+tc.address+`"}`, jsonCookie(tc.tok)); w.Code != http.StatusAccepted {
				t.Fatalf("start = %d (%s)", w.Code, w.Body.String())
			}
			if w := do(f.eh, "POST", "/api/v1/account/email/verify", `{"code":"`+mailer.code+`"}`, jsonCookie(tc.tok)); w.Code != http.StatusOK {
				t.Fatalf("verify = %d (%s)", w.Code, w.Body.String())
			}
			if len(mailer.notices) != 0 {
				t.Fatalf("notices = %q, want none", mailer.notices)
			}
		})
	}
}

// Nothing proves an unverified address belongs to the owner, so nothing is sent
// there.
func TestPasskeyNoticeSkipsAnUnverifiedAddress(t *testing.T) {
	f, mailer := noticeFixture(t)
	f.repo.staff["alex"].Email = "alex@example.net"
	// Two passkeys, so removing one is allowed without a verified email.
	f.repo.passkeyCreds["x"] = PasskeyCredential{ID: "x", UserID: "u2", CredentialID: "c-x", CreatedAt: frozenNow}
	f.repo.passkeyCreds["y"] = PasskeyCredential{ID: "y", UserID: "u2", CredentialID: "c-y", CreatedAt: frozenNow}
	f.setReauth(alexTok, f.api.now())
	if w := do(f.eh, "DELETE", "/api/v1/account/passkey/credentials/x", "", asCookie(alexTok)); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d (%s)", w.Code, w.Body.String())
	}
	if len(mailer.notices) != 0 {
		t.Fatalf("notices = %q, want none", mailer.notices)
	}
}

func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{
		"alice@example.com": "a***@example.com",
		"李雷@example.cn":     "李***@example.cn",
		"a@b.c":             "a***@b.c",
		"broken":            "***",
		"@nolocal.example":  "***",
	} {
		if got := maskEmail(in); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}
