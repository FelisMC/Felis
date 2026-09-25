package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sessionsFixture signs steve (u1) in on a laptop and a phone and alex (u2) on
// one device, each by a real cookie, so the caller's own session is whichever
// one SessionAuth resolved from the request.
type sessionsFixture struct {
	repo *fakeRepo
	api  *API
	eh   http.Handler
}

const (
	laptopTok = "tok-laptop"
	phoneTok  = "tok-phone"
	alexTok   = "tok-alex"
)

func newSessionsFixture(t *testing.T) *sessionsFixture {
	t.Helper()
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["steve"] = &StaffUser{ID: "u1", Username: "steve", Email: "steve@example.net", Role: "user", EmailVerified: true}
	repo.staff["alex"] = &StaffUser{ID: "u2", Username: "alex", Role: "user"}
	api := newTestAPI(repo, newFakeCluster())
	api.External = SessionAuth{Repo: repo, RootDomain: testRoot, Now: api.now}
	now := api.now()
	for tok, s := range map[string]*fakeSession{
		laptopTok: {userID: "u1", lastSeen: now.Add(-10 * time.Minute), userAgent: "Firefox on Linux", clientIP: "203.0.113.5"},
		phoneTok:  {userID: "u1", lastSeen: now.Add(-2 * time.Hour), userAgent: "Safari on iPhone", clientIP: "198.51.100.7"},
		alexTok:   {userID: "u2", lastSeen: now.Add(-time.Minute)},
	} {
		s.expiresAt = now.Add(time.Hour)
		repo.sessions[hashCookie(tok)] = s
	}
	return &sessionsFixture{repo: repo, api: api, eh: api.ExternalHandler()}
}

func asCookie(tok string) map[string]string {
	return map[string]string{"Cookie": sessionCookieName + "=" + tok}
}

func (f *sessionsFixture) revoked(tok string) bool {
	return f.repo.sessions[hashCookie(tok)].revoked
}

func decodeSessions(t *testing.T, w *httptest.ResponseRecorder) []SessionView {
	t.Helper()
	var body struct {
		Sessions []SessionView `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("sessions body: %v (%s)", err, w.Body.String())
	}
	return body.Sessions
}

func TestMySessionsListsOwnDevicesAndMarksThisOne(t *testing.T) {
	f := newSessionsFixture(t)
	w := do(f.eh, "GET", "/api/v1/account/sessions", "", asCookie(phoneTok))
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d (%s)", w.Code, w.Body.String())
	}
	got := decodeSessions(t, w)
	if len(got) != 2 {
		t.Fatalf("listed %d sessions, want steve's 2 (alex's is not his): %+v", len(got), got)
	}
	// Most recently seen first; the phone is the device asking, though it was seen
	// less recently than the laptop until this very request.
	if got[0].TokenHash != hashCookie(phoneTok) || got[1].TokenHash != hashCookie(laptopTok) {
		t.Fatalf("order = %s, %s; want phone (just touched) then laptop", got[0].UserAgent, got[1].UserAgent)
	}
	if !got[0].Current || got[1].Current {
		t.Fatalf("current flags = %v, %v; want only the phone", got[0].Current, got[1].Current)
	}
	if got[1].UserAgent != "Firefox on Linux" || got[1].ClientIP != "203.0.113.5" {
		t.Fatalf("laptop device = %q from %q", got[1].UserAgent, got[1].ClientIP)
	}
	if strings.Count(w.Body.String(), `"current"`) != 1 {
		t.Fatalf("current must be omitted on every other session: %s", w.Body.String())
	}
}

// A cookie that rode along beside some other credential is not the session the
// caller signed in with, so nothing is marked current and "sign out the others"
// keeps nothing back.
func TestMySessionsMarkNothingWithoutASessionPrincipal(t *testing.T) {
	f := newSessionsFixture(t)
	f.api.External = staticExternal{p: &Principal{UserID: "u1", Role: "user"}}
	eh := f.api.ExternalHandler()

	w := do(eh, "GET", "/api/v1/account/sessions", "", asCookie(phoneTok))
	for _, s := range decodeSessions(t, w) {
		if s.Current {
			t.Fatalf("session %s marked current for an Access-signed caller", s.UserAgent)
		}
	}
	if w := do(eh, "POST", "/api/v1/account/sessions/revoke-others", "", asCookie(phoneTok)); w.Code != http.StatusOK {
		t.Fatalf("revoke-others = %d (%s)", w.Code, w.Body.String())
	}
	if !f.revoked(phoneTok) || !f.revoked(laptopTok) {
		t.Fatal("an Access-signed caller's revoke-others must end every session")
	}
}

func TestRevokeMySessionOnlyReachesOwnSessions(t *testing.T) {
	f := newSessionsFixture(t)

	w := do(f.eh, "DELETE", "/api/v1/account/sessions/"+hashCookie(alexTok), "", asCookie(laptopTok))
	if w.Code != http.StatusNotFound || decodeErr(t, w) != "session_not_found" {
		t.Fatalf("revoke alex's session as steve = %d (%s), want 404 session_not_found", w.Code, w.Body.String())
	}
	if f.revoked(alexTok) {
		t.Fatal("steve ended alex's session")
	}

	w = do(f.eh, "DELETE", "/api/v1/account/sessions/"+hashCookie(phoneTok), "", asCookie(laptopTok))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"signed_out":false`) {
		t.Fatalf("revoke phone from laptop = %d (%s), want 200 signed_out:false", w.Code, w.Body.String())
	}
	if !f.revoked(phoneTok) || f.revoked(laptopTok) {
		t.Fatal("want the phone ended and the laptop still signed in")
	}
	if c := w.Header().Get("Set-Cookie"); c != "" {
		t.Fatalf("ending another device must leave this one's cookie alone, got Set-Cookie %q", c)
	}
	if last := f.repo.audits[len(f.repo.audits)-1]; last.Action != "account.session.revoked" || last.ActorUserID != "u1" {
		t.Fatalf("audit = %+v", last)
	}
}

func TestRevokingThisSessionSignsOut(t *testing.T) {
	f := newSessionsFixture(t)
	w := do(f.eh, "DELETE", "/api/v1/account/sessions/"+hashCookie(laptopTok), "", asCookie(laptopTok))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"signed_out":true`) {
		t.Fatalf("revoke own session = %d (%s), want 200 signed_out:true", w.Code, w.Body.String())
	}
	if c := w.Header().Get("Set-Cookie"); !strings.HasPrefix(c, sessionCookieName+"=;") || !strings.Contains(c, "Max-Age=0") {
		t.Fatalf("Set-Cookie = %q, want the session cookie cleared", c)
	}
	if w := do(f.eh, "GET", "/api/v1/account/sessions", "", asCookie(laptopTok)); w.Code != http.StatusUnauthorized {
		t.Fatalf("the ended session still authenticates: %d", w.Code)
	}
}

func TestRevokeOtherSessionsKeepsThisOne(t *testing.T) {
	f := newSessionsFixture(t)
	w := do(f.eh, "POST", "/api/v1/account/sessions/revoke-others", "", asCookie(laptopTok))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"revoked":1}` {
		t.Fatalf("revoke-others = %d (%s), want 200 {\"revoked\":1}", w.Code, w.Body.String())
	}
	if f.revoked(laptopTok) || !f.revoked(phoneTok) || f.revoked(alexTok) {
		t.Fatalf("after revoke-others laptop=%v phone=%v alex=%v, want only the phone ended",
			f.revoked(laptopTok), f.revoked(phoneTok), f.revoked(alexTok))
	}
}

// The admin route names the user in its path; a hash of someone else's session
// under it must not end that session.
func TestAdminRevokeSessionChecksWhoseItIs(t *testing.T) {
	f := newSessionsFixture(t)
	f.api.External = staticExternal{p: &Principal{UserID: "own", Role: "owner", ViaAdminAccess: true}}
	eh := f.api.ExternalHandler()

	w := do(eh, "DELETE", "/api/v1/users/u1/sessions/"+hashCookie(alexTok), "", nil)
	if w.Code != http.StatusNotFound || decodeErr(t, w) != "session_not_found" {
		t.Fatalf("alex's hash under steve = %d (%s), want 404 session_not_found", w.Code, w.Body.String())
	}
	if f.revoked(alexTok) {
		t.Fatal("a hash under another user's path ended alex's session")
	}
	if w := do(eh, "DELETE", "/api/v1/users/u2/sessions/"+hashCookie(alexTok), "", nil); w.Code != http.StatusOK {
		t.Fatalf("alex's hash under alex = %d (%s)", w.Code, w.Body.String())
	}
	if !f.revoked(alexTok) {
		t.Fatal("the matching revoke did not end the session")
	}
}

func TestRemovingAPasskeySignsOutOtherDevices(t *testing.T) {
	f := newSessionsFixture(t)
	f.repo.passkeyCreds["a"] = PasskeyCredential{ID: "a", UserID: "u1", CredentialID: "c-a", CreatedAt: frozenNow}

	if w := do(f.eh, "DELETE", "/api/v1/account/passkey/credentials/a", "", asCookie(laptopTok)); w.Code != http.StatusNoContent {
		t.Fatalf("delete passkey = %d (%s)", w.Code, w.Body.String())
	}
	if f.revoked(laptopTok) || !f.revoked(phoneTok) || f.revoked(alexTok) {
		t.Fatalf("after passkey removal laptop=%v phone=%v alex=%v, want only the phone ended",
			f.revoked(laptopTok), f.revoked(phoneTok), f.revoked(alexTok))
	}
}

func TestVerifyingANewEmailSignsOutOtherDevices(t *testing.T) {
	f := newSessionsFixture(t)
	mailer := &captureMailer{}
	f.api.Mailer = mailer
	hdr := asCookie(laptopTok)
	hdr["Content-Type"] = "application/json"

	if w := do(f.eh, "POST", "/api/v1/account/email/start", `{"email":"steve@new.example"}`, hdr); w.Code != http.StatusAccepted {
		t.Fatalf("start = %d (%s)", w.Code, w.Body.String())
	}
	if f.revoked(phoneTok) {
		t.Fatal("asking for a code must not sign anything out yet")
	}
	if w := do(f.eh, "POST", "/api/v1/account/email/verify", `{"code":"`+mailer.code+`"}`, hdr); w.Code != http.StatusOK {
		t.Fatalf("verify = %d (%s)", w.Code, w.Body.String())
	}
	if f.revoked(laptopTok) || !f.revoked(phoneTok) || f.revoked(alexTok) {
		t.Fatalf("after email change laptop=%v phone=%v alex=%v, want only the phone ended",
			f.revoked(laptopTok), f.revoked(phoneTok), f.revoked(alexTok))
	}
}

// With no verified address before, no session was opened through one, so a
// first verification signs nothing out; nor does proving the same address again.
func TestVerifyingAFirstOrSameEmailKeepsOtherDevices(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verified bool
		address  string
	}{
		{"first address", false, "steve@new.example"},
		{"same address again", true, "Steve@Example.NET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSessionsFixture(t)
			f.repo.staff["steve"].EmailVerified = tc.verified
			mailer := &captureMailer{}
			f.api.Mailer = mailer
			hdr := asCookie(laptopTok)
			hdr["Content-Type"] = "application/json"
			if w := do(f.eh, "POST", "/api/v1/account/email/start", `{"email":"`+tc.address+`"}`, hdr); w.Code != http.StatusAccepted {
				t.Fatalf("start = %d (%s)", w.Code, w.Body.String())
			}
			if w := do(f.eh, "POST", "/api/v1/account/email/verify", `{"code":"`+mailer.code+`"}`, hdr); w.Code != http.StatusOK {
				t.Fatalf("verify = %d (%s)", w.Code, w.Body.String())
			}
			if f.revoked(phoneTok) {
				t.Fatal("the phone was signed out")
			}
		})
	}
}

// The change has committed by the time the other sessions are signed out; a
// failure there must not report the change itself as failed.
func TestPasskeyRemovalSucceedsWhenSigningOutOthersFails(t *testing.T) {
	f := newSessionsFixture(t)
	f.repo.passkeyCreds["a"] = PasskeyCredential{ID: "a", UserID: "u1", CredentialID: "c-a", CreatedAt: frozenNow}
	f.repo.failRevokeOthers = errors.New("db blip")

	if w := do(f.eh, "DELETE", "/api/v1/account/passkey/credentials/a", "", asCookie(laptopTok)); w.Code != http.StatusNoContent {
		t.Fatalf("delete passkey = %d (%s), want 204 despite the sign-out failure", w.Code, w.Body.String())
	}
	if _, kept := f.repo.passkeyCreds["a"]; kept {
		t.Fatal("the passkey must still be removed")
	}
}

func TestSessionActivityIsRecordedAtMostOnceAMinute(t *testing.T) {
	f := newSessionsFixture(t)
	now := f.api.now()
	laptop := f.repo.sessions[hashCookie(laptopTok)]

	laptop.lastSeen = now.Add(-30 * time.Second)
	do(f.eh, "GET", "/api/v1/me", "", asCookie(laptopTok))
	if laptop.touches != 0 {
		t.Fatalf("a session seen 30s ago was touched %d times, want 0", laptop.touches)
	}

	laptop.lastSeen = now.Add(-2 * time.Minute)
	do(f.eh, "GET", "/api/v1/me", "", asCookie(laptopTok))
	if laptop.touches != 1 || !laptop.lastSeen.Equal(now) {
		t.Fatalf("a session seen 2m ago: touches=%d lastSeen=%v, want 1 touch to %v", laptop.touches, laptop.lastSeen, now)
	}

	// A failed touch leaves the request authenticated.
	laptop.lastSeen = now.Add(-2 * time.Minute)
	f.repo.failTouchSession = errors.New("db blip")
	if w := do(f.eh, "GET", "/api/v1/me", "", asCookie(laptopTok)); w.Code != http.StatusOK {
		t.Fatalf("/me with a failing touch = %d, want 200", w.Code)
	}
}

func TestSignInRecordsTheDevice(t *testing.T) {
	api, repo, mailer := seedLoginEmailAPI(t)
	api.ClientIPHeader = "CF-Connecting-IP"
	eh := api.ExternalHandler()
	if w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"player@example.net"}`, jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("start = %d", w.Code)
	}
	ua := "Mozilla/5.0 x" + strings.Repeat("é", 200) // 413 bytes, é two each
	w := do(eh, "POST", "/api/v1/auth/email/verify", `{"email":"player@example.net","code":"`+mailer.code+`"}`, map[string]string{
		"Content-Type": "application/json", "User-Agent": ua, "CF-Connecting-IP": "2001:db8::7",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("verify = %d (%s)", w.Code, w.Body.String())
	}
	if len(repo.sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(repo.sessions))
	}
	for _, s := range repo.sessions {
		if s.clientIP != "2001:db8::7" {
			t.Errorf("client_ip = %q, want the edge's 2001:db8::7", s.clientIP)
		}
		// 13 bytes of prefix leave 243 for é: byte 256 falls inside the 122nd, so
		// the cut keeps 121 of them, 255 bytes.
		if want := "Mozilla/5.0 x" + strings.Repeat("é", 121); s.userAgent != want {
			t.Errorf("user_agent = %d bytes %q, want the first 256 bytes on a rune boundary", len(s.userAgent), s.userAgent)
		}
	}
}
