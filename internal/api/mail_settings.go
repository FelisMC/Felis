package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/mailsettings"
)

// MailSettings resolves the relay felis-api sends codes and notices through: the one the
// Owner saved in the panel (mailsettings.Key) when there is one, else the [smtp] relay
// felis.toml configures. Each send resolves it again, so a relay saved in the panel
// takes effect without restarting felis-api.
type MailSettings struct {
	Repo Repo
	// Host is felis.toml's relay with its password, nil when felis.toml has none.
	Host *mailsettings.Settings
	// NewMailer turns a relay into a sender. cmd/felis wires the mail package; the
	// sender it returns also offers Ping and SendNotice.
	NewMailer func(mailsettings.Settings) OTPMailer

	mu     sync.Mutex
	cached *resolvedMail
	at     time.Time
}

// resolvedMail is one read of the row: the stored bytes (for compare-and-set) and the
// relay they hold, nil when the panel has none.
type resolvedMail struct {
	raw   []byte
	panel *mailsettings.Settings
}

// mailSettingsTTL bounds how stale a resolved relay may be: the row is read at most this
// often, and a save in this process drops the cache at once.
const mailSettingsTTL = 15 * time.Second

func (m *MailSettings) resolve(ctx context.Context, now time.Time) (resolvedMail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cached != nil && now.Sub(m.at) < mailSettingsTTL {
		return *m.cached, nil
	}
	raw, err := m.Repo.GetSetting(ctx, mailsettings.Key)
	if errors.Is(err, ErrNotFound) {
		raw, err = nil, nil
	}
	if err != nil {
		if m.cached != nil {
			// The store is out: keep sending through the last relay read.
			return *m.cached, nil
		}
		return resolvedMail{}, err
	}
	panel, err := mailsettings.Parse(raw)
	if err != nil {
		return resolvedMail{}, err
	}
	m.cached, m.at = &resolvedMail{raw: raw, panel: panel}, now
	return *m.cached, nil
}

func (m *MailSettings) invalidate() {
	m.mu.Lock()
	m.cached = nil
	m.mu.Unlock()
}

// effective is the relay in force and where it comes from: "panel", "host" or "none".
func (m *MailSettings) effective(r resolvedMail) (*mailsettings.Settings, string) {
	switch {
	case r.panel != nil:
		return r.panel, "panel"
	case m.Host != nil:
		return m.Host, "host"
	}
	return nil, "none"
}

// MailRateLimit is the install-wide mail budget for an hourly cap (0: the default).
func MailRateLimit(perHour int) RateLimit {
	if perHour <= 0 {
		perHour = config.DefaultMailPerHour
	}
	return RateLimit{Burst: max(perHour/4, 5), PerMinute: float64(perHour) / 60}
}

// mailer is the sender for this request, nil when no relay is configured anywhere. It
// also moves the install-wide mail budget to the relay's hourly cap.
func (a *API) mailer(ctx context.Context) OTPMailer {
	if a.MailSettings == nil {
		return a.Mailer
	}
	r, err := a.MailSettings.resolve(ctx, a.now())
	if err != nil {
		log.Printf("api: mail settings unreadable, using felis.toml's relay: %v", err)
		return a.Mailer
	}
	relay, _ := a.MailSettings.effective(r)
	if relay == nil {
		return nil
	}
	a.mailGate().setLimit(MailRateLimit(relay.MaxPerHour))
	return a.MailSettings.NewMailer(*relay)
}

// mailSettingsView is GET /api/v1/settings/smtp. The password is never sent back:
// password_set says whether one is stored.
type mailSettingsView struct {
	Source      string `json:"source"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	From        string `json:"from"`
	Username    string `json:"username"`
	PasswordSet bool   `json:"password_set"`
	RequireTLS  bool   `json:"require_tls"`
	MaxPerHour  int    `json:"max_per_hour"`
	Revision    string `json:"revision"`
}

// mailSettingsRequest is a relay the panel submits. A nil Password keeps the password
// of the relay in force, so editing the sender does not mean retyping it.
type mailSettingsRequest struct {
	Host       string  `json:"host"`
	Port       int     `json:"port"`
	From       string  `json:"from"`
	Username   string  `json:"username"`
	Password   *string `json:"password"`
	RequireTLS bool    `json:"require_tls"`
	MaxPerHour int     `json:"max_per_hour"`
	Revision   string  `json:"revision"`
}

// revisionOf names a stored relay for compare-and-set in the panel. It hashes the parsed
// relay re-encoded, never the stored bytes: PostgreSQL hands jsonb back re-spaced and
// re-ordered, so the bytes a save wrote differ from what the next read returns.
func revisionOf(panel *mailsettings.Settings) string {
	if panel == nil {
		return ""
	}
	canonical, _ := json.Marshal(panel)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func (a *API) mailSettingsReady(w http.ResponseWriter, r *http.Request) bool {
	if a.MailSettings == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "mail_settings_unavailable", "mail settings are not available on this install"))
		return false
	}
	return true
}

func (a *API) mailSettingsView(res resolvedMail) mailSettingsView {
	relay, source := a.MailSettings.effective(res)
	view := mailSettingsView{Source: source, Revision: revisionOf(res.panel)}
	if relay != nil {
		view.Host, view.Port, view.From, view.Username = relay.Host, relay.Port, relay.From, relay.Username
		view.PasswordSet, view.RequireTLS, view.MaxPerHour = relay.Password != "", relay.RequireTLS, relay.MaxPerHour
	}
	return view
}

func (a *API) handleGetMailSettings(w http.ResponseWriter, r *http.Request) {
	if !a.mailSettingsReady(w, r) {
		return
	}
	a.MailSettings.invalidate()
	res, err := a.MailSettings.resolve(r.Context(), a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.mailSettingsView(res))
}

// submittedRelay builds the relay a save or a test names, carrying the current password
// over when the panel sent none.
func (a *API) submittedRelay(w http.ResponseWriter, r *http.Request) (mailsettings.Settings, resolvedMail, mailSettingsRequest, bool) {
	var body mailSettingsRequest
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return mailsettings.Settings{}, resolvedMail{}, body, false
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return mailsettings.Settings{}, resolvedMail{}, body, false
	}
	a.MailSettings.invalidate()
	res, err := a.MailSettings.resolve(r.Context(), a.now())
	if err != nil {
		writeError(w, r, err)
		return mailsettings.Settings{}, resolvedMail{}, body, false
	}
	s := mailsettings.Settings{
		Host: strings.TrimSpace(body.Host), Port: body.Port, From: strings.TrimSpace(body.From),
		Username: body.Username, RequireTLS: body.RequireTLS, MaxPerHour: body.MaxPerHour,
	}
	if body.Password != nil {
		s.Password = *body.Password
	} else if current, _ := a.MailSettings.effective(res); current != nil {
		s.Password = current.Password
	}
	if err := s.Validate(); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "%s", err))
		return mailsettings.Settings{}, resolvedMail{}, body, false
	}
	return s, res, body, true
}

// relayPinger is the part of the mail package's sender a relay check uses.
type relayPinger interface {
	Ping(ctx context.Context) error
}

// handleSetMailSettings saves a relay after proving it accepts mail from the sender. The
// check is not optional: email codes are how staff confirm sensitive changes and how
// op-login reaches anyone, so a relay that silently fails would lock people out.
func (a *API) handleSetMailSettings(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) || !a.mailSettingsReady(w, r) {
		return
	}
	s, res, body, ok := a.submittedRelay(w, r)
	if !ok {
		return
	}
	if body.Revision != revisionOf(res.panel) {
		writeError(w, r, newError(http.StatusConflict, "mail_settings_changed", "the mail settings changed; reload before saving"))
		return
	}
	pinger, ok := a.MailSettings.NewMailer(s).(relayPinger)
	if !ok {
		writeError(w, r, newError(http.StatusServiceUnavailable, "mail_settings_unavailable", "this install cannot check a relay"))
		return
	}
	if err := pinger.Ping(r.Context()); err != nil {
		writeError(w, r, relayTestError(err))
		return
	}
	value, _ := json.Marshal(s)
	err := a.MailSettings.Repo.CompareAndSetSetting(r.Context(), mailsettings.Key, res.raw, value)
	if errors.Is(err, ErrConflict) {
		writeError(w, r, newError(http.StatusConflict, "mail_settings_changed", "the mail settings changed; reload before saving"))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	a.MailSettings.invalidate()
	a.audit(r, "smtp.updated", s.Host)
	writeJSON(w, http.StatusOK, a.mailSettingsView(resolvedMail{raw: value, panel: &s}))
}

// handleClearMailSettings hands the relay back to felis.toml. The row is set to JSON null
// rather than deleted, so a concurrent save still meets compare-and-set.
func (a *API) handleClearMailSettings(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) || !a.mailSettingsReady(w, r) {
		return
	}
	a.MailSettings.invalidate()
	res, err := a.MailSettings.resolve(r.Context(), a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if res.panel == nil {
		writeJSON(w, http.StatusOK, a.mailSettingsView(res))
		return
	}
	if err := a.MailSettings.Repo.CompareAndSetSetting(r.Context(), mailsettings.Key, res.raw, []byte("null")); err != nil {
		if errors.Is(err, ErrConflict) {
			err = newError(http.StatusConflict, "mail_settings_changed", "the mail settings changed; reload before saving")
		}
		writeError(w, r, err)
		return
	}
	a.MailSettings.invalidate()
	a.audit(r, "smtp.cleared", "")
	writeJSON(w, http.StatusOK, a.mailSettingsView(resolvedMail{raw: []byte("null")}))
}

// handleTestMailSettings sends one test message through a relay that has not been saved,
// to the caller's verified address (else to the sender itself). It spends the install's
// mail budget like any other mail.
func (a *API) handleTestMailSettings(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !a.requireReauth(w, r, p) || !a.mailSettingsReady(w, r) {
		return
	}
	s, _, _, ok := a.submittedRelay(w, r)
	if !ok {
		return
	}
	if ok, wait := a.mailGate().take(mailGateKey); !ok {
		writeError(w, r, errMailRateLimited(wait))
		return
	}
	to := s.From
	if p != nil && p.EmailVerified {
		if u, err := a.Repo.UserByID(r.Context(), p.UserID); err == nil && u.Email != "" {
			to = u.Email
		}
	}
	sender := a.MailSettings.NewMailer(s)
	var err error
	if notice, ok := sender.(noticeSender); ok && to != s.From {
		err = notice.SendNotice(r.Context(), to, "Felis 邮件测试 / mail test",
			"这是一封测试邮件，说明 Felis 可以通过这个邮件服务器发信。\nThis test message shows Felis can send mail through this relay.")
	} else if pinger, ok := sender.(relayPinger); ok {
		err = pinger.Ping(r.Context())
	} else {
		err = errors.New("this install cannot send a test message")
	}
	if err != nil {
		writeError(w, r, relayTestError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent_to": to})
}

// relayTestError reports why a relay failed in a few words the panel can act on. The
// relay's own text stays in the API log: it can echo internal addresses back.
func relayTestError(err error) *apiError {
	msg := err.Error()
	log.Printf("api: relay check failed: %v", err)
	code, text := "smtp_failed", "the relay did not accept the test message"
	switch {
	case strings.Contains(msg, "refusing to dial"), strings.Contains(msg, "smtp: dial"):
		code, text = "smtp_unreachable", "the relay could not be reached at that host and port"
	case strings.Contains(msg, "STARTTLS"), strings.Contains(msg, "starttls"), strings.Contains(msg, "tls:"), strings.Contains(msg, "x509"):
		code, text = "smtp_tls", "the relay's TLS could not be established"
	case strings.Contains(msg, "smtp: auth"):
		code, text = "smtp_auth", "the relay refused the user name or password"
	case strings.Contains(msg, "MAIL FROM"), strings.Contains(msg, "relay refused mail from"):
		code, text = "smtp_sender_refused", "the relay refused to send from that address"
	case strings.Contains(msg, "RCPT TO"):
		code, text = "smtp_recipient_refused", "the relay refused the recipient"
	}
	return newError(http.StatusBadRequest, code, "%s", text)
}
