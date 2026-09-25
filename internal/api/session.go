package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// Local sessions (spec §B, passwordless). The remote face authenticates statelessly
// with a Cloudflare-Access JWT and sets no cookie; the passwordless console login
// (email-OTP / passkey / setup redeem), used on op.console when Zero Trust is not
// configured (and as the demo's primary web login), needs a server-minted session.
// We store only the sha-256 of the opaque cookie value, mirroring how service tokens
// are stored, so a database read never yields a usable cookie.

const (
	// sessionCookieName is the host-only session cookie. It carries no Domain
	// attribute, so an op.console session is never sent to the player console.
	sessionCookieName = "felis_session"
	// sessionTTL bounds a local session. Staff re-authenticate after it.
	sessionTTL = 12 * time.Hour
	// staffSessionIdle ends a staff session that has authenticated no request for
	// this long; a player session has only sessionTTL. Any authenticated request
	// counts, a panel tab's background refresh included, so what this ends is a
	// session left behind in a closed tab or on a machine that went to sleep.
	staffSessionIdle = 30 * time.Minute
	// sessionTouchEvery is how stale a session's last_seen_at may grow before a
	// request advances it: an active session costs one write a minute, not one per
	// request, and the idle limit is honored to within this.
	sessionTouchEvery = time.Minute
	// maxSessionUserAgent caps the User-Agent a session keeps to name its device.
	maxSessionUserAgent = 256
)

// LocalAuthEnabledKey is the platform_settings key that gates whether
// local sessions are honored. It is flipped on by `felis breakGlass`
// direct-to-Postgres at first-run and read live per-request, so enabling local
// auth needs no pod roll. Exported so the break-glass writer and this
// per-request reader share one source of truth instead of drifting copies.
const LocalAuthEnabledKey = "local_auth_enabled"

// newSessionToken returns a fresh opaque session value (256 bits, URL-safe). It
// is the cookie value; only its hash is persisted.
func newSessionToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// hashCookie maps a cookie value to its storage key (sha-256 hex), so the raw
// cookie is never written to the database.
func hashCookie(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// signInProof says whether the sign-in minting a session proved a factor of the
// account. A proven sign-in counts as a fresh reauth, so the new session may add
// a passkey or change the email straight away (requireReauth).
type signInProof bool

const (
	// provenSignIn: a passkey, an email code, op-login or the setup token.
	provenSignIn signInProof = true
	// bindCodeSignIn: the in-game identity alone, which never unlocks the
	// account's other factors.
	bindCodeSignIn signInProof = false
)

// startSession mints a session for userID and sets its cookie. Every sign-in door
// ends here, so every session records the device it was minted for.
func (a *API) startSession(w http.ResponseWriter, r *http.Request, userID string, proof signInProof) error {
	token, err := newSessionToken()
	if err != nil {
		return err
	}
	now := a.now()
	expires := now.Add(sessionTTL)
	ip := ""
	if addr := a.clientIP(r); addr.IsValid() {
		ip = addr.String()
	}
	var reauth time.Time
	if proof == provenSignIn {
		reauth = now
	}
	if err := a.Repo.CreateSession(r.Context(), NewSession{
		TokenHash: hashCookie(token),
		UserID:    userID,
		ExpiresAt: expires,
		UserAgent: truncateUTF8(r.UserAgent(), maxSessionUserAgent),
		ClientIP:  ip,
		ReauthAt:  reauth,
	}); err != nil {
		return err
	}
	setSessionCookie(w, token, expires)
	return nil
}

// currentSessionHash is the storage key of the session cookie r carries, or ""
// when it carries none.
func currentSessionHash(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return ""
	}
	return hashCookie(c.Value)
}

// setSessionCookie writes the session cookie: HttpOnly + Secure + SameSite=Lax,
// host-only (no Domain), rooted at "/". Secure means the console must be served
// over HTTPS — already a hard requirement, since WebAuthn and Zero Trust both
// demand a secure context.
func setSessionCookie(w http.ResponseWriter, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie expires the session cookie (logout). The attributes must
// match setSessionCookie for the browser to overwrite it.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// hostIsAdminConsole reports whether the request arrived on the configured
// operator console host. The session cookie is host-only, so a session minted on
// the admin host is structurally unable to reach the player console. If older
// configs omit [auth].admin_hostname, fall back to op.console.<root_domain>.
// Local bootstrap may also use the node's private/loopback IP directly when
// wildcard DNS is unavailable; that is treated as the local admin face.
func hostIsAdminConsole(r *http.Request, rootDomain, adminHostname string) bool {
	want := strings.TrimSpace(adminHostname)
	if want == "" {
		if rootDomain == "" {
			return false
		}
		want = "op.console." + rootDomain
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	return strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(want, "."))
}

// SessionAuth is the composite ExternalAuth for the web face. It prefers a
// local session cookie and otherwise delegates to the remote JWT
// verifier, so both auth models coexist on one face:
//
//   - No cookie  → delegate to Delegate (the Cloudflare-Access JWT path).
//   - Cookie set → local auth MUST be enabled (a missing or non-true
//     local_auth_enabled setting is treated as disabled — fail closed); the
//     session hash must resolve to a live user. On any failure the request is
//     rejected and does NOT fall through to the JWT delegate, so a stale or
//     forged cookie can never be laundered into a JWT attempt.
type SessionAuth struct {
	Repo          Repo
	Delegate      ExternalAuth
	RootDomain    string
	AdminHostname string
	Now           func() time.Time
}

func (s SessionAuth) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Authenticate resolves the caller from a session cookie or delegates to the JWT
// verifier (see the type comment for the fail-closed rules).
func (s SessionAuth) Authenticate(r *http.Request) (*Principal, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		// No usable session cookie: this is the remote JWT path.
		if s.Delegate == nil {
			return nil, fmt.Errorf("external auth not configured")
		}
		return s.Delegate.Authenticate(r)
	}

	ctx := r.Context()
	enabled, err := localAuthEnabledStatus(ctx, s.Repo)
	if err != nil {
		// The session store is unreachable: this is an outage, not a verdict on
		// the caller's credentials, so the middleware answers 503 rather than a
		// misleading "please log in".
		return nil, fmt.Errorf("%w: %v", errAuthBackend, err)
	}
	if !enabled {
		// A cookie was presented but local auth is off: reject, never fall through.
		return nil, fmt.Errorf("local auth disabled")
	}

	hash, now := hashCookie(cookie.Value), s.now()
	u, err := s.Repo.SessionUser(ctx, hash, now)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, fmt.Errorf("invalid session: %w", err)
	case err != nil:
		return nil, fmt.Errorf("%w: %v", errAuthBackend, err)
	}
	if now.Sub(u.LastSeenAt) >= sessionTouchEvery {
		// A failed touch costs at most an early idle sign-out, so the request goes on.
		if err := s.Repo.TouchSession(ctx, hash, now); err != nil {
			log.Printf("api: record session activity (request_id=%s): %v", requestIDFromContext(ctx), err)
		}
	}
	return &Principal{
		UserID:         u.ID,
		Username:       u.Username,
		Email:          u.Email,
		Role:           u.Role,
		ViaAdminAccess: staffRole(u.Role) && hostIsAdminConsole(r, s.RootDomain, s.AdminHostname),
		EmailVerified:  u.EmailVerified,
		ViaSession:     true,
		ReauthAt:       u.ReauthAt,
	}, nil
}

// errAuthBackend marks an authentication failure caused by the session store
// being unreachable (e.g. Postgres down) rather than by a missing or invalid
// credential. Middleware maps it to 503 so an outage is not misreported as 401.
var errAuthBackend = errors.New("auth backend unavailable")

// localAuthEnabled reports whether the runtime local_auth_enabled toggle is true.
// A missing setting, a read error, or a non-true value all read as disabled — the
// gate fails closed so local sessions are honored, and new ones minted, only on an
// explicit opt-in. Both SessionAuth (honoring a cookie) and the login handler
// (minting one) consult it, so the two never disagree about whether local auth is
// live.
func localAuthEnabled(ctx context.Context, repo Repo) bool {
	enabled, _ := localAuthEnabledStatus(ctx, repo)
	return enabled
}

// localAuthEnabledStatus is localAuthEnabled with the outage case kept apart: a
// MISSING setting (ErrNotFound — never enabled) reads as (false, nil), while a
// store read failure reads as (false, err) so SessionAuth can tell "local auth
// is off" (401) from "the database is down" (503). An unreadable value still
// fails closed as disabled — it is a config fault, not an outage.
func localAuthEnabledStatus(ctx context.Context, repo Repo) (bool, error) {
	raw, err := repo.GetSetting(ctx, LocalAuthEnabledKey)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		return false, nil
	}
	return enabled, nil
}

// ensure SessionAuth satisfies ExternalAuth at compile time.
var _ ExternalAuth = SessionAuth{}
