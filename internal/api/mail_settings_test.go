package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/mailsettings"
)

const mailSettingsPath = "/api/v1/settings/smtp"

// fakeRelay records construction and sends, with pingErr controlling Ping and SendNotice failures.
type fakeRelay struct {
	settings mailsettings.Settings
	log      *[]string
	pingErr  error
}

func (f fakeRelay) SendOTP(_ context.Context, email, _ string) error {
	*f.log = append(*f.log, "otp "+f.settings.Host+" "+email)
	return nil
}
func (f fakeRelay) SendNotice(_ context.Context, email, _, _ string) error {
	*f.log = append(*f.log, "notice "+f.settings.Host+" "+email)
	return f.pingErr
}
func (f fakeRelay) Ping(context.Context) error {
	*f.log = append(*f.log, "ping "+f.settings.Host+" pw="+f.settings.Password)
	return f.pingErr
}

type mailFixture struct {
	a       *API
	repo    *fakeRepo
	log     []string
	pingErr error
	p       *Principal
}

func newMailFixture(host *mailsettings.Settings) *mailFixture {
	f := &mailFixture{repo: newFakeRepo()}
	f.a = newTestAPI(f.repo, newFakeCluster())
	f.p = &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true, ViaSession: true, EmailVerified: true}
	f.repo.passkeyCreds["owner-key"] = PasskeyCredential{ID: "owner-key", UserID: "owner", UserVerified: true}
	f.repo.staff["owner"] = &StaffUser{ID: "owner", Username: "owner", Email: "owner@example.net", Role: "owner", EmailVerified: true}
	f.a.External = staticExternal{p: f.p}
	f.a.MailSettings = &MailSettings{Repo: f.repo, Host: host, NewMailer: func(s mailsettings.Settings) OTPMailer {
		return fakeRelay{settings: s, log: &f.log, pingErr: f.pingErr}
	}}
	return f
}

func hostRelay() *mailsettings.Settings {
	return &mailsettings.Settings{Host: "host-relay.example.com", Port: 587, From: "noreply@example.com", Username: "noreply@example.com", Password: "host-secret", RequireTLS: true}
}

func (f *mailFixture) view(t *testing.T) mailSettingsView {
	t.Helper()
	w := do(f.a.ExternalHandler(), "GET", mailSettingsPath, "", nil)
	if w.Code != 200 {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("the password left the API: %s", w.Body.String())
	}
	var v mailSettingsView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func relayBody(revision string, password *string, port int) string {
	body := map[string]any{"host": "panel-relay.example.com", "port": port, "from": "mail@example.com", "username": "mail@example.com", "require_tls": true, "max_per_hour": 60, "revision": revision}
	if password != nil {
		body["password"] = *password
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func TestMailSettingsReadShowsWhereTheRelayComesFrom(t *testing.T) {
	if v := newMailFixture(nil).view(t); v.Source != "none" || v.Host != "" || v.PasswordSet {
		t.Errorf("no relay anywhere = %+v", v)
	}
	v := newMailFixture(hostRelay()).view(t)
	if v.Source != "host" || v.Host != "host-relay.example.com" || !v.PasswordSet || v.Revision != "" {
		t.Errorf("felis.toml's relay = %+v", v)
	}
}

func TestMailSettingsSaveChecksTheRelayAndOverridesFelisToml(t *testing.T) {
	f := newMailFixture(hostRelay())
	w := do(f.a.ExternalHandler(), "PUT", mailSettingsPath, relayBody("", nil, 587), jsonHeader)
	if w.Code != 403 || decodeErr(t, w) != "reauth_required" || len(f.log) != 0 {
		t.Fatalf("stale save = %d %s (sent %v)", w.Code, w.Body.String(), f.log)
	}
	f.p.ReauthAt = f.a.now()

	f.pingErr = errors.New("smtp: auth as mail@example.com: 535 bad credentials")
	w = do(f.a.ExternalHandler(), "PUT", mailSettingsPath, relayBody("", nil, 587), jsonHeader)
	if w.Code != 400 || decodeErr(t, w) != "smtp_auth" {
		t.Fatalf("a refused relay = %d %s", w.Code, w.Body.String())
	}
	if _, saved := f.repo.settings[mailsettings.Key]; saved {
		t.Fatal("a relay that failed its check was saved")
	}
	var reply map[string]map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reply["error"]["message"], "535") {
		t.Errorf("the relay's own reply reached the panel: %s", w.Body.String())
	}

	f.pingErr = nil
	f.log = nil
	w = do(f.a.ExternalHandler(), "PUT", mailSettingsPath, relayBody("", nil, 587), jsonHeader)
	if w.Code != 200 {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	if len(f.log) != 1 || f.log[0] != "ping panel-relay.example.com pw=host-secret" {
		t.Errorf("the save did not check the relay with the carried-over password: %v", f.log)
	}
	// The very next send uses the saved relay: the save dropped the cached one.
	f.log = nil
	if err := f.a.deliverOTP(context.Background(), "player@example.net", "123456"); err != nil {
		t.Fatal(err)
	}
	if len(f.log) != 1 || f.log[0] != "otp panel-relay.example.com player@example.net" {
		t.Errorf("a code went through %v, want the panel's relay", f.log)
	}
	v := f.view(t)
	if v.Source != "panel" || v.Host != "panel-relay.example.com" || !v.PasswordSet || v.MaxPerHour != 60 || v.Revision == "" {
		t.Errorf("after the save = %+v", v)
	}
	if len(f.repo.audits) != 1 || f.repo.audits[0].Action != "smtp.updated" || strings.Contains(string(f.repo.audits[0].Payload), "secret") {
		t.Errorf("audit = %+v", f.repo.audits)
	}

	if got := f.a.mailGate().limit; got != MailRateLimit(60) {
		t.Errorf("mail budget = %+v, want the relay's hourly cap", got)
	}

	// A stale revision is refused; an explicit empty password clears it.
	if w = do(f.a.ExternalHandler(), "PUT", mailSettingsPath, relayBody("", nil, 587), jsonHeader); w.Code != 409 {
		t.Errorf("a stale revision = %d %s", w.Code, w.Body.String())
	}
	empty := ""
	if w = do(f.a.ExternalHandler(), "PUT", mailSettingsPath, relayBody(v.Revision, &empty, 587), jsonHeader); w.Code != 200 {
		t.Fatalf("clearing the password = %d %s", w.Code, w.Body.String())
	}
	if f.view(t).PasswordSet {
		t.Error("an explicit empty password was kept")
	}
}

func TestMailSettingsRefusesAPortOffTheSubmissionList(t *testing.T) {
	f := newMailFixture(nil)
	f.p.ReauthAt = f.a.now()
	for _, path := range []string{mailSettingsPath, mailSettingsPath + "/test"} {
		method := "PUT"
		if strings.HasSuffix(path, "/test") {
			method = "POST"
		}
		w := do(f.a.ExternalHandler(), method, path, relayBody("", nil, 5432), jsonHeader)
		if w.Code != 400 || len(f.log) != 0 {
			t.Errorf("%s with port 5432 = %d %s (sent %v)", path, w.Code, w.Body.String(), f.log)
		}
	}
}

func TestMailSettingsClearHandsTheRelayBack(t *testing.T) {
	f := newMailFixture(hostRelay())
	f.p.ReauthAt = f.a.now()
	if w := do(f.a.ExternalHandler(), "PUT", mailSettingsPath, relayBody("", nil, 465), jsonHeader); w.Code != 200 {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	f.p.ReauthAt = time.Time{}
	if w := do(f.a.ExternalHandler(), "DELETE", mailSettingsPath, "", nil); w.Code != 403 {
		t.Fatalf("a stale clear = %d", w.Code)
	}
	f.p.ReauthAt = f.a.now()
	if w := do(f.a.ExternalHandler(), "DELETE", mailSettingsPath, "", nil); w.Code != 200 {
		t.Fatalf("clear = %d %s", w.Code, w.Body.String())
	}
	if v := f.view(t); v.Source != "host" || v.Host != "host-relay.example.com" {
		t.Errorf("after clearing = %+v", v)
	}
	f.log = nil
	if err := f.a.deliverOTP(context.Background(), "player@example.net", "123456"); err != nil || len(f.log) != 1 || !strings.Contains(f.log[0], "host-relay") {
		t.Errorf("after clearing a code went through %v (%v)", f.log, err)
	}
}

func TestMailSettingsTestMailsTheOwner(t *testing.T) {
	f := newMailFixture(nil)
	f.p.ReauthAt = f.a.now()
	w := do(f.a.ExternalHandler(), "POST", mailSettingsPath+"/test", relayBody("", nil, 587), jsonHeader)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "owner@example.net") {
		t.Fatalf("test = %d %s", w.Code, w.Body.String())
	}
	if len(f.log) != 1 || f.log[0] != "notice panel-relay.example.com owner@example.net" {
		t.Errorf("sent %v, want one notice to the Owner", f.log)
	}
	if _, saved := f.repo.settings[mailsettings.Key]; saved {
		t.Error("a test saved the relay")
	}
	f.p.EmailVerified = false
	f.log = nil
	if w = do(f.a.ExternalHandler(), "POST", mailSettingsPath+"/test", relayBody("", nil, 587), jsonHeader); w.Code != 200 || len(f.log) != 1 || !strings.HasPrefix(f.log[0], "ping") {
		t.Errorf("without a verified address = %d, sent %v", w.Code, f.log)
	}
}

func TestMailSettingsOwnerOnly(t *testing.T) {
	f := newMailFixture(hostRelay())
	f.a.External = staticExternal{p: &Principal{UserID: "admin", Role: "admin", ViaAdminAccess: true}}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		if w := do(f.a.ExternalHandler(), method, mailSettingsPath, relayBody("", nil, 587), jsonHeader); w.Code != 403 {
			t.Errorf("admin %s = %d", method, w.Code)
		}
	}
}

func TestRelayTestErrorCategories(t *testing.T) {
	for msg, want := range map[string]string{
		"smtp: dial smtp.example.com:587: connection refused":     "smtp_unreachable",
		"refusing to dial 169.254.169.254: not a routable":        "smtp_unreachable",
		"smtp: starttls: tls: handshake failure":                  "smtp_tls",
		"smtp: smtp.example.com:25 does not offer STARTTLS":       "smtp_tls",
		"smtp: auth as a@b.c: 535 5.7.8":                          "smtp_auth",
		"smtp: relay refused mail from a@b.c: 551 not authorised": "smtp_sender_refused",
		"smtp: RCPT TO: 550 no such user":                         "smtp_recipient_refused",
		"smtp: write message: broken pipe":                        "smtp_failed",
	} {
		if got := relayTestError(errors.New(msg)).code; got != want {
			t.Errorf("%q -> %s, want %s", msg, got, want)
		}
	}
}

func TestMailSettingsRevisionSurvivesReSpacing(t *testing.T) {
	f := newMailFixture(nil)
	f.p.ReauthAt = f.a.now()
	w := do(f.a.ExternalHandler(), "PUT", mailSettingsPath, relayBody("", nil, 587), jsonHeader)
	if w.Code != 200 {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	var saved mailSettingsView
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(f.repo.settings[mailsettings.Key], &stored); err != nil {
		t.Fatal(err)
	}
	respaced, _ := json.MarshalIndent(stored, "", "    ")
	f.repo.settings[mailsettings.Key] = respaced
	if got := f.view(t).Revision; got != saved.Revision {
		t.Fatalf("revision after re-spacing = %s, want %s", got, saved.Revision)
	}
	if w = do(f.a.ExternalHandler(), "PUT", mailSettingsPath, relayBody(saved.Revision, nil, 465), jsonHeader); w.Code != 200 {
		t.Fatalf("second save = %d %s", w.Code, w.Body.String())
	}
}
